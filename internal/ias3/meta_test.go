package ias3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWebTestClient returns an ias3.Client with BOTH hosts repointed at one
// httptest fake: webBase (archive.org — metadata + tasks endpoints, the
// surface under test here) and base (s3.us.archive.org — so a stray S3 call
// from a buggy path can never reach the live network). Same override seam
// as newTestClient; tests run sequentially, mutation is race-free.
func newWebTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	cl, ts := newTestClient(t, h)
	cl.webBase = ts.URL
	return cl
}

// newWebTestClientWithCookies is newWebTestClient with cookie fields ALSO
// populated in the credentials: DeriveStatus must still attach zero cookies
// (the Tasks API rejects them), which only this setup can prove.
func newWebTestClientWithCookies(t *testing.T, h http.Handler) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cl := NewClient(httpx.NewClient(nil, "wauploader/test"), auth.Credentials{
		Access:     "AKIAIOSFODNN7EXAMPLE",
		Secret:     "wJalrXUtnFEMI",
		CookieUser: "user@example.com;2026",
		CookieSig:  "cookie-sig-secret-value",
	})
	cl.base = ts.URL
	cl.webBase = ts.URL
	return cl
}

func readMetaFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

// metaHandler serves a fixture for the metadata path and records the last
// request (path + Authorization) for wire assertions. The record lives in a
// mutex-guarded wireRecorder: the handler writes it from its own goroutine
// while the test reads it from the test goroutine.
func metaHandler(t *testing.T, fixture string, rec *wireRecorder) http.Handler {
	t.Helper()
	raw := readMetaFixture(t, fixture)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.update(func(cr *wireRecord) {
				cr.path = r.URL.Path
				cr.authorization = r.Header.Get("Authorization")
			})
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
}

// tasksHandler serves a fixture for the tasks.php path and records the last
// request (query criteria, Authorization, cookies) for wire assertions. The
// record lives in a mutex-guarded wireRecorder: the handler writes it from
// its own goroutine while the test reads it from the test goroutine.
func tasksHandler(t *testing.T, fixture string, rec *wireRecorder) http.Handler {
	t.Helper()
	raw := readMetaFixture(t, fixture)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.update(func(cr *wireRecord) {
				q := r.URL.Query()
				cr.identifier = len(q["identifier"])
				cr.catalog = len(q["catalog"])
				cr.history = len(q["history"])
				cr.limit = len(q["limit"])
				cr.authorization = r.Header.Get("Authorization")
				cr.cookieHeader = r.Header.Get("Cookie")
				cr.cookieCount = len(r.Cookies())
			})
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
}

// --- ItemFiles -------------------------------------------------------------

func TestItemFilesParses(t *testing.T) {
	rec := &wireRecorder{}
	cl := newWebTestClient(t, metaHandler(t, "meta_files.json", rec))

	files, err := cl.ItemFiles(context.Background(), "testid1")
	require.NoError(t, err)
	require.Len(t, files, 3, "every files[] entry must be mapped")

	got := rec.get()
	assert.Equal(t, "/metadata/testid1", got.path, "must GET the Metadata Read endpoint for the identifier")
	assert.True(t, strings.HasPrefix(got.authorization, "LOW "),
		"S3 keys must ride as Authorization: LOW (restricted items need them; cookies are never used)")

	// String-typed numbers (size/mtime arrive as strings in real answers,
	// numbers in the docs) must parse through the same field.
	assert.Equal(t, ItemFile{
		Name:  "file.bin",
		MD5:   "5d41402abc4b2a76b9719d911017c592",
		SHA1:  "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d",
		CRC32: "2ef17948",
		Size:  5,
	}, files[0])

	// Numeric-typed size must parse identically.
	assert.Equal(t, int64(12345), files[1].Size)
	assert.Equal(t, "second.dat", files[1].Name)

	// A derivative entry without digests maps to empty strings, not an error.
	assert.Equal(t, "nomd5.bin", files[2].Name)
	assert.Empty(t, files[2].MD5)
	assert.Empty(t, files[2].SHA1)
	assert.Empty(t, files[2].CRC32)
	assert.Equal(t, int64(1), files[2].Size)

	// Malformed identifier fails fast in Russian before any request.
	_, err = cl.ItemFiles(context.Background(), "ab")
	require.Error(t, err, "identifier rules must gate the metadata read too")
	assert.Contains(t, err.Error(), "5")
}

func TestItemFilesEmptyItem(t *testing.T) {
	for _, fixture := range []string{"meta_empty.json"} {
		cl := newWebTestClient(t, metaHandler(t, fixture, nil))
		files, err := cl.ItemFiles(context.Background(), "testid1")
		require.NoErrorf(t, err, "%s: an empty item is a valid answer, not an error", fixture)
		assert.NotNilf(t, files, "%s: contract requires an empty slice, not nil", fixture)
		assert.Lenf(t, files, 0, "%s: empty item must carry zero files", fixture)
	}
}

// --- FileExistsMatchingMD5 ---------------------------------------------------

func TestFileExistsMatchingMD5(t *testing.T) {
	cl := newWebTestClient(t, metaHandler(t, "meta_files.json", nil))
	ctx := context.Background()

	// Present + identical digest → skip.
	skip, err := cl.FileExistsMatchingMD5(ctx, "testid1", "file.bin", "5d41402abc4b2a76b9719d911017c592")
	require.NoError(t, err)
	assert.True(t, skip, "same key with the same md5 must be reported as existing")

	// Present + different digest → re-upload.
	skip, err = cl.FileExistsMatchingMD5(ctx, "testid1", "file.bin", "00000000000000000000000000000000")
	require.NoError(t, err)
	assert.False(t, skip, "a digest mismatch must force a re-upload")

	// Absent key → false, nil.
	skip, err = cl.FileExistsMatchingMD5(ctx, "testid1", "missing.bin", "5d41402abc4b2a76b9719d911017c592")
	require.NoError(t, err)
	assert.False(t, skip, "a missing file can never be skipped")

	// Key names are case-sensitive on IA-S3.
	skip, err = cl.FileExistsMatchingMD5(ctx, "testid1", "FILE.BIN", "5d41402abc4b2a76b9719d911017c592")
	require.NoError(t, err)
	assert.False(t, skip, "key comparison must be exact (case-sensitive)")

	// Empty local digest: identity is unverifiable — never skip.
	skip, err = cl.FileExistsMatchingMD5(ctx, "testid1", "file.bin", "")
	require.NoError(t, err)
	assert.False(t, skip, "an unknown local digest must never match (fail-safe re-upload)")

	// Stored digest absent (derivative entry): nothing to compare → no skip.
	skip, err = cl.FileExistsMatchingMD5(ctx, "testid1", "nomd5.bin", "abc")
	require.NoError(t, err)
	assert.False(t, skip, "a file without a stored md5 cannot prove identity")
}

// --- DeriveStatus ------------------------------------------------------------

// TestDeriveStates pins the full wait_admin → DeriveState mapping from real
// fixture envelopes: catalog-active states (queued/running/error/paused),
// done (left catalog, present in history), and none (no derive anywhere).
func TestDeriveStates(t *testing.T) {
	cases := []struct {
		fixture  string
		want     DeriveState
		wantFrag string // Russian console fragment required in String()
	}{
		{"tasks_none.json", DeriveNone, "не запущена"},
		{"tasks_queued.json", DeriveQueued, "в очереди"},
		{"tasks_running.json", DeriveRunning, "выполняется"},
		{"tasks_error.json", DeriveError, "ошибкой"},
		{"tasks_paused.json", DerivePaused, "приостановлена"},
		{"tasks_done.json", DeriveDone, "завершена"},
	}
	for _, tc := range cases {
		rec := &wireRecorder{}
		cl := newWebTestClient(t, tasksHandler(t, tc.fixture, rec))

		st, err := cl.DeriveStatus(context.Background(), "testid1")
		require.NoErrorf(t, err, "%s", tc.fixture)
		assert.Equalf(t, tc.want, st, "%s: wrong state mapping", tc.fixture)
		assert.Containsf(t, st.String(), tc.wantFrag,
			"%s: Russian console String() must describe the state", tc.fixture)

		got := rec.get()
		assert.Equalf(t, 1, got.identifier, "%s: identifier criteria required", tc.fixture)
		assert.Equalf(t, 1, got.catalog, "%s: catalog=1 required", tc.fixture)
		assert.Equalf(t, 1, got.history, "%s: history=1 required", tc.fixture)
		assert.Equalf(t, 1, got.limit, "%s: explicit page limit required (presence scan)", tc.fixture)
	}
}

// TestDeriveStatusTasksAuthHeader proves the hard Tasks-API credential rule:
// Authorization: LOW present, cookies never sent — even when the credential
// set carries them.
func TestDeriveStatusTasksAuthHeader(t *testing.T) {
	rec := &wireRecorder{}
	cl := newWebTestClientWithCookies(t, tasksHandler(t, "tasks_running.json", rec))

	st, err := cl.DeriveStatus(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, DeriveRunning, st)

	got := rec.get()
	assert.True(t, strings.HasPrefix(got.authorization, "LOW AKIAIOSFODNN7EXAMPLE:"),
		"Tasks API requires the S3-key LOW header, got %q", got.authorization)
	assert.Empty(t, got.cookieHeader, "the raw Cookie header must never be attached to tasks.php")
	assert.Equal(t, 0, got.cookieCount, "the Tasks API rejects cookies — zero cookies may reach it")
}

// TestDeriveErrorEnvelope: success:false envelopes must surface the server's
// error text inside a Russian-framed failure, never a silent zero state.
func TestDeriveErrorEnvelope(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_envelope_error.json", nil))

	st, err := cl.DeriveStatus(context.Background(), "testid1")
	require.Error(t, err, "success:false must be an error, not a state")
	assert.Equal(t, DeriveNone, st, "no state may be reported from a failed envelope")
	assert.Contains(t, err.Error(), "you must be logged in to use this service",
		"the server's error text must survive into the message for diagnosis")
	assert.Contains(t, err.Error(), "archive.org", "Russian framing must name the service")
	assert.NotContains(t, err.Error(), "wJalrXUtnFEMI", "the secret must never leak into errors")
}

// --- WaitDerive ---------------------------------------------------------------

func TestWaitDeriveTimeout(t *testing.T) {
	var calls atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(readMetaFixture(t, "tasks_running.json"))
	})
	cl, ts := newTestClient(t, h)
	cl.webBase = ts.URL

	err := WaitDerive(context.Background(), cl, "testid1", 0, 2*time.Millisecond, 40*time.Millisecond)
	require.Error(t, err, "a derive that never finishes must end in a timeout error")
	assert.Contains(t, err.Error(), "время", "the timeout error must be Russian and name the wait")
	assert.Contains(t, err.Error(), "testid1", "the timeout error must name the item")
	assert.Greater(t, calls.Load(), int64(1), "the loop must have polled repeatedly, not given up at once")
}

func TestWaitDeriveSuccessAfterPolling(t *testing.T) {
	var calls atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture := "tasks_queued.json"
		if calls.Add(1) >= 3 {
			fixture = "tasks_done.json"
		}
		_, _ = w.Write(readMetaFixture(t, fixture))
	})
	cl, ts := newTestClient(t, h)
	cl.webBase = ts.URL

	err := WaitDerive(context.Background(), cl, "testid1", 0, time.Millisecond, 5*time.Second)
	require.NoError(t, err, "queued→queued→done must end in success")
	assert.GreaterOrEqual(t, calls.Load(), int64(3), "every non-terminal poll must precede the done answer")
}

// TestWaitDeriveErrorTerminal: wait_admin=2 is terminal with the task_id in
// the message; the loop must stop immediately, not keep polling.
func TestWaitDeriveErrorTerminal(t *testing.T) {
	var calls atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(readMetaFixture(t, "tasks_error.json"))
	})
	cl, ts := newTestClient(t, h)
	cl.webBase = ts.URL

	err := WaitDerive(context.Background(), cl, "testid1", 0, time.Millisecond, 5*time.Second)
	require.Error(t, err)
	assert.Equal(t, int64(1), calls.Load(), "a derive error is terminal — no further polling")
	assert.Contains(t, err.Error(), "103", "the error must include the derive task_id when present")
	assert.Contains(t, err.Error(), "ошибкой", "the error must be Russian")
}

// --- Fix round 2: baseline task_id derives contract + wait budget ----------
//
// Supersedes round 1's notBefore approach (controller ruling on CodeRabbit
// PR#11 findings): the caller captures the largest pre-upload derive
// task_id (baseline) via NewestDeriveTaskID just before the PUT; a history
// derive counts as done only when its task_id is NEWER than the baseline;
// ANY catalog task (including non-derive archive.php) means pending and
// never falls through to history.

// TestDeriveStatusBaselineBlocksStaleHistory: the fixture's done derive has
// task_id 99; with baseline 99 (captured pre-PUT) the history entry is NOT
// newer → pending, never a faked done.
func TestDeriveStatusBaselineBlocksStaleHistory(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_done.json", nil))

	st, err := cl.DeriveStatusSince(context.Background(), "testid1", 99)
	require.NoError(t, err)
	assert.Equal(t, DeriveNone, st, "a history derive not newer than the baseline belongs to a prior batch")
}

// TestDeriveStatusFreshBaselineHistoryDone: zero baseline (fresh item / no
// prior derives) → any history derive is this batch's result → done.
func TestDeriveStatusFreshBaselineHistoryDone(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_done.json", nil))

	st, err := cl.DeriveStatusSince(context.Background(), "testid1", 0)
	require.NoError(t, err)
	assert.Equal(t, DeriveDone, st, "with no prior derives the history derive is the current batch's done")
}

// TestWaitDeriveBaselineBlocksStaleHistory proves the baseline loop
// end-to-end: baseline equal to the history derive's id keeps WaitDerive
// polling into the timeout — no success on poll #1.
func TestWaitDeriveBaselineBlocksStaleHistory(t *testing.T) {
	var calls atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(readMetaFixture(t, "tasks_done.json"))
	})
	cl, ts := newTestClient(t, h)
	cl.webBase = ts.URL

	err := WaitDerive(context.Background(), cl, "testid1", 99, time.Millisecond, 25*time.Millisecond)
	require.Error(t, err, "a non-newer history derive must keep the caller waiting, never fake success")
	assert.Contains(t, err.Error(), "время", "the wait must end in the Russian timeout error")
	assert.Greater(t, calls.Load(), int64(1), "the stale-done answer must not terminate the loop")
}

// TestDeriveStatusAnyCatalogTaskBlocksHistory: a running archive.php (the
// PUT's own ingest task) fills the catalog — the observation must stay
// pending and NEVER fall through to the done derive sitting in history.
func TestDeriveStatusAnyCatalogTaskBlocksHistory(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_catalog_archivephp.json", nil))

	st, err := cl.DeriveStatusSince(context.Background(), "testid1", 0)
	require.NoError(t, err)
	assert.Equal(t, DeriveNone, st, "non-empty catalog means pending even with a done derive in history — archive.php ingest runs before the derive")
}

// TestDeriveStatusNewestCatalogWins: tasks serialize per item, so the
// newest catalog derive is the live one — an older errored task must not
// mask the queued successor (controller finding 2, round 1 — retained).
func TestDeriveStatusNewestCatalogWins(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_catalog_old_error.json", nil))

	st, err := cl.DeriveStatus(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, DeriveQueued, st, "newest catalog task (200, queued) wins over stale error 100")
}

// TestDeriveStatusErrorWhenNewestErrored: the error state still surfaces
// when the newest catalog derive IS the errored one.
func TestDeriveStatusErrorWhenNewestErrored(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_catalog_error_newest.json", nil))

	st, err := cl.DeriveStatus(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, DeriveError, st, "newest catalog task (201) is the errored one → error")
}

// TestNewestDeriveTaskID: 0 on a task-less item; the largest derive
// task_id across catalog and history otherwise; non-derive tasks (the
// ingest archive.php) are ignored even when their id is larger.
func TestNewestDeriveTaskID(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_none.json", nil))
	id, err := cl.NewestDeriveTaskID(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), id, "no derives at all → baseline 0")

	cl = newWebTestClient(t, tasksHandler(t, "tasks_catalog_old_error.json", nil))
	id, err = cl.NewestDeriveTaskID(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, int64(200), id, "newest across catalog+history (200 > 100)")

	cl = newWebTestClient(t, tasksHandler(t, "tasks_done.json", nil))
	id, err = cl.NewestDeriveTaskID(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, int64(99), id, "history derives count toward the baseline")

	cl = newWebTestClient(t, tasksHandler(t, "tasks_catalog_archivephp.json", nil))
	id, err = cl.NewestDeriveTaskID(context.Background(), "testid1")
	require.NoError(t, err)
	assert.Equal(t, int64(50), id, "only derive tasks count: archive.php id 60 is ignored, derive 50 wins")

	cl = newWebTestClient(t, tasksHandler(t, "tasks_envelope_error.json", nil))
	_, err = cl.NewestDeriveTaskID(context.Background(), "testid1")
	require.Error(t, err, "a failed envelope must surface as an error, never a silent baseline 0")
}

// TestWaitDeriveBudgetBoundsInFlight: the timeout must bound in-flight
// requests too — with the budget shorter than one poll interval the wait
// ends promptly, far before a full poll cycle would elapse.
func TestWaitDeriveBudgetBoundsInFlight(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_running.json", nil))

	start := time.Now()
	err := WaitDerive(context.Background(), cl, "testid1", 0, 200*time.Millisecond, 30*time.Millisecond)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "время", "budget exhaustion must be the Russian timeout error")
	assert.Less(t, elapsed, 150*time.Millisecond,
		"the wait must end on the budget (30ms), not after a full 200ms poll cycle")
}

// TestWaitDeriveParentCancel: a cancelled parent context ends the wait as
// an interruption (Ctrl-C semantics), distinct from budget exhaustion.
func TestWaitDeriveParentCancel(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_running.json", nil))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(5*time.Millisecond, cancel)

	err := WaitDerive(ctx, cl, "testid1", 0, 50*time.Millisecond, 5*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "прервано", "a parent cancellation must read as an interruption")
	assert.True(t, errors.Is(err, context.Canceled), "context.Canceled must survive the wrap, got: %v", err)
}

// TestWaitDeriveTimeoutGuard: symmetric fail-fast guards for non-positive
// timeout and poll (round-1 finding 3 — retained); no request may leave.
func TestWaitDeriveTimeoutGuard(t *testing.T) {
	cl, ts := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may be sent when the arguments are invalid")
	}))
	cl.webBase = ts.URL

	err := WaitDerive(context.Background(), cl, "testid1", 0, time.Millisecond, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "время", "the timeout guard must be Russian like the poll guard")

	err = WaitDerive(context.Background(), cl, "testid1", 0, 0, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "интервал опроса", "the poll guard must stand")
}

// TestHasActiveTasks: the skip-guard primitive (reference-client
// semantics) — ANY catalog task counts as active regardless of its
// command (archive.php ingest included); an empty catalog is quiet;
// an error envelope is an error, never a silent false.
func TestHasActiveTasks(t *testing.T) {
	cl := newWebTestClient(t, tasksHandler(t, "tasks_catalog_archivephp.json", nil))
	active, err := cl.HasActiveTasks(context.Background(), "testid1")
	require.NoError(t, err)
	assert.True(t, active, "a catalog task of ANY command means the item is busy")

	cl = newWebTestClient(t, tasksHandler(t, "tasks_none.json", nil))
	active, err = cl.HasActiveTasks(context.Background(), "testid1")
	require.NoError(t, err)
	assert.False(t, active, "an empty catalog means no active tasks")

	cl = newWebTestClient(t, tasksHandler(t, "tasks_envelope_error.json", nil))
	active, err = cl.HasActiveTasks(context.Background(), "testid1")
	require.Error(t, err)
	assert.False(t, active, "an error must not read as a false negative")
}
