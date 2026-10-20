// upload.go implements the upload pipeline (docs/plan.md Task 11):
// targets → hash once → skip-if-exists (guarded by active item tasks) →
// check_limit preflight → derive baseline → PUT → optional verify →
// per-item derive watch, journaled at every transition.
//
// Load-bearing decisions (documented for Task 12 wiring):
//
//   - Run mirrors SaveRunner's completion contract: the returned func
//     blocks until the batch ends and returns (Summary, error) — the
//     summary for the «Итог» line, a non-nil error only for fatal
//     conditions (limits misconfiguration, auth pause, cancelled
//     context). Per-file failures are journaled, never fatal.
//   - Journal target = abs path + "\x00" + identifier + "/" + key
//     (journal.KeyFor("upload", ...) derives the bucket key). Event
//     .Target carries the same composite; console lines print the
//     human path → identifier/key form instead.
//   - Hashing is a single streaming pass (io.CopyBuffer into
//     io.MultiWriter over MD5 and SHA-256 — the chunked TeeReader
//     pattern generalized to both hashers, no full-file buffering).
//     The digests persist on the queued record, so a resumed run never
//     re-hashes a file. A local file change between hash and PUT is
//     rejected by ias3.Upload's size check (fail, not overwrite).
//   - Skip guard (reference-client semantics, owner ruling 2026-10-19):
//     a matching remote md5 justifies a skip only while the item has NO
//     active catalog tasks (any command — derive and ingest alike;
//     ias3.HasActiveTasks). During derive/ingest the metadata may be
//     stale, so an active item re-uploads; an erroring Tasks API fails
//     the file retryable (skip-guard) instead of skipping on doubt.
//   - Journal state usage for uploads: queued (created, hash persisted
//     while queued), skipped (proven remote duplicate or already done
//     in the journal), done, failed. submitted/pending/quota_parked do
//     not apply: a PUT is synchronous, has no server job id, and the
//     IA-S3 SlowDown retry budget lives inside ias3.Upload. Resume
//     scans queued records; re-listed done files are reported skipped
//     (this contract has no force flag); failed/skipped files re-run on
//     explicit relist.
//   - Ordering: strict sequence WITHIN one identifier (the pre-PUT
//     derive baseline capture and the size-hint/meta headers depend on
//     it), parallelism only ACROSS identifiers, bounded by
//     Limits.Concurrency. Limits.PerHost does not apply — the upload
//     host (s3.us.archive.org) is constant, so there is nothing to
//     fan out per host; the field stays validated for the shared
//     Limits contract. Limits.PerMinute gates PUT admissions (no-op at
//     rate.Inf — the IA-S3 politeness surface is the check_limit
//     preflight, there is no documented per-minute upload cap).
//   - Item bootstrap: the first PUT of an item carries
//     AutoMakeBucket + SizeHint = the summed size of every file bound
//     for that item in this run; QueueDerive is true only on the last
//     file. Collection/mediatype/meta/keep-old-version and the rest of
//     the caller's HeaderOpts ride every PUT (reference-client
//     behavior; metadata is idempotent). A skip-existing hit may make
//     the SizeHint a slight overcount — it is a scheduling hint, not a
//     contract.
//   - Derive watch (--wait-derive) is PER ITEM and gated by the
//     waitDerive argument: without it the tool never waits — the
//     derive still lands on the IA queue via QueueDerive and the run
//     ends as soon as the files are done. Even with the flag the watch
//     fires only when a PUT actually carrying QueueDerive=true
//     succeeded ("armed"): if the last file failed or skip-matched, no
//     derive was queued and waiting would burn the JobWait budget into
//     a false item failure. The baseline task id is captured lazily,
//     right before the item's first actual PUT (items whose files all
//     skip never touch the Tasks API). After the last file, WaitDerive
//     runs with Limits.PollStart as the poll cadence and Limits.JobWait
//     as the budget; a derive failure is an item-level failed event
//     while the files stay done, and the summary counts it as one extra
//     Failed (mixed granularity by design).
//   - Verify (--verify): after a successful PUT the Item Metadata API
//     must show the file with the local MD5. A missing file or an
//     entry without a digest right after the PUT is ingest lag — retried
//     up to verifyAttempts with short sleeps; a definite digest
//     mismatch fails immediately. Verify failure = failed record; the
//     next run re-uploads (idempotent overwrite) and re-verifies. A
//     cancelled context is not a verify failure: like every other
//     interrupt, it leaves the record queued (M-2 ruling).
//   - Interrupts are resumable: every ctx-aware stage (skip-check,
//     skip-guard, preflight, pacing gate, PUT, verify, derive baseline)
//     treats ctx.Err() != nil as a stop — the record stays queued with
//     its persisted digests and the next run's resume scan continues it
//     (mirroring SaveRunner's semantics). Only a real stage failure is
//     journaled failed.
//   - Auth: ias3.ErrBadCredentials journals the file failed (unlike the
//     save runner, which leaves its record queued — the brief pins the
//     upload behavior), fires one Russian auth event, stops admitting
//     new uploads and surfaces as the completion error.

package runner

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/ias3"
	"github.com/4q4r/wauploader/internal/journal"
)

// EventProgress marks a per-file PUT progress event: Event.Sent bytes
// of Event.Total sent in the current PUT attempt.
const EventProgress = "progress"

// uploadTargetSep joins the local path and the item/key pair into the
// composite journal target. A NUL cannot appear in a filesystem path,
// so the composite is unambiguous.
const uploadTargetSep = "\x00"

// Progress throttle: an event fires when the byte watermark advanced by
// the step — 2% of the file size or progressMinStep, whichever is
// coarser (fewer events). progressMinStep is a package var so tests can
// shrink it (tests run sequentially).
const progressFraction = 0.02

var progressMinStep int64 = 64 << 20

// verifyAttempts bounds the post-PUT metadata check: the freshly
// uploaded file becomes visible in the Item Metadata API only after the
// ingest task runs, so a missing file is expected lag, retried briefly
// before being reported.
const verifyAttempts = 4

// progressThrottle is the per-PUT progress watermark. emit reports
// whether a progress event should be emitted for the cumulative byte
// count sent and updates the watermark; a byte regression means ias3
// restarted the PUT from byte 0 (no resume exists), which resets the
// watermark without emitting.
type progressThrottle struct {
	total int64
	last  int64
}

func (p *progressThrottle) step() int64 {
	s := int64(float64(p.total) * progressFraction)
	if s < progressMinStep {
		s = progressMinStep
	}

	return s
}

func (p *progressThrottle) emit(sent int64) bool {
	if sent < p.last {
		p.last = 0
		return false
	}

	if sent-p.last >= p.step() {
		p.last = sent
		return true
	}

	return false
}

// UploadTarget is one file bound for one IA item. Key defaults to the
// file's base name; Identifier is required.
//
// Per-target item metadata (--auto-meta, Task 14): a non-empty Title or
// Description overrides the batch HeaderOpts value on THIS file's PUTs,
// and Extra values append after the batch Extra values for the same
// names (batch first, derived second). Zero-value fields change nothing.
type UploadTarget struct {
	Path        string
	Identifier  string
	Key         string
	Title       string
	Description string
	Extra       ias3.Meta
}

// UploadRunner drives the upload pipeline over one ias3 client and one
// journal. Construct with NewUploadRunner; Run is safe to call once per
// instance at a time (the journal store itself is concurrency-safe).
type UploadRunner struct {
	cl   *ias3.Client
	st   *journal.Store
	lim  Limits
	con  *console.Console
	gate *rate.Limiter
}

// NewUploadRunner wires the client, journal, limits and console into a
// runner. Zero poll fields apply the documented defaults; a nil console
// discards human output. PerHost is validated but not applied (the
// upload host is constant — see the package doc above).
func NewUploadRunner(cl *ias3.Client, st *journal.Store, lim Limits, con *console.Console) *UploadRunner {
	lim = lim.withDefaults()

	if con == nil {
		con = console.New(io.Discard, io.Discard, false, true)
	}

	return &UploadRunner{
		cl:   cl,
		st:   st,
		lim:  lim,
		con:  con,
		gate: rate.NewLimiter(lim.PerMinute, 1),
	}
}

// Run starts the batch over targets and returns the event stream plus a
// completion func returning (Summary, error) — mirroring SaveRunner.
// The stream closes when the batch ends; the caller must drain it fully.
func (r *UploadRunner) Run(ctx context.Context, targets []UploadTarget, o ias3.HeaderOpts, verify, skipExisting, waitDerive bool) (<-chan Event, func() (Summary, error)) {
	events := make(chan Event)
	res := &runResult{done: make(chan struct{})}

	go func() {
		defer close(events)

		res.summary, res.err = r.execute(ctx, targets, o, verify, skipExisting, waitDerive, events)
		close(res.done)
	}()

	return events, func() (Summary, error) {
		<-res.done
		return res.summary, res.err
	}
}

// execute validates limits, plans the run and fans the per-item
// sequential chains out under the global concurrency cap.
func (r *UploadRunner) execute(ctx context.Context, targets []UploadTarget, o ias3.HeaderOpts, verify, skipExisting, waitDerive bool, events chan<- Event) (Summary, error) {
	if err := r.lim.Validate(); err != nil {
		return Summary{}, err
	}

	c := &counters{}

	items, err := r.planRun(targets, events, c)
	if err != nil {
		return c.summary(), err
	}

	var paused atomic.Bool

	var authOnce sync.Once

	var authErr error

	// One goroutine per ITEM: the files of an item run strictly
	// sequentially inside it; the errgroup cap bounds how many items
	// are in flight. Workers never return errors — per-file failures
	// are journaled — so cancellation travels through gctx.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.lim.Concurrency)

	for _, it := range items {
		if paused.Load() || gctx.Err() != nil {
			break // stop admitting; in-flight items finish
		}

		g.Go(func() error {
			r.processItem(gctx, it, o, verify, skipExisting, waitDerive, events, c, &paused, &authOnce, &authErr)
			return nil
		})
	}

	_ = g.Wait()

	if authErr != nil {
		return c.summary(), authErr
	}

	return c.summary(), ctx.Err()
}

// uploadFile is one planned file of an item.
type uploadFile struct {
	target     string // composite journal target
	t          UploadTarget
	rec        *journal.Record
	bootstrap  bool // first PUT of the item: AutoMakeBucket + SizeHint
	lastInPlan bool // last PUT of the item: QueueDerive true
}

// uploadItem is one identifier's strictly sequential chain.
type uploadItem struct {
	identifier string
	files      []uploadFile
	totalBytes int64 // SizeHint for the bootstrap PUT
}

// planRun assembles the per-item chains: resumed queued records
// prepend, then fresh or re-run targets; already-done targets are
// reported skipped (this contract has no force flag). Files are grouped
// by identifier preserving first-appearance order; within an item the
// first file bootstraps the item and the last one queues derive.
func (r *UploadRunner) planRun(targets []UploadTarget, events chan<- Event, c *counters) ([]uploadItem, error) { //nolint:gocognit // journal-state → queue-decision matrix; each branch is one documented resume rule
	seen := map[string]bool{}

	var order []string

	byItem := map[string][]uploadFile{}

	add := func(t UploadTarget, rec *journal.Record) {
		tgt := uploadRecordTarget(t)
		if seen[tgt] {
			return
		}

		seen[tgt] = true

		if _, ok := byItem[t.Identifier]; !ok {
			order = append(order, t.Identifier)
		}

		byItem[t.Identifier] = append(byItem[t.Identifier], uploadFile{target: tgt, t: t, rec: rec})
	}

	resume, err := r.st.ByStates([]journal.Kind{journal.KindUpload}, []journal.State{journal.StateQueued})
	if err != nil {
		r.con.Errorf("Не удалось прочитать журнал для возобновления: %v", err)
		return nil, fmt.Errorf("чтение журнала не выполнено: %w", err)
	}

	for i := range resume {
		rec := resume[i] // ByStates returns copies

		t, ok := parseUploadRecordTarget(rec.Target)
		if !ok {
			return nil, fmt.Errorf("запись журнала повреждена — не разбирается как цель загрузки: %q", rec.Target)
		}

		// Restore the persisted per-target metadata (--auto-meta): a
		// resumed upload must rebuild the same PUT headers.
		t.Title, t.Description, t.Extra = rec.Title, rec.Description, rec.Extra

		add(t, &rec)
	}

	for _, raw := range targets {
		t, err := normalizeUploadTarget(raw)
		if err != nil {
			return nil, err
		}

		tgt := uploadRecordTarget(t)
		if seen[tgt] {
			// Queued in a previous run AND re-listed now: merge the fresh
			// derivation onto the resume entry (fresh wins, journal
			// fills — mergeFreshMeta).
			mergeFreshMeta(byItem[t.Identifier], tgt, t)

			continue
		}

		rec, ok, err := r.st.Get(journal.KindUpload, tgt)
		if err != nil {
			r.con.Errorf("Не удалось прочитать запись журнала для %s: %v", t.Path, err)
			return nil, fmt.Errorf("чтение журнала не выполнено: %w", err)
		}

		if ok && rec.State == journal.StateDone {
			r.emit(events, Event{Kind: EventState, Target: tgt, State: journal.StateSkipped})
			r.con.Dimf("Пропущено (уже загружено ранее): %s → %s/%s", t.Path, t.Identifier, t.Key)
			c.incSkipped()

			continue
		}

		rec, t, err = r.relistRecord(tgt, t, rec, ok)
		if err != nil {
			return nil, err
		}

		add(t, rec)
	}

	items := make([]uploadItem, 0, len(order))
	for _, id := range order {
		files := byItem[id]

		var total int64

		for i := range files {
			st, err := os.Stat(files[i].t.Path)
			if err != nil {
				continue // the hash stage reports this failure properly
			}

			total += st.Size()
		}

		files[0].bootstrap = true
		files[len(files)-1].lastInPlan = true
		items = append(items, uploadItem{identifier: id, files: files, totalBytes: total})
	}

	return items, nil
}

// relistRecord prepares an explicitly re-listed target's journal record
// (existed = the journal already holds one). A previous run's record
// (failed or skipped) is reset to queued — keeping the record and the
// persisted digests, so the file is not re-hashed — and its per-target
// metadata is refreshed with the mergeFreshMeta guard semantics: fresh
// non-empty values win, persisted values are never wiped by an empty
// re-listing. The in-memory target rides the merged values too, so an
// empty-fresh re-list still PUTs the journal-held metadata. A first-seen
// target gets a fresh queued record.
func (r *UploadRunner) relistRecord(tgt string, t UploadTarget, rec *journal.Record, existed bool) (*journal.Record, UploadTarget, error) {
	if !existed {
		fresh := &journal.Record{
			Kind:        journal.KindUpload,
			Target:      tgt,
			State:       journal.StateQueued,
			Title:       t.Title,
			Description: t.Description,
			Extra:       t.Extra,
		}
		if err := r.st.Put(fresh); err != nil {
			r.con.Errorf("Не удалось создать запись журнала для %s: %v", t.Path, err)
			return nil, t, fmt.Errorf("создание записи журнала не выполнено: %w", err)
		}

		return fresh, t, nil
	}

	// Compute the MERGED metadata FIRST and use the same values for the
	// journal record (persisted by the UpdateState below), the in-memory
	// record and the target — the reset-time write must never persist
	// raw empty fresh values (round-4 🟠). Guards: fresh non-empty wins;
	// persisted values are never wiped by an empty re-listing; Extra per
	// name — fresh replaces, held fills missing names (mergeExtra).
	mergedTitle, mergedDescription := t.Title, t.Description
	if mergedTitle == "" {
		mergedTitle = rec.Title
	}

	if mergedDescription == "" {
		mergedDescription = rec.Description
	}

	mergedExtra := mergeExtra(t.Extra, rec.Extra)

	if err := r.st.UpdateState(journal.KindUpload, tgt, journal.StateQueued, func(m *journal.Record) {
		resetRecord(m)
		m.Title, m.Description, m.Extra = mergedTitle, mergedDescription, mergedExtra
	}); err != nil {
		r.con.Errorf("Не удалось сбросить запись журнала для %s: %v", t.Path, err)
		return nil, t, fmt.Errorf("сброс записи журнала не выполнен: %w", err)
	}

	rec.State = journal.StateQueued
	rec.ErrKind, rec.ErrMsg = "", ""
	rec.Title, rec.Description, rec.Extra = mergedTitle, mergedDescription, mergedExtra

	// The in-memory target rides the merged values too, so an
	// empty-fresh re-list still PUTs the journal-held metadata.
	t.Title, t.Description, t.Extra = mergedTitle, mergedDescription, mergedExtra

	return rec, t, nil
}

// mergeExtra merges held (journal) Extra into fresh Extra BY NAME: for
// every name present in fresh, the fresh values REPLACE the held ones
// entirely — a --meta subject=x retry must not resurrect the derived
// subjects, and a creator-source both→file retry must drop the PDF Author
// value. Held values fill in only for names fresh does not carry. Values
// within one name stay order-preserving and deduplicated.
func mergeExtra(fresh, held ias3.Meta) ias3.Meta {
	if len(fresh) == 0 {
		return held
	}

	merged := make(ias3.Meta, len(fresh)+len(held))
	for name, values := range fresh {
		merged[name] = dedupeStrings(values)
	}

	for name, values := range held {
		if _, freshHasName := fresh[name]; freshHasName {
			continue
		}

		merged[name] = append(merged[name], values...)
	}

	return merged
}

// dedupeStrings copies values keeping the first occurrence of each string.
func dedupeStrings(values []string) []string {
	if len(values) <= 1 {
		return values
	}

	out := make([]string, 0, len(values))
	for _, v := range values {
		if !containsString(out, v) {
			out = append(out, v)
		}
	}

	return out
}

func containsString(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}

	return false
}

// mergeFreshMeta applies a re-listed target's fresh derivation onto the
// already-added resume entry: fresh values win when present, the journal
// fills the rest (per field; Extra via mergeExtra).
func mergeFreshMeta(files []uploadFile, tgt string, fresh UploadTarget) {
	for idx := range files {
		if files[idx].target != tgt {
			continue
		}

		if fresh.Title != "" {
			files[idx].t.Title = fresh.Title
		}

		if fresh.Description != "" {
			files[idx].t.Description = fresh.Description
		}

		if len(fresh.Extra) > 0 {
			files[idx].t.Extra = mergeExtra(fresh.Extra, files[idx].t.Extra)
		}
	}
}

// putOptions materializes one file's PUT option set from the batch
// surface: item bootstrap (AutoMakeBucket + SizeHint) on the first PUT,
// QueueDerive on the last, and the per-target metadata overlay
// (--auto-meta, Task 14): non-empty Title/Description override, Extra
// values append after the batch values for the same names.
func (r *UploadRunner) putOptions(o ias3.HeaderOpts, it *uploadItem, f *uploadFile) ias3.HeaderOpts {
	opt := o

	opt.AutoMakeBucket = f.bootstrap
	if f.bootstrap {
		opt.SizeHint = it.totalBytes
	} else {
		opt.SizeHint = 0
	}

	opt.QueueDerive = o.QueueDerive && f.lastInPlan

	if f.t.Title != "" {
		opt.Title = f.t.Title
	}

	if f.t.Description != "" {
		opt.Description = f.t.Description
	}

	if len(f.t.Extra) > 0 {
		extra := make(ias3.Meta, len(o.Extra)+len(f.t.Extra))
		for name, values := range o.Extra {
			extra[name] = append(extra[name], values...)
		}

		for name, values := range f.t.Extra {
			extra[name] = append(extra[name], values...)
		}

		opt.Extra = extra
	}

	return opt
}

// processItem runs one item's sequential chain: hash → skip-if-exists →
// preflight → PUT → verify per file, then the per-item derive watch.
func (r *UploadRunner) processItem(ctx context.Context, it uploadItem, o ias3.HeaderOpts, verify, skipExisting, waitDerive bool, events chan<- Event, c *counters, paused *atomic.Bool, authOnce *sync.Once, authErr *error) { //nolint:gocognit,gocyclo // per-file state machine: preflight/skip/PUT/verify branches mirror the wire; 72 gocognit / 34 gocyclo vs caps 30/20
	if paused.Load() {
		return
	}

	var (
		baseline     int64
		haveBaseline bool
		deriveArmed  bool // a PUT carrying QueueDerive=true succeeded
	)

	for i := range it.files {
		f := &it.files[i]

		if paused.Load() || ctx.Err() != nil {
			return
		}

		r.emit(events, Event{Kind: EventState, Target: f.target, State: f.rec.State})

		// Stage 1: hash once. The digests persist, so resumed runs
		// (and re-runs after failures) never re-read the file here.
		if f.rec.MD5 == "" {
			md5hex, shaHex, size, err := hashUploadFilePath(f.t.Path)
			if err != nil {
				r.failFile(events, c, f, "hash", err.Error())
				continue
			}

			f.rec.MD5, f.rec.SHA256, f.rec.Size = md5hex, shaHex, size
			if err := r.persist(f, journal.StateQueued); err != nil {
				r.journalError(events, c, f, err)
				continue
			}
		}

		// Stage 2: skip-if-exists by remote MD5 — and only while the
		// item has no active tasks (reference-client guard): during
		// derive/ingest the metadata answer may be stale, so a matching
		// md5 alone does not prove the skip. An erroring Tasks API
		// makes the skip undecidable — the file fails retryable (the
		// next run retries it) instead of skipping on doubt.
		if skipExisting { //nolint:nestif // skip-guard: md5 proof and tasks-busy checks nest by design
			exists, err := r.cl.FileExistsMatchingMD5(ctx, f.t.Identifier, f.t.Key, f.rec.MD5)
			if err != nil {
				if ctx.Err() != nil {
					return // cancelled; the record stays queued
				}

				r.failFile(events, c, f, "skip-check", err.Error())

				continue
			}

			if exists {
				active, err := r.cl.HasActiveTasks(ctx, f.t.Identifier)
				if err != nil {
					if ctx.Err() != nil {
						return // cancelled; the record stays queued
					}

					r.failFile(events, c, f, "skip-guard", fmt.Sprintf("не удалось проверить активные задачи item — пропуск недостоверен: %v", err))

					continue
				}

				if active {
					note := "в item активны задачи — пропуск отменён, файл будет загружен"
					r.emit(events, Event{Kind: EventWait, Target: f.target, State: f.rec.State, ErrMsg: note})
					r.con.Dimf("%s → %s/%s: %s", f.t.Path, f.t.Identifier, f.t.Key, note)
				} else {
					note := "уже в элементе, пропущен (md5 совпал)"

					f.rec.ErrKind, f.rec.ErrMsg = "", ""
					if err := r.persist(f, journal.StateSkipped); err != nil {
						r.journalError(events, c, f, err)
						continue
					}

					c.incSkipped()
					r.emit(events, Event{Kind: EventState, Target: f.target, State: journal.StateSkipped, ErrMsg: note})
					r.con.Dimf("Пропущено (уже в элементе, md5 совпал): %s → %s/%s", f.t.Path, f.t.Identifier, f.t.Key)

					continue
				}
			}
		}

		// Stage 3: check_limit preflight — poll while the item's upload
		// queue is over limit; the fail-safe cap ends a stably broken
		// endpoint. Runs per file, right before admission, so the
		// baseline below stays the closest pre-PUT snapshot.
		if err := ias3.AwaitNotOverLimit(ctx, r.cl, f.t.Identifier); err != nil {
			if ctx.Err() != nil {
				return // cancelled while waiting; the record stays queued
			}

			r.failFile(events, c, f, "check-limit", fmt.Sprintf("лимит очереди IA-S3 стабильно занят: %v", err))

			continue
		}

		// Stages 3+6 prep: capture the derive baseline right before the
		// item's first actual PUT. An item whose files all skip never
		// reaches this. On error the whole chain stops: the remaining
		// files stay queued and resumable, because every later PUT
		// would leave the derive watch without a baseline.
		if o.QueueDerive && !haveBaseline {
			base, err := r.cl.NewestDeriveTaskID(ctx, it.identifier)
			if err != nil {
				if ctx.Err() != nil {
					return // cancelled; the record stays queued
				}

				r.failFile(events, c, f, "derive-baseline", err.Error())

				return
			}

			baseline, haveBaseline = base, true
		}

		// Pacing gate for PUT admissions (no-op at rate.Inf).
		if err := r.gate.Wait(ctx); err != nil {
			return // cancelled while pacing; the record stays queued
		}

		// Stage 4: the PUT. ias3.Upload owns the SlowDown retry budget;
		// a returned error is terminal here and the next run restarts
		// the file from byte 0 (no resume exists server-side).
		opt := r.putOptions(o, &it, f)

		size := f.rec.Size
		thr := &progressThrottle{total: size}
		progress := func(sent int64) {
			if thr.emit(sent) {
				r.emit(events, Event{Kind: EventProgress, Target: f.target, Sent: sent, Total: size})
			}
		}

		req := ias3.UploadRequest{
			Identifier: f.t.Identifier,
			Key:        f.t.Key,
			Path:       f.t.Path,
			Size:       size,
			MD5Hex:     f.rec.MD5,
		}
		if err := r.cl.Upload(ctx, req, opt, progress); err != nil {
			if ctx.Err() != nil {
				// Cancelled mid-PUT: an interrupt, not a failure — the
				// digests are persisted, the record stays queued and the
				// next run restarts the file from byte 0.
				return
			}

			if errors.Is(err, ias3.ErrBadCredentials) {
				r.failFile(events, c, f, "auth", err.Error())
				r.authPause(events, paused, authOnce, authErr, err)

				return
			}

			r.failFile(events, c, f, "upload", err.Error())

			continue
		}
		// The derive-queueing PUT went out successfully: arm the watch
		// even if verify below fails — the derive is queued server-side
		// either way. A failed or skip-matched last file never arms it.
		if o.QueueDerive && f.lastInPlan {
			deriveArmed = true
		}

		r.emit(events, Event{Kind: EventProgress, Target: f.target, Sent: size, Total: size})

		// Stage 5: verify the remote copy by MD5.
		if verify {
			if err := r.verifyUpload(ctx, f.t, f.rec.MD5); err != nil {
				if ctx.Err() != nil {
					// Cancelled during verify (M-2 ruling): the PUT
					// succeeded, so this is an interrupt — the record
					// stays queued and the next run re-verifies it.
					return
				}

				r.failFile(events, c, f, "verify", err.Error())

				continue
			}
		}

		// Done.
		f.rec.ErrKind, f.rec.ErrMsg = "", ""
		if err := r.persist(f, journal.StateDone); err != nil {
			r.journalError(events, c, f, err)
			continue
		}

		c.incDone()
		r.emit(events, Event{Kind: EventState, Target: f.target, State: journal.StateDone})
		r.con.Successf("Загружено: %s → %s/%s", f.t.Path, f.t.Identifier, f.t.Key)
	}

	// Stage 6: one derive watch per item, after the last file — only
	// when the caller asked for it (waitDerive, the --wait-derive flag)
	// AND a PUT actually carrying QueueDerive=true succeeded (armed).
	// Without the flag the run ends here: the derive is on the IA
	// queue, the tool does not wait for it. If the last file failed or
	// skip-matched, no derive was queued and waiting would burn the
	// JobWait budget into a false item failure.
	if !deriveArmed || !waitDerive || !haveBaseline {
		return
	}

	r.emit(events, Event{Kind: EventWait, Target: it.identifier, ErrMsg: "ожидание деривации элемента"})
	r.con.Infof("Ожидание деривации элемента «%s»...", it.identifier)

	err := ias3.WaitDerive(ctx, r.cl, it.identifier, baseline, r.lim.PollStart, r.lim.JobWait)
	switch {
	case err == nil:
		r.emit(events, Event{Kind: EventWait, Target: it.identifier, ErrMsg: ias3.DeriveDone.String()})
		r.con.Successf("Деривация элемента «%s» завершена", it.identifier)
	case ctx.Err() != nil:
		// The run is stopping — not a derive failure.
	default:
		// Item-level failure: the files stay done, the summary counts
		// one extra failure.
		c.incFailed()
		r.emit(events, Event{Kind: EventState, Target: it.identifier, State: journal.StateFailed, ErrMsg: err.Error()})
		r.con.Errorf("Деривация элемента «%s»: %v", it.identifier, err)
	}
}

// verifyUpload confirms the uploaded copy: the item's metadata must
// list the key with the local MD5. Right after a PUT the file may be
// invisible (ingest lag) — missing entries and digest-less entries are
// retried; a definite mismatch fails immediately.
func (r *UploadRunner) verifyUpload(ctx context.Context, t UploadTarget, md5hex string) error {
	var lastErr error

	for attempt := 1; attempt <= verifyAttempts; attempt++ {
		files, err := r.cl.ItemFiles(ctx, t.Identifier)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("проверка загрузки прервана: %w", ctx.Err())
			}

			lastErr = err
		} else {
			lastErr = nil

			for _, f := range files {
				if f.Name != t.Key {
					continue
				}

				switch f.MD5 {
				case md5hex:
					return nil
				case "":
					// No digest to prove identity with — treat as lag.
					lastErr = fmt.Errorf("файл %s пока числится в элементе без md5", t.Key)
				default:
					return fmt.Errorf("проверка целостности не пройдена: md5 файла %s в элементе (%s) не совпадает с локальным (%s)",
						t.Key, f.MD5, md5hex)
				}
			}

			if lastErr == nil {
				lastErr = fmt.Errorf("файл %s пока не появился в метаданных элемента (задержка обработки archive.org)", t.Key)
			}
		}

		if attempt == verifyAttempts {
			return fmt.Errorf("проверка загрузки не удалась после %d попыток: %w", verifyAttempts, lastErr)
		}

		if !sleepCtx(ctx, r.lim.PollStart) {
			return fmt.Errorf("проверка загрузки прервана: %w", ctx.Err())
		}
	}
	// Unreachable: every loop path returns.
	return lastErr
}

// persist mirrors the runner-owned fields into the journal under the
// given state; on success the local record's state follows.
func (r *UploadRunner) persist(f *uploadFile, st journal.State) error {
	err := r.st.UpdateState(journal.KindUpload, f.target, st, func(m *journal.Record) {
		m.Attempts = f.rec.Attempts
		m.MD5 = f.rec.MD5
		m.SHA256 = f.rec.SHA256
		m.Size = f.rec.Size
		m.ErrKind = f.rec.ErrKind
		m.ErrMsg = f.rec.ErrMsg
		// Per-target metadata (--auto-meta): set once at record creation,
		// carried through every persist so a resume rebuilds the PUT.
		m.Title = f.rec.Title
		m.Description = f.rec.Description
		m.Extra = f.rec.Extra
	})
	if err == nil {
		f.rec.State = st
	}

	return err
}

// failFile journals a failed record and emits the failure.
func (r *UploadRunner) failFile(events chan<- Event, c *counters, f *uploadFile, kind, msg string) {
	f.rec.ErrKind, f.rec.ErrMsg = kind, msg
	if err := r.persist(f, journal.StateFailed); err != nil {
		r.journalError(events, c, f, err)
		return
	}

	c.incFailed()
	r.emit(events, Event{Kind: EventState, Target: f.target, State: journal.StateFailed, ErrMsg: msg})
	r.con.Errorf("Не загружено: %s → %s/%s: %s", f.t.Path, f.t.Identifier, f.t.Key, msg)
}

// journalError reports a journal write failure: the transition cannot
// be guaranteed, so the file counts as failed and the error is loud.
func (r *UploadRunner) journalError(events chan<- Event, c *counters, f *uploadFile, err error) {
	c.incFailed()

	msg := fmt.Sprintf("ошибка записи журнала: %v", err)
	r.emit(events, Event{Kind: EventState, Target: f.target, State: f.rec.State, ErrMsg: msg})
	r.con.Errorf("%s: %v", f.t.Path, err)
}

// authPause fires once per run: it stops admitting new uploads and
// emits one clear Russian event. In-flight items keep running to their
// journaled ends.
func (r *UploadRunner) authPause(events chan<- Event, paused *atomic.Bool, authOnce *sync.Once, authErr *error, cause error) {
	authOnce.Do(func() {
		paused.Store(true)

		*authErr = fmt.Errorf("ошибка авторизации archive.org (%v): выполнение очереди приостановлено — проверьте ключи s3_access/s3_secret в конфигурации и запустите снова", cause)
		r.emit(events, Event{Kind: EventAuth, ErrMsg: (*authErr).Error()})
		r.con.Errorf("%v", *authErr)
	})
}

// emit sends one event downstream. The consumer must drain the stream
// until close — the contract of Run.
func (r *UploadRunner) emit(events chan<- Event, ev Event) {
	events <- ev
}

// uploadRecordTarget builds the composite journal target of a file:
// abs path + "\x00" + identifier + "/" + key.
func uploadRecordTarget(t UploadTarget) string {
	return t.Path + uploadTargetSep + t.Identifier + "/" + t.Key
}

// parseUploadRecordTarget is the inverse of uploadRecordTarget; false
// marks a record the runner cannot resume (never expected — fail loudly
// at the call site).
func parseUploadRecordTarget(s string) (UploadTarget, bool) {
	path, rest, ok := strings.Cut(s, uploadTargetSep)
	if !ok || path == "" {
		return UploadTarget{}, false
	}

	id, key, ok := strings.Cut(rest, "/")
	if !ok || id == "" || key == "" {
		return UploadTarget{}, false
	}

	return UploadTarget{Path: path, Identifier: id, Key: key}, true
}

// normalizeUploadTarget validates one target and applies the
// documented defaults: absolute path, Key = base name, identifier
// required.
func normalizeUploadTarget(t UploadTarget) (UploadTarget, error) {
	if t.Path == "" {
		return t, errors.New("цель загрузки без пути к файлу")
	}

	if t.Identifier == "" {
		return t, fmt.Errorf("для файла %s не указан идентификатор элемента", t.Path)
	}

	abs, err := filepath.Abs(t.Path)
	if err != nil {
		return t, fmt.Errorf("не удалось определить полный путь для %s: %w", t.Path, err)
	}

	t.Path = abs
	if t.Key == "" {
		t.Key = filepath.Base(abs)
	}

	return t, nil
}

// hashUploadFilePath streams the file once and returns both digests and
// the byte count (the Size the PUT declares — a file that changed since
// hashing is rejected by ias3.Upload's size check).
func hashUploadFilePath(path string) (md5hex, shaHex string, size int64, err error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from the operator's own request
	if err != nil {
		return "", "", 0, fmt.Errorf("не удалось открыть файл %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	md5hex, shaHex, size, err = hashUploadFile(f)
	if err != nil {
		return "", "", 0, fmt.Errorf("файл %s: %w", path, err)
	}

	return md5hex, shaHex, size, nil
}

// hashUploadFile computes MD5 and SHA-256 in a single streaming pass:
// chunked reads through io.MultiWriter into both hashers (the chunked
// TeeReader pattern generalized to two digests). Nothing is buffered
// whole, so multi-GB files hash in constant memory.
func hashUploadFile(src io.Reader) (md5hex, shaHex string, size int64, err error) {
	md5h := md5.New()
	shaH := sha256.New()
	buf := make([]byte, 1<<20)

	n, err := io.CopyBuffer(io.MultiWriter(md5h, shaH), src, buf)
	if err != nil {
		return "", "", 0, fmt.Errorf("подсчёт контрольных сумм: %w", err)
	}

	return hex.EncodeToString(md5h.Sum(nil)), hex.EncodeToString(shaH.Sum(nil)), n, nil
}
