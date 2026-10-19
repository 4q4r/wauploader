// Package runner tests: the save pipeline is verified against an
// in-process fake SPN2 server (httptest). spn2.baseURL is a
// package-private variable, so the fake is wired in by wrapping the
// exported httpx.Client.HTTP transport and rewriting the
// web.archive.org host to the httptest listener — the spn2 package is
// not modified for tests.
package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/4q4r/wauploader/internal/journal"
	"github.com/4q4r/wauploader/internal/spn2"
	"golang.org/x/time/rate"
)

// --- fake SPN2 server -------------------------------------------------------

// fakeResp is one canned HTTP answer.
type fakeResp struct {
	code int
	body string
}

// fakeSPN2 serves POST /save, GET /save/status/<job> and
// GET /save/status/user. Handlers are driven by per-call functions;
// nil functions fall back to a successful submit and a successful
// status. The struct is goroutine-safe: the save runner hits it from
// several workers.
type fakeSPN2 struct {
	srv *httptest.Server

	mu              sync.Mutex
	hold            time.Duration
	inflight        int
	maxInflight     int
	submits         int
	submitsByTarget map[string]int
	submitTimes     []time.Time
	pollsByJob      map[string]int
	userPolls       int

	submitFn func(target string, nth int) fakeResp
	statusFn func(jobID string, nth int) fakeResp
	userFn   func() fakeResp
}

func newFakeSPN2(t *testing.T) *fakeSPN2 {
	t.Helper()
	f := &fakeSPN2{
		submitsByTarget: map[string]int{},
		pollsByJob:      map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /save", f.handleSave)
	mux.HandleFunc("/save/status/user", f.handleUser)
	mux.HandleFunc("/save/status/", f.handleStatus)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client builds an spn2.Client pointed at the fake: the default
// httpx transport is wrapped so every web.archive.org request is
// rewritten to the httptest listener.
func (f *fakeSPN2) client() *spn2.Client {
	cl := httpx.NewClient(nil, "wauploader-tests")
	cl.HTTP.Transport = rewriteTransport{inner: cl.HTTP.Transport, target: mustURL(f.srv.URL)}
	return spn2.NewClient(cl, auth.Credentials{})
}

func (f *fakeSPN2) handleSave(w http.ResponseWriter, r *http.Request) {
	target := r.FormValue("url")
	f.mu.Lock()
	f.submits++
	nth := f.submits
	f.submitsByTarget[target]++
	f.submitTimes = append(f.submitTimes, time.Now())
	f.inflight++
	if f.inflight > f.maxInflight {
		f.maxInflight = f.inflight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	if f.hold > 0 {
		time.Sleep(f.hold)
	}
	resp := fakeResp{code: http.StatusOK, body: fmt.Sprintf(`{"job_id":"job-%d","status":"pending"}`, nth)}
	if f.submitFn != nil {
		resp = f.submitFn(target, f.count(f.submitsByTarget, target))
	}
	writeFake(w, resp)
}

func (f *fakeSPN2) handleStatus(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/save/status/")
	f.mu.Lock()
	f.pollsByJob[jobID]++
	nth := f.pollsByJob[jobID]
	f.mu.Unlock()
	resp := fakeResp{code: http.StatusOK, body: `{"status":"success","job_id":"` + jobID + `","original_url":"https://example.com/","timestamp":"20260119120000","download_size":1024}`}
	if f.statusFn != nil {
		resp = f.statusFn(jobID, nth)
	}
	writeFake(w, resp)
}

func (f *fakeSPN2) handleUser(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.userPolls++
	f.mu.Unlock()
	resp := fakeResp{code: http.StatusOK, body: `{"available":1,"processing":0}`}
	if f.userFn != nil {
		resp = f.userFn()
	}
	writeFake(w, resp)
}

// userCount reports how many times the session-quota probe was hit.
func (f *fakeSPN2) userCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userPolls
}

func (f *fakeSPN2) count(m map[string]int, k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[k]
}

func (f *fakeSPN2) snapshot() (submits, maxInflight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submits, f.maxInflight
}

func (f *fakeSPN2) submitGaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, 0, len(f.submitTimes))
	for i := 1; i < len(f.submitTimes); i++ {
		out = append(out, f.submitTimes[i].Sub(f.submitTimes[i-1]))
	}
	return out
}

func writeFake(w http.ResponseWriter, resp fakeResp) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.code)
	_, _ = io.WriteString(w, resp.body)
}

// rewriteTransport redirects web.archive.org requests to the fake
// server, leaving every other URL untouched.
type rewriteTransport struct {
	inner  http.RoundTripper
	target *url.URL
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "web.archive.org" {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = rt.target.Scheme
		clone.URL.Host = rt.target.Host
		clone.Host = rt.target.Host
		req = clone
	}
	return rt.inner.RoundTrip(req)
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// --- test scaffolding -------------------------------------------------------

// testLimits: tiny poll backoffs and a short JobWait so tests stay in
// the millisecond range; rate is infinite unless a test overrides it.
func testLimits() Limits {
	return Limits{
		PerMinute:   rate.Inf,
		Concurrency: 4,
		PerHost:     3,
		JobWait:     5 * time.Second,
		PollStart:   5 * time.Millisecond,
		PollMax:     15 * time.Millisecond,
	}
}

func newTestRunner(t *testing.T, f *fakeSPN2, lim Limits) (*SaveRunner, *journal.Store) {
	t.Helper()
	st, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	con := console.New(io.Discard, io.Discard, false, true)
	return NewSaveRunner(f.client(), st, lim, con), st
}

// runAll drains the event stream to completion and returns the events
// plus the completion results.
func runAll(t *testing.T, ctx context.Context, r *SaveRunner, targets []string, opts spn2.Options, force bool) ([]Event, Summary, error) {
	t.Helper()
	events, done := r.Run(ctx, targets, opts, force)
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	sum, err := done()
	return got, sum, err
}

func mustRecord(t *testing.T, st *journal.Store, target string) *journal.Record {
	t.Helper()
	rec, ok, err := st.Get(journal.KindSave, target)
	require.NoError(t, err)
	require.True(t, ok, "journal record for %s must exist", target)
	return rec
}

func submitOK(jobID string) fakeResp {
	return fakeResp{code: http.StatusOK, body: `{"job_id":"` + jobID + `","status":"pending"}`}
}

func statusPending(size int64) fakeResp {
	return fakeResp{code: http.StatusOK, body: fmt.Sprintf(`{"status":"pending","job_id":"job-1","url":"https://example.com/","download_size":%d}`, size)}
}

func statusSuccess() fakeResp {
	return fakeResp{code: http.StatusOK, body: `{"status":"success","job_id":"job-1","original_url":"https://example.com/","timestamp":"20260119120000","download_size":4096}`}
}

// --- tests ------------------------------------------------------------------

func TestHappyPathSingleURL(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp { return statusSuccess() }
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	events, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1}, sum)

	// Event stream covers the full ladder: queued → submitted → done.
	states := []journal.State{}
	for _, ev := range events {
		if ev.Kind == EventState {
			states = append(states, ev.State)
		}
	}
	require.Equal(t, []journal.State{
		journal.StateQueued, journal.StateSubmitted, journal.StateDone,
	}, states)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateDone, rec.State)
	require.Equal(t, "job-1", rec.SPN2JobID)
	require.Equal(t, "20260119120000", rec.SnapshotTimestamp)
	require.Equal(t, "https://web.archive.org/web/20260119120000/https://example.com/", rec.SnapshotURL)
}

func TestPendingPollsUntilSuccess(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp {
		switch nth {
		case 1, 2:
			return statusPending(0)
		default:
			return statusSuccess()
		}
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	_, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateDone, rec.State)
	require.Equal(t, "https://web.archive.org/web/20260119120000/https://example.com/", rec.SnapshotURL)
	require.GreaterOrEqual(t, f.pollsByJob["job-1"], 3, "job must be polled until success")
}

func TestStallDetectionDownloadSizePlateau(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp { return statusPending(100) }
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	events, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Failed: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateFailed, rec.State)
	require.Equal(t, "stall", rec.ErrKind)
	require.NotEmpty(t, rec.ErrMsg)

	// The plateau of three identical nonzero sizes must stop the poll
	// loop at the third identical reading, not run to JobWait.
	require.LessOrEqual(t, f.pollsByJob["job-1"], 4)
	found := false
	for _, ev := range events {
		if ev.State == journal.StateFailed && ev.Target == target {
			found = true
		}
	}
	require.True(t, found, "a failed event must be emitted for the stalled job")
}

func TestQuotaParksUntilNextRun(t *testing.T) {
	f := newFakeSPN2(t)
	f.submitFn = func(target string, nth int) fakeResp {
		return fakeResp{code: http.StatusOK, body: `{"status":"error","status_ext":"error:too-many-daily-captures","message":"daily capture limit reached"}`}
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	events, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Parked: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateQuotaParked, rec.State)
	require.Equal(t, "quota", rec.ErrKind)
	require.Contains(t, rec.ErrMsg, "полуночи", "park message must carry the midnight hint")

	parked := false
	for _, ev := range events {
		if ev.State == journal.StateQuotaParked && strings.Contains(ev.ErrMsg, "полуночи") {
			parked = true
		}
	}
	require.True(t, parked, "a quota_parked event with the midnight hint must be emitted")
}

func TestDedupIsDone(t *testing.T) {
	f := newFakeSPN2(t)
	f.submitFn = func(target string, nth int) fakeResp {
		return fakeResp{code: http.StatusOK, body: `{"status":"ok","job_id":null,"message":"Recently archived: see https://web.archive.org/web/20240101000000/https://example.com/"}`}
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	events, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateDone, rec.State)
	require.Equal(t, "20240101000000", rec.SnapshotTimestamp)
	require.Equal(t, "https://web.archive.org/web/20240101000000/https://example.com/", rec.SnapshotURL)

	done := false
	for _, ev := range events {
		if ev.State == journal.StateDone && ev.SnapshotURL == rec.SnapshotURL {
			done = true
		}
	}
	require.True(t, done, "dedup must produce a done event with the existing snapshot URL")
}

func TestSubmitRefusalTerminalJournaled(t *testing.T) {
	// Review Focus #1: SPN2 can refuse with HTTP 200 and no job_id;
	// the runner must journal a terminal failure, never poll.
	f := newFakeSPN2(t)
	f.submitFn = func(target string, nth int) fakeResp {
		return fakeResp{code: http.StatusOK, body: `{"status":"error","status_ext":"error:blocked-url","message":"blocked"}`}
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	_, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Failed: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateFailed, rec.State)
	require.Equal(t, "terminal", rec.ErrKind)
	require.Contains(t, rec.ErrMsg, "error:blocked-url")
	require.Zero(t, f.pollsByJob["job-1"], "a refused target must never be polled")
}

func TestResumeSkipsDoneAndPollsPending(t *testing.T) {
	f := newFakeSPN2(t)
	st, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	doneTarget := "https://done.example.com/"
	require.NoError(t, st.Put(&journal.Record{
		Kind: journal.KindSave, Target: doneTarget, State: journal.StateDone,
		SnapshotURL: "https://web.archive.org/web/20250101000000/https://done.example.com/",
	}))
	pendingTarget := "https://pending.example.com/"
	require.NoError(t, st.Put(&journal.Record{
		Kind: journal.KindSave, Target: pendingTarget, State: journal.StatePending,
		SPN2JobID: "job-b",
	}))

	con := console.New(io.Discard, io.Discard, false, true)
	r := NewSaveRunner(f.client(), st, testLimits(), con)

	// Without force: done target is skipped, pending target is polled.
	f.statusFn = func(jobID string, nth int) fakeResp {
		require.Equal(t, "job-b", jobID, "only the pending record's job may be polled")
		return statusSuccess()
	}
	events, sum, err := runAll(t, context.Background(), r, []string{doneTarget, pendingTarget}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1, Skipped: 1}, sum)
	skipped := false
	for _, ev := range events {
		if ev.State == journal.StateSkipped && ev.Target == doneTarget {
			skipped = true
		}
	}
	require.True(t, skipped)
	_, submits := f.snapshot()
	require.Zero(t, submits, "resume without force must not resubmit done targets")

	// With force: the done target goes through a fresh submit.
	f.submitFn = func(target string, nth int) fakeResp { return submitOK("job-f") }
	f.statusFn = func(jobID string, nth int) fakeResp {
		require.Equal(t, "job-f", jobID, "a force-resubmitted target must poll its new job")
		return statusSuccess()
	}
	events, sum, err = runAll(t, context.Background(), r, []string{doneTarget}, spn2.Options{}, true)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1}, sum)
	_, submits = f.snapshot()
	require.Equal(t, 1, submits, "force must resubmit exactly the done target")
	require.Equal(t, journal.StateDone, mustRecord(t, st, doneTarget).State)
}

func TestPerHostLimiterCapsConcurrency(t *testing.T) {
	f := newFakeSPN2(t)
	f.hold = 80 * time.Millisecond
	lim := testLimits()
	lim.Concurrency = 4
	lim.PerHost = 2
	r, _ := newTestRunner(t, f, lim)

	targets := []string{
		"https://example.com/a", "https://example.com/b",
		"https://example.com/c", "https://example.com/d",
		"https://example.com/e",
	}
	_, sum, err := runAll(t, context.Background(), r, targets, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 5}, sum)

	_, maxInflight := f.snapshot()
	require.LessOrEqual(t, maxInflight, 2, "per-host cap must bound simultaneous captures")
	require.GreaterOrEqual(t, maxInflight, 2, "cap of 2 must actually allow overlap (otherwise the test proves nothing)")
}

func TestGlobalRateLimit(t *testing.T) {
	f := newFakeSPN2(t)
	lim := testLimits()
	lim.PerMinute = rate.Every(50 * time.Millisecond) // one submit per 50 ms
	r, _ := newTestRunner(t, f, lim)

	targets := []string{
		"https://a.example.com/", "https://b.example.com/",
		"https://c.example.com/", "https://d.example.com/",
		"https://e.example.com/",
	}
	_, sum, err := runAll(t, context.Background(), r, targets, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 5}, sum)

	submits, _ := f.snapshot()
	require.Equal(t, 5, submits)
	for i, gap := range f.submitGaps() {
		require.GreaterOrEqual(t, gap, 45*time.Millisecond,
			"submit %d arrived too early: the global limiter must pace admissions", i+1)
	}
}

func TestAuthFailurePausesQueue(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp { return statusSuccess() }
	lim := testLimits()
	lim.Concurrency = 1
	r, st := newTestRunner(t, f, lim)

	first, second, third := "https://one.example.com/", "https://two.example.com/", "https://three.example.com/"
	f.submitFn = func(target string, nth int) fakeResp {
		if strings.Contains(target, "two.example.com") {
			return fakeResp{code: http.StatusUnauthorized, body: `{"status":"error","status_ext":"error:unauthorized"}`}
		}
		return submitOK("job-" + strings.TrimSuffix(strings.TrimPrefix(target, "https://"), ".example.com/"))
	}

	events, sum, err := runAll(t, context.Background(), r, []string{first, second, third}, spn2.Options{}, false)
	require.Error(t, err, "auth failure must surface from the completion func")
	require.Contains(t, err.Error(), "авторизации")
	require.Equal(t, Summary{Done: 1}, sum)

	authEvents := 0
	for _, ev := range events {
		if ev.Kind == EventAuth {
			authEvents++
			require.Contains(t, ev.ErrMsg, "авторизации")
		}
	}
	require.Equal(t, 1, authEvents, "exactly one auth event must be emitted")

	submits, _ := f.snapshot()
	require.Equal(t, 2, submits, "the queue must stop admitting after the auth failure")

	require.Equal(t, journal.StateDone, mustRecord(t, st, first).State)
	require.Equal(t, journal.StateQueued, mustRecord(t, st, second).State)
	require.Equal(t, journal.StateQueued, mustRecord(t, st, third).State)
	require.Zero(t, mustRecord(t, st, second).Attempts, "an auth refusal must not charge the resubmit budget")
	require.Zero(t, mustRecord(t, st, third).Attempts)
}

func TestSessionLimitWaitsForUserStatus(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp { return statusSuccess() }
	userCalls := 0
	f.userFn = func() fakeResp {
		userCalls++
		if userCalls == 1 {
			return fakeResp{code: http.StatusOK, body: `{"available":0,"processing":1}`}
		}
		return fakeResp{code: http.StatusOK, body: `{"available":1,"processing":0}`}
	}
	f.submitFn = func(target string, nth int) fakeResp {
		if nth == 1 {
			return fakeResp{code: http.StatusOK, body: `{"status":"error","status_ext":"error:user-session-limit","message":"session busy"}`}
		}
		return submitOK("job-sl")
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	_, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1}, sum)

	require.GreaterOrEqual(t, userCalls, 2, "the runner must poll UserStatus until the session frees")
	require.Equal(t, 2, f.count(f.submitsByTarget, target), "the target must be resubmitted after the session frees")
	require.Zero(t, mustRecord(t, st, target).Attempts, "session-limit waits must not charge the resubmit budget")
}

func TestStatusAuth401PausesQueue(t *testing.T) {
	f := newFakeSPN2(t)
	f.submitFn = func(target string, nth int) fakeResp { return submitOK("job-401") }
	f.statusFn = func(jobID string, nth int) fakeResp {
		return fakeResp{code: http.StatusUnauthorized, body: `{"status":"error","status_ext":"error:unauthorized"}`}
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	events, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.Error(t, err, "a status-endpoint 401 must surface from the completion func")
	require.Contains(t, err.Error(), "авторизации")
	require.Equal(t, Summary{}, sum)

	authEvents := 0
	for _, ev := range events {
		if ev.Kind == EventAuth {
			authEvents++
		}
	}
	require.Equal(t, 1, authEvents, "exactly one auth event must be emitted")

	// Not terminal-failed: the submitted job stays submitted for the
	// next run's resume.
	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateSubmitted, rec.State)
	require.Empty(t, rec.ErrKind)
}

func TestUserStatusAuth401PausesQueue(t *testing.T) {
	f := newFakeSPN2(t)
	f.submitFn = func(target string, nth int) fakeResp {
		return fakeResp{code: http.StatusOK, body: `{"status":"error","status_ext":"error:user-session-limit","message":"session busy"}`}
	}
	f.userFn = func() fakeResp {
		return fakeResp{code: http.StatusUnauthorized, body: `{"status":"error","status_ext":"error:unauthorized"}`}
	}
	lim := testLimits()
	lim.JobWait = 300 * time.Millisecond
	r, st := newTestRunner(t, f, lim)

	target := "https://example.com/"
	events, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.Error(t, err, "a UserStatus 401 must surface from the completion func")
	require.Contains(t, err.Error(), "авторизации")
	require.Equal(t, Summary{}, sum)

	authEvents := 0
	for _, ev := range events {
		if ev.Kind == EventAuth {
			authEvents++
		}
	}
	require.Equal(t, 1, authEvents, "exactly one auth event must be emitted")

	// No spin: the session wait must stop on the first auth failure.
	require.Equal(t, 1, f.userCount(), "UserStatus must be probed exactly once before the pause")
	require.Equal(t, 1, f.count(f.submitsByTarget, target), "no resubmit behind a paused queue")

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateQueued, rec.State)
	require.Zero(t, rec.Attempts, "the auth refusal must not charge the attempt budget")
}

func TestJobWaitTimeoutMarksFailed(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp { return statusPending(0) }
	lim := testLimits()
	lim.JobWait = 80 * time.Millisecond
	r, st := newTestRunner(t, f, lim)

	target := "https://example.com/"
	_, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Failed: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateFailed, rec.State)
	require.Equal(t, "timeout", rec.ErrKind)
	require.NotEmpty(t, rec.ErrMsg)
}

func TestChaos502StormRecovers(t *testing.T) {
	f := newFakeSPN2(t)
	f.statusFn = func(jobID string, nth int) fakeResp { return statusSuccess() }
	f.submitFn = func(target string, nth int) fakeResp {
		if nth <= 2 {
			return fakeResp{code: http.StatusBadGateway, body: "<html>Bad Gateway</html>"}
		}
		return submitOK("job-chaos")
	}
	r, st := newTestRunner(t, f, testLimits())

	target := "https://example.com/"
	_, sum, err := runAll(t, context.Background(), r, []string{target}, spn2.Options{}, false)
	require.NoError(t, err)
	require.Equal(t, Summary{Done: 1}, sum)

	rec := mustRecord(t, st, target)
	require.Equal(t, journal.StateDone, rec.State)
	require.Equal(t, 2, rec.Attempts, "two failed submits must be journaled in the Attempts counter")
	require.Equal(t, 3, f.submitsByTarget[target], "the runner must resubmit until the storm passes")
}

func TestLimitsValidate(t *testing.T) {
	// T2 ruling: [limits] numeric bounds are the runner's duty.
	base := testLimits()
	require.NoError(t, base.Validate())

	noRate := base
	noRate.PerMinute = 0
	require.ErrorContains(t, noRate.Validate(), "save_per_minute")

	negRate := base
	negRate.PerMinute = rate.Limit(-1)
	require.ErrorContains(t, negRate.Validate(), "save_per_minute")

	noConc := base
	noConc.Concurrency = 0
	require.ErrorContains(t, noConc.Validate(), "save_concurrency")

	noHost := base
	noHost.PerHost = 0
	require.ErrorContains(t, noHost.Validate(), "per_host_concurrency")

	tooManyHost := base
	tooManyHost.PerHost = 6
	require.ErrorContains(t, tooManyHost.Validate(), "per_host_concurrency")

	noWait := base
	noWait.JobWait = 0
	require.ErrorContains(t, noWait.Validate(), "job_wait")

	badPoll := base
	badPoll.PollStart = time.Second
	badPoll.PollMax = time.Millisecond
	require.ErrorContains(t, badPoll.Validate(), "poll_max")
}
