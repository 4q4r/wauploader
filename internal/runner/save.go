// save.go implements the save pipeline: targets → POST /save →
// /save/status polling → done/failed/quota_parked, journaled at every
// transition (docs/plan.md Task 10).
//
// Load-bearing decisions (documented for Task 12 wiring):
//
//   - Run carries an explicit force parameter: targets already done are
//     reported skipped unless force is true, in which case they are
//     reset and resubmitted. spn2.Options stays a pure wire struct.
//   - The completion func returns (Summary, error): the summary for
//     reporting, and a non-nil error only for fatal conditions —
//     limits misconfiguration, the auth pause (Review Focus #5), or a
//     cancelled context. Per-target failures are journaled, never fatal.
//   - Resubmits are bounded by the journaled Attempts counter: at most
//     maxSubmitAttempts POST /save calls per target; a retryable
//     failure sleeps briefly and resubmits, an auth refusal is not
//     charged (the record stays queued for the next run).
//   - user-session-limit is special-cased before quota parking: the
//     runner polls UserStatus and waits while processing >= available,
//     then retries, because the session frees as in-flight jobs finish;
//     the wait is expected behavior and does not consume the resubmit
//     budget.
//   - Job-state error documents from the status endpoint are classified
//     runner-side: the quota family parks, everything else fails as
//     terminal (the full wire taxonomy lives in spn2 for Submit
//     envelope refusals, which carry a typed spn2.Error).
//   - Resume prepends journal records in queued|submitted|pending;
//     records with a job id skip straight to polling. quota_parked
//     records are NOT auto-resumed: requeueing them is a command-layer
//     decision (T12), because the daily window may not have reset.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/journal"
	"github.com/4q4r/wauploader/internal/spn2"
)

// Event kinds on the event stream. EventState marks routine journal
// transitions, EventWait marks informational pauses (server-stated
// delays, session-limit backoff), EventAuth marks the single
// auth-pause notice.
const (
	EventState = "state"
	EventWait  = "wait"
	EventAuth  = "auth"
)

// Event is one save-pipeline event for the console and --json
// channels. State carries the journal state after the transition;
// ErrMsg carries the failure text on error transitions and the
// informational note on EventWait; Attempt is the number of Submit
// calls consumed so far for the target.
type Event struct {
	Kind        string
	Target      string
	State       journal.State
	JobID       string
	SnapshotURL string
	ErrMsg      string
	Attempt     int

	// Upload progress (EventProgress events, upload.go): Sent bytes of
	// Total sent in the current PUT attempt; always zero on save
	// events. Additive fields for the shared event stream.
	Sent  int64
	Total int64
}

// Summary is the batch outcome reported by the completion func.
type Summary struct {
	Done    int
	Failed  int
	Parked  int
	Skipped int
}

// SaveRunner drives the save pipeline over one spn2 client and one
// journal. Construct with NewSaveRunner; Run is safe to call once per
// instance at a time (the journal store itself is concurrency-safe).
type SaveRunner struct {
	sp    *spn2.Client
	st    *journal.Store
	lim   Limits
	con   *console.Console
	gate  *rate.Limiter
	hosts *hostLimiters
}

// NewSaveRunner wires the client, journal, limits and console into a
// runner. Zero poll fields apply the documented 2s→30s ladder; a nil
// console discards human output.
func NewSaveRunner(sp *spn2.Client, st *journal.Store, lim Limits, con *console.Console) *SaveRunner {
	lim = lim.withDefaults()
	if con == nil {
		con = console.New(io.Discard, io.Discard, false, true)
	}
	return &SaveRunner{
		sp:    sp,
		st:    st,
		lim:   lim,
		con:   con,
		gate:  rate.NewLimiter(lim.PerMinute, 1),
		hosts: newHostLimiters(lim.PerHost),
	}
}

// runResult carries the completion answers from the internal goroutine
// to the caller.
type runResult struct {
	done    chan struct{}
	summary Summary
	err     error
}

// Run starts the batch over targets and returns the event stream plus
// a completion func. The stream closes when the batch ends; the
// completion func blocks until then and returns the Summary and a
// non-nil error on fatal conditions (limits misconfiguration, auth
// pause, cancelled context). The caller must drain the stream fully.
//
// Resume: journal records in queued|submitted|pending are prepended;
// records with a live job id skip straight to polling; targets already
// done are reported skipped unless force. Explicitly re-listed failed
// or parked targets always run again (explicit user intent).
func (r *SaveRunner) Run(ctx context.Context, targets []string, opts spn2.Options, force bool) (<-chan Event, func() (Summary, error)) {
	events := make(chan Event)
	res := &runResult{done: make(chan struct{})}
	go func() {
		defer close(events)
		res.summary, res.err = r.execute(ctx, targets, opts, force, events)
		close(res.done)
	}()
	return events, func() (Summary, error) {
		<-res.done
		return res.summary, res.err
	}
}

// counters aggregates the summary under a mutex; workers update it
// from several goroutines.
type counters struct {
	mu      sync.Mutex
	done    int
	failed  int
	parked  int
	skipped int
}

func (c *counters) incDone()    { c.mu.Lock(); c.done++; c.mu.Unlock() }
func (c *counters) incFailed()  { c.mu.Lock(); c.failed++; c.mu.Unlock() }
func (c *counters) incParked()  { c.mu.Lock(); c.parked++; c.mu.Unlock() }
func (c *counters) incSkipped() { c.mu.Lock(); c.skipped++; c.mu.Unlock() }

func (c *counters) summary() Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Summary{Done: c.done, Failed: c.failed, Parked: c.parked, Skipped: c.skipped}
}

// workItem is one unit of save work: the target plus its journal
// record (always present — buildQueue creates queued records up front
// so every listed target is visible in the journal and resumable even
// if the run stops before admitting it).
type workItem struct {
	rec *journal.Record
}

// execute validates limits, builds the queue and runs the worker pool.
func (r *SaveRunner) execute(ctx context.Context, targets []string, opts spn2.Options, force bool, events chan<- Event) (Summary, error) {
	if err := r.lim.Validate(); err != nil {
		return Summary{}, err
	}
	c := &counters{}
	items, err := r.buildQueue(targets, force, events, c)
	if err != nil {
		return c.summary(), err
	}

	var paused atomic.Bool
	var authOnce sync.Once
	var authErr error

	// The errgroup's limit is the global concurrency semaphore: at
	// most Concurrency items are in flight (submit + polling). Workers
	// never return errors — per-target failures are journaled — so the
	// group error is always nil and cancellation travels through gctx.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.lim.Concurrency)
	for _, it := range items {
		if paused.Load() || gctx.Err() != nil {
			break // stop admitting; in-flight jobs finish
		}
		g.Go(func() error {
			r.process(gctx, it, opts, events, c, &paused, &authOnce, &authErr)
			return nil
		})
	}
	_ = g.Wait()

	if authErr != nil {
		return c.summary(), authErr
	}
	return c.summary(), ctx.Err()
}

// buildQueue assembles the work list: journal records in
// queued|submitted|pending prepend (resume), then fresh or re-run
// targets. Already-done targets are skipped unless force. Every listed
// target ends up with a queued journal record.
func (r *SaveRunner) buildQueue(targets []string, force bool, events chan<- Event, c *counters) ([]workItem, error) {
	items := []workItem{}
	seen := map[string]bool{}

	resume, err := r.st.ByStates([]journal.Kind{journal.KindSave}, []journal.State{
		journal.StateQueued, journal.StateSubmitted, journal.StatePending,
	})
	if err != nil {
		r.con.Errorf("Не удалось прочитать журнал для возобновления: %v", err)
		return nil, fmt.Errorf("чтение журнала не выполнено: %w", err)
	}
	for i := range resume {
		rec := resume[i] // ByStates returns copies
		items = append(items, workItem{rec: &rec})
		seen[rec.Target] = true
	}

	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		rec, ok, err := r.st.Get(journal.KindSave, t)
		if err != nil {
			r.con.Errorf("Не удалось прочитать запись журнала для %s: %v", t, err)
			return nil, fmt.Errorf("чтение журнала не выполнено: %w", err)
		}
		if ok && rec.State == journal.StateDone && !force {
			r.emit(events, Event{Kind: EventState, Target: t, State: journal.StateSkipped})
			r.con.Dimf("Пропущено (уже сохранено): %s", t)
			c.incSkipped()
			continue
		}
		fresh := &journal.Record{Kind: journal.KindSave, Target: t, State: journal.StateQueued}
		if ok {
			// done+force, failed, quota_parked or skipped: an explicit
			// re-run resets the work fields but keeps the record.
			if err := r.st.UpdateState(journal.KindSave, t, journal.StateQueued, resetRecord); err != nil {
				r.con.Errorf("Не удалось сбросить запись журнала для %s: %v", t, err)
				return nil, fmt.Errorf("сброс записи журнала не выполнен: %w", err)
			}
		} else if err := r.st.Put(fresh); err != nil {
			r.con.Errorf("Не удалось создать запись журнала для %s: %v", t, err)
			return nil, fmt.Errorf("создание записи журнала не выполнено: %w", err)
		}
		items = append(items, workItem{rec: fresh})
	}
	return items, nil
}

// resetRecord clears the work fields for an explicit re-run.
func resetRecord(m *journal.Record) {
	m.Attempts = 0
	m.SPN2JobID = ""
	m.SnapshotURL = ""
	m.SnapshotTimestamp = ""
	m.ErrKind = ""
	m.ErrMsg = ""
}

// pollState carries per-item polling state that never belongs in the
// journal: the download-size plateau tracker and the backoff step.
type pollState struct {
	last int64 // last nonzero download_size reading
	run  int   // consecutive identical nonzero readings
	poll int   // completed backoff steps
}

// trackSize records one download_size reading and reports a stall:
// three consecutive identical nonzero values.
func (ps *pollState) trackSize(size int64) bool {
	if size <= 0 {
		ps.last, ps.run = 0, 0
		return false
	}
	if size == ps.last {
		ps.run++
	} else {
		ps.last, ps.run = size, 1
	}
	return ps.run >= 3
}

// backoff returns the next poll wait: PollStart doubled each step up
// to PollMax.
func (ps *pollState) backoff(start, max time.Duration) time.Duration {
	d := start
	for i := 0; i < ps.poll && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	ps.poll++
	return d
}

// process runs one item through the state machine under the per-host
// politeness slot and the per-job time budget.
func (r *SaveRunner) process(ctx context.Context, it workItem, opts spn2.Options, events chan<- Event, c *counters, paused *atomic.Bool, authOnce *sync.Once, authErr *error) {
	// Admission double-check: the feeder tested the flag before g.Go,
	// but this goroutine may start after an auth pause landed (the
	// global cap can queue the start). A paused item stays queued.
	if paused.Load() {
		return
	}
	release, err := r.hosts.acquire(ctx, hostOf(it.rec.Target))
	if err != nil {
		return // run cancelled while queued for a host slot
	}
	defer release()

	jctx, cancel := context.WithTimeout(ctx, r.lim.JobWait)
	defer cancel()

	rec := it.rec
	r.emit(events, Event{Kind: EventState, Target: rec.Target, State: rec.State, JobID: rec.SPN2JobID, Attempt: rec.Attempts})

	ps := &pollState{}
	for {
		if r.submitStage(jctx, rec, opts, events, c, paused, authOnce, authErr) {
			r.finishOnStop(ctx, jctx, rec, events, c)
			return
		}
		if r.pollStage(jctx, rec, ps, events, c, paused, authOnce, authErr) {
			r.finishOnStop(ctx, jctx, rec, events, c)
			return
		}
	}
}

// finishOnStop closes out a stopped item: only a budget expiry with a
// still-in-flight record becomes a journaled timeout failure; every
// other stop (auth pause, parent cancel, already-terminal record)
// leaves the record as the stage left it.
func (r *SaveRunner) finishOnStop(ctx, jctx context.Context, rec *journal.Record, events chan<- Event, c *counters) {
	inFlight := rec.State == journal.StateQueued || rec.State == journal.StateSubmitted || rec.State == journal.StatePending
	if inFlight && ctx.Err() == nil && jctx.Err() == context.DeadlineExceeded {
		rec.ErrKind = "timeout"
		rec.ErrMsg = fmt.Sprintf("превышено время ожидания задания (job_wait %s)", r.lim.JobWait)
		r.finishFailed(events, c, rec)
	}
}

// submitStage submits one capture job. It returns true when the item
// is finished or the run paused; false means "advance the state loop".
func (r *SaveRunner) submitStage(ctx context.Context, rec *journal.Record, opts spn2.Options, events chan<- Event, c *counters, paused *atomic.Bool, authOnce *sync.Once, authErr *error) bool {
	if rec.SPN2JobID != "" {
		return false // resumed with a job id: go straight to polling
	}
	if rec.Attempts >= maxSubmitAttempts {
		rec.ErrKind = "attempts"
		rec.ErrMsg = fmt.Sprintf("исчерпаны попытки отправки (%d)", maxSubmitAttempts)
		r.finishFailed(events, c, rec)
		return true
	}
	if paused.Load() {
		// An auth pause landed while this worker waited in the resubmit
		// loop: stay queued instead of submitting behind a paused queue.
		return true
	}
	// Global pacing gate for POST /save admissions (burst 1).
	if err := r.gate.Wait(ctx); err != nil {
		return true // cancelled while pacing; the record stays queued
	}
	res, err := r.sp.Submit(ctx, rec.Target, opts)
	if err != nil {
		if isAuthFailure(err) {
			// Not charged: the refusal is about credentials, not the
			// target; the record stays queued for the next run.
			r.authPause(events, paused, authOnce, authErr, err)
			return true
		}
		if isSessionLimit(err) {
			// Session-busy refusal: wait for the quota to free and
			// resubmit. The wait is expected behavior, not a failed
			// attempt, so no attempt is charged and the record stays
			// queued throughout.
			if !r.waitSession(ctx, rec, events, paused, authOnce, authErr) {
				return true
			}
			return false
		}
		rec.Attempts++
		rec.ErrKind = errKindName(err)
		rec.ErrMsg = err.Error()
		if serr := r.persist(rec, rec.State); serr != nil {
			r.journalError(events, c, rec, serr)
			return true
		}
		switch classifySPN2(err) {
		case spn2.KindQuota:
			rec.ErrKind = "quota"
			rec.ErrMsg = parkMessage(err.Error())
			r.park(events, c, rec)
			return true
		case spn2.KindTerminal:
			r.finishFailed(events, c, rec)
			return true
		default:
			// Retryable: resubmit after a short backoff; the
			// exhaustion check at the top bounds the ladder.
			if !sleepCtx(ctx, r.lim.PollStart) {
				return true
			}
			return false
		}
	}

	if res.Deduped {
		// A recent capture already exists: done with the existing
		// snapshot, no job to poll.
		r.finishDedup(events, c, rec, res.Message)
		return true
	}

	// Persist the job id immediately — status retention is 1h.
	rec.SPN2JobID = res.JobID
	rec.ErrKind, rec.ErrMsg = "", ""
	if err := r.persist(rec, journal.StateSubmitted); err != nil {
		r.journalError(events, c, rec, err)
		return true
	}
	attempt := rec.Attempts + 1
	r.emit(events, Event{Kind: EventState, Target: rec.Target, State: journal.StateSubmitted, JobID: res.JobID, Attempt: attempt})
	r.con.Infof("Отправлено: %s (задание %s, попытка %d)", rec.Target, res.JobID, attempt)

	if d, ok := spn2.ParseDelayMessage(res.Message); ok {
		r.waitNote(events, rec, fmt.Sprintf("сервер указал задержку старта захвата: %s", d))
		if !sleepCtx(ctx, d) {
			return true
		}
	}
	return false
}

// pollStage reads the job status once and advances the record. It
// returns true when finished or paused; false keeps polling.
func (r *SaveRunner) pollStage(ctx context.Context, rec *journal.Record, ps *pollState, events chan<- Event, c *counters, paused *atomic.Bool, authOnce *sync.Once, authErr *error) bool {
	st, err := r.sp.Status(ctx, rec.SPN2JobID)
	if err != nil {
		if isAuthFailure(err) {
			// The submitted job survives server-side; leave the record
			// submitted so the next run's resume polls it (status
			// retention is 1h).
			r.authPause(events, paused, authOnce, authErr, err)
			return true
		}
		if classifySPN2(err) == spn2.KindTerminal {
			rec.ErrKind = errKindName(err)
			rec.ErrMsg = err.Error()
			r.finishFailed(events, c, rec)
			return true
		}
		// Retryable (502 storms, transport hiccups): keep polling; the
		// job budget bounds the loop.
		if !sleepCtx(ctx, ps.backoff(r.lim.PollStart, r.lim.PollMax)) {
			return true
		}
		return false
	}

	switch st.State {
	case "success":
		orig := st.OriginalURL
		if orig == "" {
			orig = rec.Target
		}
		rec.SnapshotTimestamp = st.Timestamp
		rec.SnapshotURL = fmt.Sprintf(spn2.SnapshotURLFmt, st.Timestamp, orig)
		rec.ErrKind, rec.ErrMsg = "", ""
		if err := r.persist(rec, journal.StateDone); err != nil {
			r.journalError(events, c, rec, err)
			return true
		}
		c.incDone()
		r.emit(events, Event{Kind: EventState, Target: rec.Target, State: journal.StateDone, JobID: rec.SPN2JobID, SnapshotURL: rec.SnapshotURL, Attempt: rec.Attempts})
		r.con.Successf("Сохранено: %s → %s", rec.Target, rec.SnapshotURL)
		return true

	case "error":
		return r.statusError(ctx, st, rec, events, c, paused, authOnce, authErr)

	default: // pending (and anything unrecognized): keep polling
		if d, ok := spn2.ParseDelayMessage(st.Message); ok {
			// Sleep the server-stated delay instead of the blind ladder.
			r.waitNote(events, rec, fmt.Sprintf("сервер указал задержку захвата: %s", d))
			if !sleepCtx(ctx, d) {
				return true
			}
			return false
		}
		if ps.trackSize(st.DownloadSize) {
			rec.ErrKind = "stall"
			rec.ErrMsg = fmt.Sprintf("загрузка остановилась: download_size не растёт (%d байт на трёх опросах подряд)", st.DownloadSize)
			r.finishFailed(events, c, rec)
			return true
		}
		if !sleepCtx(ctx, ps.backoff(r.lim.PollStart, r.lim.PollMax)) {
			return true
		}
		return false
	}
}

// statusQuotaCodes is the runner-side copy of the SPN2 quota family
// for JOB-STATE error documents. Submit refusals — the main quota
// entry — are classified by spn2's full taxonomy via spn2.Error.Kind;
// this map covers only the rare case of a capture dying of quota
// mid-run, where the runner sees a raw status doc.
var statusQuotaCodes = map[string]bool{
	"too-many-daily-captures":     true,
	"max-daily-bandwidth":         true,
	"max-daily-bandwidth-from-ip": true,
	"max-daily-bandwidth-host":    true,
}

// statusError handles a job SPN2 reports in the error state: the
// session-limit case waits and requeues for a fresh submit, quota
// parks, everything else is a terminal failure (the job has ended;
// resubmitting would burn the 5-per-day-per-URL cap).
func (r *SaveRunner) statusError(ctx context.Context, st spn2.Status, rec *journal.Record, events chan<- Event, c *counters, paused *atomic.Bool, authOnce *sync.Once, authErr *error) bool {
	code := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(st.StatusExt)), "error:")
	switch {
	case code == "user-session-limit":
		if !r.waitSession(ctx, rec, events, paused, authOnce, authErr) {
			return true
		}
		// The errored job is dead; requeue for a fresh submit.
		rec.SPN2JobID = ""
		rec.ErrKind, rec.ErrMsg = "", ""
		if err := r.persist(rec, journal.StateQueued); err != nil {
			r.journalError(events, c, rec, err)
			return true
		}
		return false
	case statusQuotaCodes[code]:
		rec.ErrKind = "quota"
		rec.ErrMsg = parkMessage(strings.Join(nonEmpty([]string{st.StatusExt, st.Message}), ": "))
		r.park(events, c, rec)
		return true
	default:
		rec.ErrKind = "terminal"
		rec.ErrMsg = strings.Join(nonEmpty([]string{"задание завершилось ошибкой: " + st.StatusExt, st.Message}), ": ")
		r.finishFailed(events, c, rec)
		return true
	}
}

// waitSession backs off while SPN2 reports the session busy
// (processing >= available). It returns true when the session freed
// and the caller may retry; false means the run stopped (auth pause
// or cancelled context).
func (r *SaveRunner) waitSession(ctx context.Context, rec *journal.Record, events chan<- Event, paused *atomic.Bool, authOnce *sync.Once, authErr *error) bool {
	noted := false
	for {
		if ctx.Err() != nil {
			return false
		}
		avail, proc, err := r.sp.UserStatus(ctx)
		if err != nil {
			if isAuthFailure(err) {
				r.authPause(events, paused, authOnce, authErr, err)
				return false
			}
			if !sleepCtx(ctx, r.lim.PollStart) {
				return false
			}
			continue
		}
		if proc < avail {
			return true
		}
		if !noted {
			r.waitNote(events, rec, fmt.Sprintf("лимит сеансов исчерпан (занято %d из %d) — ожидание освобождения", proc, avail))
			noted = true
		}
		if !sleepCtx(ctx, r.lim.PollStart) {
			return false
		}
	}
}

// persist mirrors the runner-owned fields into the journal under the
// given state; on success the local record's state follows.
func (r *SaveRunner) persist(rec *journal.Record, st journal.State) error {
	err := r.st.UpdateState(journal.KindSave, rec.Target, st, func(m *journal.Record) {
		m.Attempts = rec.Attempts
		m.SPN2JobID = rec.SPN2JobID
		m.SnapshotURL = rec.SnapshotURL
		m.SnapshotTimestamp = rec.SnapshotTimestamp
		m.ErrKind = rec.ErrKind
		m.ErrMsg = rec.ErrMsg
	})
	if err == nil {
		rec.State = st
	}
	return err
}

// finishDedup journals the documented dedup outcome: a recent capture
// already exists and the server message references it.
func (r *SaveRunner) finishDedup(events chan<- Event, c *counters, rec *journal.Record, message string) {
	if ts, ok := extractTimestamp(message); ok {
		rec.SnapshotTimestamp = ts
		rec.SnapshotURL = fmt.Sprintf(spn2.SnapshotURLFmt, ts, rec.Target)
	}
	if err := r.persist(rec, journal.StateDone); err != nil {
		r.journalError(events, c, rec, err)
		return
	}
	c.incDone()
	r.emit(events, Event{Kind: EventState, Target: rec.Target, State: journal.StateDone, SnapshotURL: rec.SnapshotURL, ErrMsg: message, Attempt: rec.Attempts})
	r.con.Successf("Уже сохранено: %s → %s", rec.Target, rec.SnapshotURL)
}

// finishFailed journals a failed record and emits the failure.
func (r *SaveRunner) finishFailed(events chan<- Event, c *counters, rec *journal.Record) {
	if err := r.persist(rec, journal.StateFailed); err != nil {
		r.journalError(events, c, rec, err)
		return
	}
	c.incFailed()
	r.emit(events, Event{Kind: EventState, Target: rec.Target, State: journal.StateFailed, JobID: rec.SPN2JobID, ErrMsg: rec.ErrMsg, Attempt: rec.Attempts})
	r.con.Errorf("Не сохранено: %s: %s", rec.Target, rec.ErrMsg)
}

// park journals a quota-parked record and emits the midnight hint.
func (r *SaveRunner) park(events chan<- Event, c *counters, rec *journal.Record) {
	if err := r.persist(rec, journal.StateQuotaParked); err != nil {
		r.journalError(events, c, rec, err)
		return
	}
	c.incParked()
	r.emit(events, Event{Kind: EventState, Target: rec.Target, State: journal.StateQuotaParked, ErrMsg: rec.ErrMsg, Attempt: rec.Attempts})
	r.con.Warnf("Квота исчерпана, отложено: %s", rec.Target)
}

// journalError reports a journal write failure: the transition cannot
// be guaranteed, so the item counts as failed and the error is loud.
func (r *SaveRunner) journalError(events chan<- Event, c *counters, rec *journal.Record, err error) {
	c.incFailed()
	msg := fmt.Sprintf("ошибка записи журнала: %v", err)
	r.emit(events, Event{Kind: EventState, Target: rec.Target, State: rec.State, ErrMsg: msg, Attempt: rec.Attempts})
	r.con.Errorf("%s: %v", rec.Target, err)
}

// authPause fires once per run: it stops admitting new jobs and emits
// one clear Russian event (Review Focus #5). In-flight jobs keep
// running to their journaled ends.
func (r *SaveRunner) authPause(events chan<- Event, paused *atomic.Bool, authOnce *sync.Once, authErr *error, cause error) {
	authOnce.Do(func() {
		paused.Store(true)
		*authErr = fmt.Errorf("ошибка авторизации archive.org (%v): выполнение очереди приостановлено — проверьте ключи или куки в конфигурации и запустите снова", cause)
		r.emit(events, Event{Kind: EventAuth, ErrMsg: (*authErr).Error()})
		r.con.Errorf("%v", *authErr)
	})
}

// waitNote emits an informational wait event (EventWait).
func (r *SaveRunner) waitNote(events chan<- Event, rec *journal.Record, note string) {
	r.emit(events, Event{Kind: EventWait, Target: rec.Target, State: rec.State, JobID: rec.SPN2JobID, ErrMsg: note, Attempt: rec.Attempts})
	r.con.Infof("%s: %s", rec.Target, note)
}

// emit sends one event downstream. The consumer must drain the stream
// until close — the contract of Run.
func (r *SaveRunner) emit(events chan<- Event, ev Event) {
	events <- ev
}

// parkMessage renders the Russian park message: the cause plus the
// daily-limit reset hint (local midnight).
func parkMessage(cause string) string {
	return fmt.Sprintf("%s. Цель отложена до следующего запуска — дневной лимит archive.org обновляется около полуночи (через %s)",
		cause, untilMidnight().Round(time.Minute))
}

// snapshotTSRe isolates the 14-digit timestamp of an existing capture
// referenced in a dedup message.
var snapshotTSRe = regexp.MustCompile(`web\.archive\.org/web/(\d{14})/`)

// extractTimestamp pulls the existing capture's timestamp out of a
// dedup message; the second result is false when there is none.
func extractTimestamp(message string) (string, bool) {
	m := snapshotTSRe.FindStringSubmatch(message)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// asSPN2Error unwraps a typed spn2 failure when present.
func asSPN2Error(err error) (*spn2.Error, bool) {
	var se *spn2.Error
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// isAuthFailure reports SPN2 auth loss: HTTP 401 or an unauthorized
// status_ext from any call (Review Focus #5).
func isAuthFailure(err error) bool {
	se, ok := asSPN2Error(err)
	if !ok {
		return false
	}
	if se.HTTP == http.StatusUnauthorized {
		return true
	}
	return strings.Contains(strings.ToLower(se.StatusExt), "unauthorized")
}

// isSessionLimit reports the session-busy refusal that the runner
// waits out instead of parking.
func isSessionLimit(err error) bool {
	se, ok := asSPN2Error(err)
	return ok && strings.Contains(strings.ToLower(se.StatusExt), "user-session-limit")
}

// classifySPN2 maps an error to its spn2 kind; untyped errors are
// fail-open retryable.
func classifySPN2(err error) spn2.Kind {
	if se, ok := asSPN2Error(err); ok {
		return se.Kind
	}
	return spn2.KindRetryable
}

// errKindName renders the journal ErrKind for a classified error.
func errKindName(err error) string {
	switch classifySPN2(err) {
	case spn2.KindQuota:
		return "quota"
	case spn2.KindTerminal:
		return "terminal"
	default:
		return "retryable"
	}
}

// nonEmpty drops empty strings from a slice, preserving order.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
