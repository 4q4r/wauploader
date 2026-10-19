// Package runner tests: the upload pipeline is verified against an
// in-process fake IA-S3 server (httptest). ias3.Client points at the
// real s3.us.archive.org / archive.org hosts, so the fake is wired in
// by wrapping the exported httpx.Client.HTTP transport and rewriting
// both hosts to the httptest listener — the ias3 package is not
// modified for tests (same pattern as save_test.go).
package runner

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/4q4r/wauploader/internal/ias3"
	"github.com/4q4r/wauploader/internal/journal"
)

// --- fake IA-S3 server -------------------------------------------------------

// fakeIAS3 serves the four IA-S3 endpoints the upload pipeline touches:
// PUT /<id>/<key>, GET /?check_limit=1, GET /metadata/<id> and
// GET /services/tasks.php. All state is mutex-guarded: parallel items
// hit it from several goroutines.
type fakeIAS3 struct {
	srv *httptest.Server

	mu               sync.Mutex
	holds            time.Duration
	putsByKey        map[string]int
	hdrByKey         map[string]http.Header // last PUT's headers per key
	bodyByKey        map[string][]byte      // accumulated body bytes per key
	inflightByID     map[string]int
	maxInflightByID  map[string]int
	inflightTotal    int
	maxInflightTotal int
	putSeq           []string // global PUT start order
	metaCalls        int
	metaFiles        map[string][]metaEntry // per identifier
	taskCallsByID    map[string]int
	checks           int

	// putFn overrides the PUT status per (identifier, key, nth-put-for-key);
	// nil = always 200. taskFn overrides the tasks.php body per
	// (identifier, nth-call); nil = a derive that completes immediately.
	// failChecks makes the check_limit endpoint answer 501 (non-retryable
	// for the httpx ladder) — the stalled-queue preflight path.
	putFn      func(identifier, key string, nth int) int
	taskFn     func(identifier string, nth int) string
	failChecks bool

	// blockPutUntilCancel / blockMetaUntilCancel make the PUT / metadata
	// handler hang until the request context is cancelled — the mid-flight
	// interrupt tests (cancel lands while the stage is in flight).
	// blockPutKey narrows the PUT block to one key ("" = every PUT).
	blockPutUntilCancel  bool
	blockPutKey          string
	blockMetaUntilCancel bool
}

type metaEntry struct {
	name string
	md5  string
}

func newFakeIAS3(t *testing.T) *fakeIAS3 {
	t.Helper()
	f := &fakeIAS3{
		putsByKey:       map[string]int{},
		hdrByKey:        map[string]http.Header{},
		bodyByKey:       map[string][]byte{},
		inflightByID:    map[string]int{},
		maxInflightByID: map[string]int{},
		metaFiles:       map[string][]metaEntry{},
		taskCallsByID:   map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /{id}/{key}", f.handlePut)
	mux.HandleFunc("GET /metadata/{id}", f.handleMeta)
	mux.HandleFunc("GET /services/tasks.php", f.handleTasks)
	mux.HandleFunc("/", f.handleRoot) // check_limit=1 preflight
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIAS3) handlePut(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("key")
	f.mu.Lock()
	f.putsByKey[key]++
	nth := f.putsByKey[key]
	f.hdrByKey[key] = r.Header.Clone()
	f.bodyByKey[key] = append(f.bodyByKey[key], readAllBounded(r.Body)...)
	f.inflightByID[id]++
	if f.inflightByID[id] > f.maxInflightByID[id] {
		f.maxInflightByID[id] = f.inflightByID[id]
	}
	f.inflightTotal++
	if f.inflightTotal > f.maxInflightTotal {
		f.maxInflightTotal = f.inflightTotal
	}
	f.putSeq = append(f.putSeq, key)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflightByID[id]--
		f.inflightTotal--
		f.mu.Unlock()
	}()
	if f.holds > 0 {
		time.Sleep(f.holds)
	}
	f.mu.Lock()
	blocked := f.blockPutUntilCancel && (f.blockPutKey == "" || f.blockPutKey == key)
	f.mu.Unlock()
	if blocked {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	code := http.StatusOK
	if f.putFn != nil {
		code = f.putFn(id, key, nth)
	}
	if code == http.StatusOK {
		w.WriteHeader(http.StatusOK)
		return
	}
	// AWS-style error XML; a 4xx with valid XML is terminal for ias3.
	// 403 carries InvalidAccessKeyId — the credentials-rejection shape.
	errCode := "InvalidArgument"
	if code == http.StatusForbidden {
		errCode = "InvalidAccessKeyId"
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `<Error><Code>`+errCode+`</Code><Message>fake failure</Message></Error>`)
}

func (f *fakeIAS3) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("check_limit") {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.checks++
	fail := f.failChecks
	f.mu.Unlock()
	if fail {
		// 501 Not Implemented: the httpx ladder does not retry it, so
		// the preflight outage path is deterministic and fast.
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"over_limit":0}`)
}

func (f *fakeIAS3) handleMeta(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	f.metaCalls++
	files := f.metaFiles[id]
	f.mu.Unlock()
	f.mu.Lock()
	blocked := f.blockMetaUntilCancel
	f.mu.Unlock()
	if blocked {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	f.mu.Lock()
	var b strings.Builder
	b.WriteString(`{"files":[`)
	for i, e := range files {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":%q,"md5":%q,"size":1}`, e.name, e.md5)
	}
	b.WriteString(`]}`)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, b.String())
}

func (f *fakeIAS3) handleTasks(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("identifier")
	f.mu.Lock()
	f.taskCallsByID[id]++
	nth := f.taskCallsByID[id]
	body := ""
	if f.taskFn != nil {
		body = f.taskFn(id, nth)
	} else {
		// Default: derive finishes immediately — the history task_id
		// grows with every call, so it is always newer than the
		// baseline captured on the first call.
		body = fmt.Sprintf(`{"success":true,"value":{"catalog":[],"history":[{"task_id":%d,"cmd":"derive.php"}]}}`, 50+nth)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// readAllBounded drains a request body fully (the fake must consume the
// whole PUT for honest progress semantics) with a small cap.
func readAllBounded(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 8<<20))
	return b
}

// client builds an ias3.Client pointed at the fake: the default httpx
// transport is wrapped so both archive.org hosts rewrite to the fake.
func (f *fakeIAS3) client() *ias3.Client {
	cl := httpx.NewClient(nil, "wauploader-tests")
	cl.HTTP.Transport = rewriteArchiveTransport{inner: cl.HTTP.Transport, target: mustURL(f.srv.URL)}
	return ias3.NewClient(cl, auth.Credentials{Access: "test-access", Secret: "test-secret"})
}

// rewriteArchiveTransport redirects s3.us.archive.org and archive.org
// requests to the fake server, leaving every other URL untouched.
type rewriteArchiveTransport struct {
	inner  http.RoundTripper
	target *url.URL
}

func (rt rewriteArchiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "s3.us.archive.org" || req.URL.Host == "archive.org" {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = rt.target.Scheme
		clone.URL.Host = rt.target.Host
		clone.Host = rt.target.Host
		req = clone
	}
	return rt.inner.RoundTrip(req)
}

// --- test scaffolding -------------------------------------------------------

// uploadTestLimits: tiny derive poll (PollStart feeds WaitDerive) and a
// short JobWait so the derive wait stays in the millisecond range.
func uploadTestLimits() Limits {
	return Limits{
		PerMinute:   rate.Inf,
		Concurrency: 2,
		PerHost:     3,
		JobWait:     5 * time.Second,
		PollStart:   5 * time.Millisecond,
		PollMax:     15 * time.Millisecond,
	}
}

// uploadOpts mirrors config defaults: collection/mediatype required,
// queue_derive and keep_old_version true.
func uploadOpts() ias3.HeaderOpts {
	return ias3.HeaderOpts{
		Collection:     "testcollection",
		Mediatype:      "data",
		QueueDerive:    true,
		KeepOldVersion: true,
	}
}

func newTestUploadRunner(t *testing.T, f *fakeIAS3, lim Limits) (*UploadRunner, *journal.Store) {
	t.Helper()
	st, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return newTestUploadRunnerOnStore(t, f, st, lim), st
}

// newTestUploadRunnerOnStore builds a runner over an existing store —
// the resume tests re-run against the SAME journal.
func newTestUploadRunnerOnStore(t *testing.T, f *fakeIAS3, st *journal.Store, lim Limits) *UploadRunner {
	t.Helper()
	con := console.New(io.Discard, io.Discard, false, true)
	return NewUploadRunner(f.client(), st, lim, con)
}

// writeUploadFiles creates files with the given contents in a temp dir
// and returns the paths in order.
func writeUploadFiles(t *testing.T, contents ...string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(contents))
	for i, c := range contents {
		p := filepath.Join(dir, fmt.Sprintf("file%d.bin", i+1))
		require.NoError(t, os.WriteFile(p, []byte(c), 0o600))
		paths[i] = p
	}
	return paths
}

func uploadTargets(paths []string, identifier string) []UploadTarget {
	out := make([]UploadTarget, len(paths))
	for i, p := range paths {
		out[i] = UploadTarget{Path: p, Identifier: identifier, Key: filepath.Base(p)}
	}
	return out
}

func runUploadAll(t *testing.T, ctx context.Context, r *UploadRunner, targets []UploadTarget, o ias3.HeaderOpts, verify, skipExisting bool) ([]Event, Summary, error) {
	t.Helper()
	events, done := r.Run(ctx, targets, o, verify, skipExisting)
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	sum, err := done()
	return got, sum, err
}

func mustUploadRecord(t *testing.T, st *journal.Store, tgt UploadTarget) *journal.Record {
	t.Helper()
	rec, ok, err := st.Get(journal.KindUpload, uploadRecordTarget(tgt))
	require.NoError(t, err)
	require.True(t, ok, "journal record for %s must exist", tgt.Key)
	return rec
}

func (f *fakeIAS3) puts(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putsByKey[key]
}

func (f *fakeIAS3) headers(key string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hdrByKey[key]
}

func (f *fakeIAS3) body(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodyByKey[key]
}

func (f *fakeIAS3) maxInflight(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflightByID[id]
}

func (f *fakeIAS3) maxTotalInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflightTotal
}

func (f *fakeIAS3) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.putSeq...)
}

func (f *fakeIAS3) taskCalls(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.taskCallsByID[id]
}

func (f *fakeIAS3) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func (f *fakeIAS3) metaCallsTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metaCalls
}

// setBlockPutUntilCancel / setBlockMetaUntilCancel flip the interrupt-test
// hold flags under the fake's mutex (the handler goroutines read them).
func (f *fakeIAS3) setBlockPutUntilCancel(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockPutUntilCancel = v
}

func (f *fakeIAS3) setBlockMetaUntilCancel(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockMetaUntilCancel = v
}

// --- tests ------------------------------------------------------------------

// TestSinglePassHashing: the streaming hash reads the file exactly once
// (total bytes pulled == file size) and produces reference digests; a
// full run persists MD5/SHA256/Size into the journal record.
func TestSinglePassHashing(t *testing.T) {
	content := strings.Repeat("wauploader hashing\r\n", 5000) // ~100 KB
	path := writeUploadFiles(t, content)[0]

	got := &countingFile{}
	md5h, shaHex, size, err := hashUploadFile(got.reader(t, path))
	require.NoError(t, err)

	wantMD5 := md5.Sum([]byte(content))
	wantSHA := sha256.Sum256([]byte(content))
	assert.Equal(t, hex.EncodeToString(wantMD5[:]), md5h)
	assert.Equal(t, hex.EncodeToString(wantSHA[:]), shaHex)
	assert.Equal(t, int64(len(content)), size)
	// Single pass: the source yielded exactly size bytes, no re-reads.
	assert.Equal(t, int64(len(content)), got.n, "file must be read exactly once")

	// And the pipeline persists the digests.
	f := newFakeIAS3(t)
	r, st := newTestUploadRunner(t, f, uploadTestLimits())
	tgts := uploadTargets([]string{path}, "testid-upload-1")
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Done)
	rec := mustUploadRecord(t, st, tgts[0])
	assert.Equal(t, md5h, rec.MD5)
	assert.Equal(t, shaHex, rec.SHA256)
	assert.Equal(t, size, rec.Size)
	assert.Equal(t, journal.StateDone, rec.State)
}

// countingFile wraps a file open + read to prove a single pass.
type countingFile struct {
	n int64
}

func (c *countingFile) reader(t *testing.T, path string) io.Reader {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- test-owned temp path
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return &countReader{r: f, c: c}
}

type countReader struct {
	r io.Reader
	c *countingFile
}

func (cr *countReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.c.n += int64(n)
	return n, err
}

// TestItemFileOrderSequential: within one identifier the PUTs never
// overlap and keep the given order.
func TestItemFileOrderSequential(t *testing.T) {
	paths := writeUploadFiles(t, "one", "two", "three")
	f := newFakeIAS3(t)
	f.holds = 20 * time.Millisecond
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-sequen1")
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 3, sum.Done)

	assert.Equal(t, 1, f.maxInflight("testid-sequen1"), "PUTs into one item must never overlap")
	assert.Equal(t, []string{"file1.bin", "file2.bin", "file3.bin"}, f.sequence(),
		"files of one item must upload in the given order")
	assert.Equal(t, 3, f.checkCount(), "each admitted PUT must pass the check_limit preflight")
	for _, tgt := range tgts {
		assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgt).State)
	}
}

// TestParallelAcrossItems: items upload concurrently — two items with
// held PUTs must overlap when Concurrency >= 2.
func TestParallelAcrossItems(t *testing.T) {
	p1 := writeUploadFiles(t, "alpha")
	p2 := writeUploadFiles(t, "beta")
	f := newFakeIAS3(t)
	f.holds = 80 * time.Millisecond
	r, _ := newTestUploadRunner(t, f, uploadTestLimits())

	targets := []UploadTarget{
		{Path: p1[0], Identifier: "testid-parallela"},
		{Path: p2[0], Identifier: "testid-parallelb"},
	}
	_, sum, err := runUploadAll(t, context.Background(), r, targets, uploadOpts(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Done)
	assert.GreaterOrEqual(t, f.maxTotalInflight(), 2, "distinct items must upload in parallel")
}

// TestSkipExistingByMD5: a remote file whose stored md5 equals the local
// digest is skipped with no PUT.
func TestSkipExistingByMD5(t *testing.T) {
	paths := writeUploadFiles(t, "already-there")
	sum5 := md5.Sum([]byte("already-there"))
	f := newFakeIAS3(t)
	f.metaFiles["testid-skipexis1"] = []metaEntry{{name: "file1.bin", md5: hex.EncodeToString(sum5[:])}}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-skipexis1")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, true)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Skipped)
	assert.Equal(t, 0, sum.Done)
	assert.Equal(t, 0, f.puts("file1.bin"), "a proven-identical remote file must not be re-uploaded")

	rec := mustUploadRecord(t, st, tgts[0])
	assert.Equal(t, journal.StateSkipped, rec.State)
	found := false
	for _, ev := range got {
		if ev.Target == uploadRecordTarget(tgts[0]) && ev.State == journal.StateSkipped {
			found = true
			assert.Contains(t, ev.ErrMsg, "пропущен")
		}
	}
	assert.True(t, found, "expected a skipped event for the existing file")
}

// TestFailedFileRestartsFromZeroOthersKept (Review Focus #4): a
// mid-item failure fails only that file; the re-run re-uploads just the
// failed file from byte 0 while done files keep their journal state.
func TestFailedFileRestartsFromZeroOthersKept(t *testing.T) {
	paths := writeUploadFiles(t, "first-content", "second-content", "third-content")
	f := newFakeIAS3(t)
	f.putFn = func(identifier, key string, nth int) int {
		if key == "file2.bin" && nth == 1 {
			return http.StatusBadRequest // terminal, no ias3 retry loop
		}
		return http.StatusOK
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())
	tgts := uploadTargets(paths, "testid-restart01")

	// Run 1: file2 fails, siblings keep going.
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Done)
	assert.Equal(t, 1, sum.Failed)
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[0]).State)
	assert.Equal(t, journal.StateFailed, mustUploadRecord(t, st, tgts[1]).State)
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[2]).State)
	assert.Equal(t, []string{"file1.bin", "file2.bin", "file3.bin"}, f.sequence(),
		"a failed file must not abort its item: the chain continues in order")

	// Run 2: only the failed file re-uploads, whole content from byte 0.
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	got, sum2, err := runUploadAll(t, context.Background(), r2, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, sum2.Done)
	assert.Equal(t, 2, sum2.Skipped)
	assert.Equal(t, 1, f.puts("file1.bin"), "done files must not re-upload")
	assert.Equal(t, 1, f.puts("file3.bin"), "done files must not re-upload")
	assert.Equal(t, 2, f.puts("file2.bin"), "the failed file must restart")
	assert.Contains(t, string(f.body("file2.bin")), "second-content",
		"re-PUT must carry the whole file from byte 0")

	var skippedTarget, doneTarget bool
	for _, ev := range got {
		if ev.State == journal.StateSkipped && ev.Target == uploadRecordTarget(tgts[0]) {
			skippedTarget = true
		}
		if ev.State == journal.StateDone && ev.Target == uploadRecordTarget(tgts[1]) {
			doneTarget = true
		}
	}
	assert.True(t, skippedTarget, "run 2 must report the done file as skipped")
	assert.True(t, doneTarget, "run 2 must report the restarted file as done")
}

// TestSizeHintOnFirstPUTOnly: only the first PUT of an item carries
// x-archive-size-hint (whole-item total) and auto-make-bucket.
func TestSizeHintOnFirstPUTOnly(t *testing.T) {
	paths := writeUploadFiles(t, "aaa", "bbbbb", "c")
	f := newFakeIAS3(t)
	r, _ := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-sizehint1")
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 3, sum.Done)

	total := int64(len("aaa") + len("bbbbb") + len("c"))
	h1 := f.headers("file1.bin")
	assert.Equal(t, fmt.Sprint(total), h1.Get("x-archive-size-hint"), "first PUT carries the whole-item size hint")
	assert.Equal(t, "1", h1.Get("x-archive-auto-make-bucket"), "first PUT creates the item")
	assert.Empty(t, f.headers("file2.bin").Get("x-archive-size-hint"), "subsequent PUTs carry no size hint")
	assert.Empty(t, f.headers("file3.bin").Get("x-archive-size-hint"), "subsequent PUTs carry no size hint")
	assert.Empty(t, f.headers("file2.bin").Get("x-archive-auto-make-bucket"))
	// The caller's metadata surface rides every PUT.
	for _, key := range []string{"file1.bin", "file2.bin", "file3.bin"} {
		assert.Equal(t, "testcollection", f.headers(key).Get("x-archive-meta01-collection"))
		assert.Equal(t, "data", f.headers(key).Get("x-archive-meta-mediatype"))
	}
}

// TestQueueDeriveFalseExceptLast: only the last file of an item asks
// IA to queue derive; intermediate files suppress it.
func TestQueueDeriveFalseExceptLast(t *testing.T) {
	paths := writeUploadFiles(t, "x", "y", "z")
	f := newFakeIAS3(t)
	r, _ := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-qderivel1")
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 3, sum.Done)

	assert.Equal(t, "0", f.headers("file1.bin").Get("x-archive-queue-derive"))
	assert.Equal(t, "0", f.headers("file2.bin").Get("x-archive-queue-derive"))
	assert.Equal(t, "1", f.headers("file3.bin").Get("x-archive-queue-derive"), "only the last PUT queues derive")

	// The derive watch ran per item: baseline capture + completion poll.
	assert.Equal(t, 2, f.taskCalls("testid-qderivel1"))
}

// TestProgressCallbackCounts: PUT progress reaches the event stream
// through the documented throttle — coarse steps plus a final 100%
// event.
func TestProgressCallbackCounts(t *testing.T) {
	// The throttle itself, precisely: 100-byte total, 30-byte step.
	old := progressMinStep
	progressMinStep = 30
	t.Cleanup(func() { progressMinStep = old })

	th := &progressThrottle{total: 100}
	assert.False(t, th.emit(10), "below the step no event fires")
	assert.True(t, th.emit(35), "crossing the step fires")
	assert.False(t, th.emit(50))
	assert.True(t, th.emit(70))
	assert.False(t, th.emit(30), "a byte regression (fresh PUT attempt) resets, no event")
	assert.True(t, th.emit(95), "after the reset the watermark starts from zero")
	assert.False(t, th.emit(100), "a small advance under the step stays quiet")

	// End to end: a run must end with a full-size progress event.
	paths := writeUploadFiles(t, strings.Repeat("progress", 40)) // 320 bytes
	f := newFakeIAS3(t)
	r, _ := newTestUploadRunner(t, f, uploadTestLimits())
	tgts := uploadTargets(paths, "testid-progress1")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Done)

	var last Event
	count := 0
	for _, ev := range got {
		if ev.Kind != EventProgress {
			continue
		}
		count++
		last = ev
		assert.GreaterOrEqual(t, ev.Sent, int64(0), "progress bytes must never go backwards in events")
	}
	require.GreaterOrEqual(t, count, 1, "at least the final progress event must fire")
	require.Equal(t, int64(320), last.Total, "progress Total must be the file size")
	assert.Equal(t, last.Total, last.Sent, "the run must end with a 100% progress event")
}

// TestDeriveWatchPerItemAfterLast: a derive failure is an item-level
// failure — the files stay done, the summary counts one extra failure,
// and the watch runs once per item, after the last file.
func TestDeriveWatchPerItemAfterLast(t *testing.T) {
	paths := writeUploadFiles(t, "d1", "d2")
	f := newFakeIAS3(t)
	f.taskFn = func(identifier string, nth int) string {
		if nth == 1 {
			// Baseline capture: an old finished derive.
			return `{"success":true,"value":{"catalog":[],"history":[{"task_id":10,"cmd":"derive.php"}]}}`
		}
		// Watch: a derive stuck in the error state (catalog task).
		return `{"success":true,"value":{"catalog":[{"task_id":77,"cmd":"derive.php","wait_admin":2}],"history":[]}}`
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-derivewt1")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err) // per-file failures are journaled, never fatal
	assert.Equal(t, 2, sum.Done, "files stay done — derive is item-level")
	assert.Equal(t, 1, sum.Failed, "the item-level derive failure counts once")
	for _, tgt := range tgts {
		assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgt).State)
	}
	found := false
	for _, ev := range got {
		if ev.Kind == EventState && ev.Target == "testid-derivewt1" && ev.State == journal.StateFailed {
			found = true
			assert.Contains(t, ev.ErrMsg, "деривация")
		}
	}
	assert.True(t, found, "expected an item-level failed event for the derive")
	// One watch per item: baseline (call 1) + the failing poll (call 2).
	assert.Equal(t, 2, f.taskCalls("testid-derivewt1"))
}

// TestCheckLimitOutageFailsFileWithoutPUT: a stably erroring check_limit
// endpoint fails the file after the poll cap — no PUT is ever admitted.
func TestCheckLimitOutageFailsFileWithoutPUT(t *testing.T) {
	old := ias3.CheckLimitPoll
	ias3.CheckLimitPoll = time.Millisecond // shrink the poll cadence
	t.Cleanup(func() { ias3.CheckLimitPoll = old })

	paths := writeUploadFiles(t, "preflight-me")
	f := newFakeIAS3(t)
	f.failChecks = true
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-prefligh1")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err) // per-file failures are journaled, never fatal
	assert.Equal(t, 0, f.puts("file1.bin"), "a stalled-queue preflight must never admit a PUT")
	assert.GreaterOrEqual(t, f.checkCount(), 1, "the preflight must actually poll the endpoint")
	assert.Equal(t, 1, sum.Failed)
	assert.Equal(t, 0, sum.Done)
	rec := mustUploadRecord(t, st, tgts[0])
	assert.Equal(t, journal.StateFailed, rec.State)
	found := false
	for _, ev := range got {
		if ev.State == journal.StateFailed && strings.Contains(ev.ErrMsg, "лимит очереди") {
			found = true
		}
	}
	assert.True(t, found, "expected the Russian stalled-queue failure message")
}

// TestDeriveWatchNotArmedWhenLastFileFails: the derive-queueing PUT
// never happened, so the item must not watch derive — one Tasks API
// call (the baseline before the first PUT's sibling) and no item-level
// failure event.
func TestDeriveWatchNotArmedWhenLastFileFails(t *testing.T) {
	paths := writeUploadFiles(t, "d1", "d2-last")
	f := newFakeIAS3(t)
	f.putFn = func(identifier, key string, nth int) int {
		if key == "file2.bin" && nth == 1 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-derivenot1")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Done)
	assert.Equal(t, 1, sum.Failed)
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[0]).State)
	assert.Equal(t, journal.StateFailed, mustUploadRecord(t, st, tgts[1]).State)
	assert.Equal(t, 1, f.taskCalls("testid-derivenot1"),
		"baseline only: the last PUT failed, so no derive was queued and no watch may run")
	for _, ev := range got {
		if ev.Kind == EventState && ev.Target == "testid-derivenot1" && ev.State == journal.StateFailed {
			t.Fatalf("item-level failure event must not fire without a queued derive: %+v", ev)
		}
	}
}

// TestDeriveWatchNotArmedWhenLastFileSkipped: a skip-matched last file
// never PUTs, so no derive is queued and the item must not watch.
func TestDeriveWatchNotArmedWhenLastFileSkipped(t *testing.T) {
	paths := writeUploadFiles(t, "d1", "d2-skipme")
	sum5 := md5.Sum([]byte("d2-skipme"))
	f := newFakeIAS3(t)
	f.metaFiles["testid-derivenot2"] = []metaEntry{{name: "file2.bin", md5: hex.EncodeToString(sum5[:])}}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-derivenot2")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, true)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Done)
	assert.Equal(t, 1, sum.Skipped)
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[0]).State)
	assert.Equal(t, journal.StateSkipped, mustUploadRecord(t, st, tgts[1]).State)
	assert.Equal(t, 2, f.taskCalls("testid-derivenot2"),
		"baseline + the skip-guard probe only: the last file skipped, so no derive was queued and no watch may run")
	for _, ev := range got {
		if ev.Kind == EventState && ev.Target == "testid-derivenot2" && ev.State == journal.StateFailed {
			t.Fatalf("item-level failure event must not fire without a queued derive: %+v", ev)
		}
	}
}

// TestSkipGuardTasksActiveUploadsAnyway: a matching remote md5 alone
// does not justify a skip while the item has active catalog tasks
// (metadata may be stale during derive/ingest) — the file uploads.
func TestSkipGuardTasksActiveUploadsAnyway(t *testing.T) {
	paths := writeUploadFiles(t, "guard-me")
	sum5 := md5.Sum([]byte("guard-me"))
	f := newFakeIAS3(t)
	f.metaFiles["testid-skipguard1"] = []metaEntry{{name: "file1.bin", md5: hex.EncodeToString(sum5[:])}}
	f.taskFn = func(identifier string, nth int) string {
		return `{"success":true,"value":{"catalog":[{"task_id":5,"cmd":"derive.php","wait_admin":1}],"history":[]}}`
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-skipguard1")
	o := uploadOpts()
	o.QueueDerive = false // the guard is independent of derive watching
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, o, false, true)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Done)
	assert.Equal(t, 0, sum.Skipped, "an active-task item must not produce a skip")
	assert.Equal(t, 1, f.puts("file1.bin"), "the file must upload instead of skipping")
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[0]).State)
	noted := false
	for _, ev := range got {
		if ev.Kind == EventWait && strings.Contains(ev.ErrMsg, "активны задачи") {
			noted = true
		}
	}
	assert.True(t, noted, "expected a dim note that the skip was cancelled")
}

// TestSkipGuardTasksErrorFailsRetryable: an erroring Tasks API makes
// the skip undecidable — the file fails with the retryable Russian
// message and no PUT is spent.
func TestSkipGuardTasksErrorFailsRetryable(t *testing.T) {
	paths := writeUploadFiles(t, "guard-me")
	sum5 := md5.Sum([]byte("guard-me"))
	f := newFakeIAS3(t)
	f.metaFiles["testid-skipguard2"] = []metaEntry{{name: "file1.bin", md5: hex.EncodeToString(sum5[:])}}
	f.taskFn = func(identifier string, nth int) string {
		return `{"success":false,"error":"tasks are unavailable"}`
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-skipguard2")
	o := uploadOpts()
	o.QueueDerive = false
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, o, false, true)
	require.NoError(t, err)
	assert.Equal(t, 0, f.puts("file1.bin"), "an undecidable skip must not PUT")
	assert.Equal(t, 1, sum.Failed)
	assert.Equal(t, journal.StateFailed, mustUploadRecord(t, st, tgts[0]).State)
	found := false
	for _, ev := range got {
		if ev.State == journal.StateFailed && strings.Contains(ev.ErrMsg, "активные задачи") {
			found = true
		}
	}
	assert.True(t, found, "expected the Russian undecidable-skip failure message")
}

// TestDeriveWatchSurvivesVerifyFailure: a verify failure after a
// successful last PUT must not cancel the watch — the derive WAS queued
// server-side by that PUT.
func TestDeriveWatchSurvivesVerifyFailure(t *testing.T) {
	paths := writeUploadFiles(t, "verify-me")
	sum5 := md5.Sum([]byte("something-else"))
	f := newFakeIAS3(t)
	f.metaFiles["testid-derivewt2"] = []metaEntry{{name: "file1.bin", md5: hex.EncodeToString(sum5[:])}}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-derivewt2")
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), true, false)
	require.NoError(t, err)
	assert.Equal(t, 0, sum.Done)
	assert.Equal(t, 1, sum.Failed)
	assert.Equal(t, journal.StateFailed, mustUploadRecord(t, st, tgts[0]).State)
	// Baseline capture + the (successful) watch prove the watch ran
	// even though the file failed at verify.
	assert.Equal(t, 2, f.taskCalls("testid-derivewt2"))
}

// TestBadCredentialsPausesQueue: a credentials refusal journals the file
// failed, fires exactly one auth event, stops admitting new uploads and
// surfaces as the completion error.
func TestBadCredentialsPausesQueue(t *testing.T) {
	paths := writeUploadFiles(t, "secret-a", "secret-b")
	f := newFakeIAS3(t)
	f.putFn = func(identifier, key string, nth int) int {
		return http.StatusForbidden // 403 + InvalidAccessKeyId = ErrBadCredentials
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-badcreds1")
	got, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.Error(t, err, "an auth pause must surface as the completion error")
	assert.Contains(t, err.Error(), "авторизации")
	assert.Equal(t, 1, f.puts("file1.bin"), "the queue stops after the first credential refusal")
	assert.Equal(t, 0, f.puts("file2.bin"), "the second file must never start")

	authEvents := 0
	for _, ev := range got {
		if ev.Kind == EventAuth {
			authEvents++
		}
	}
	assert.Equal(t, 1, authEvents, "exactly one auth event")
	assert.Equal(t, 1, sum.Failed)
	rec := mustUploadRecord(t, st, tgts[0])
	assert.Equal(t, journal.StateFailed, rec.State)
	assert.Equal(t, journal.StateQueued, mustUploadRecord(t, st, tgts[1]).State)
}

// TestCancelMidUploadLeavesQueuedAndResumes: a context cancel while the
// PUT is in flight is an interruption, not a per-file failure — the
// record must stay queued (digests already persisted), so the exit-1
// message «часть задач осталась в журнале и продолжится» stays true and
// the next run's resume scan finishes the file.
func TestCancelMidUploadLeavesQueuedAndResumes(t *testing.T) {
	paths := writeUploadFiles(t, "interrupt-me")
	f := newFakeIAS3(t)
	f.blockPutUntilCancel = true
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-cancelput1")
	ctx, cancel := context.WithCancel(context.Background())
	canceler := make(chan struct{})
	go func() {
		defer close(canceler)
		assert.Eventually(t, func() bool { return f.puts("file1.bin") == 1 },
			2*time.Second, 2*time.Millisecond, "the PUT must start before the cancel")
		cancel()
	}()
	got, _, err := runUploadAll(t, ctx, r, tgts, uploadOpts(), false, false)
	<-canceler
	require.ErrorIs(t, err, context.Canceled, "a cancelled run surfaces as the completion error")

	rec := mustUploadRecord(t, st, tgts[0])
	require.Equal(t, journal.StateQueued, rec.State,
		"cancel mid-PUT must leave the record queued (resumable), not failed")
	require.NotEmpty(t, rec.MD5, "digests are persisted while queued, so resume never re-hashes")
	for _, ev := range got {
		if ev.Target == uploadRecordTarget(tgts[0]) && ev.State == journal.StateFailed {
			t.Fatalf("an interrupted PUT must not journal failed: %+v", ev)
		}
	}

	// The next run's resume scan picks the queued record up and finishes it.
	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	_, sum2, err := runUploadAll(t, context.Background(), r2, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[0]).State)
}

// TestCancelMidVerifyLeavesQueuedAndResumes: the preserved M-2 ruling —
// a cancel during the post-PUT verify leaves the record queued (the PUT
// succeeded and the digests are persisted), so the next run re-uploads
// idempotently and re-verifies to done; the file never becomes failed.
func TestCancelMidVerifyLeavesQueuedAndResumes(t *testing.T) {
	paths := writeUploadFiles(t, "verify-interrupt")
	f := newFakeIAS3(t)
	f.blockMetaUntilCancel = true
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgts := uploadTargets(paths, "testid-cancelver1")
	ctx, cancel := context.WithCancel(context.Background())
	canceler := make(chan struct{})
	go func() {
		defer close(canceler)
		assert.Eventually(t, func() bool { return f.metaCallsTotal() >= 1 },
			2*time.Second, 2*time.Millisecond, "verify must reach the metadata API (PUT already done)")
		cancel()
	}()
	_, _, err := runUploadAll(t, ctx, r, tgts, uploadOpts(), true /* verify */, false)
	<-canceler
	require.ErrorIs(t, err, context.Canceled)

	rec := mustUploadRecord(t, st, tgts[0])
	require.Equal(t, journal.StateQueued, rec.State,
		"cancel during verify must leave the record queued (M-2 ruling), not failed")
	require.NotEmpty(t, rec.MD5, "digests are persisted while queued")

	// Resume: healthy metadata with the matching digest — the run finishes.
	f.setBlockMetaUntilCancel(false)
	sum5 := md5.Sum([]byte("verify-interrupt"))
	f.metaFiles["testid-cancelver1"] = []metaEntry{{name: "file1.bin", md5: hex.EncodeToString(sum5[:])}}
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	_, sum2, err := runUploadAll(t, context.Background(), r2, tgts, uploadOpts(), true, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgts[0]).State)
}
