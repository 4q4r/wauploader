// Test infrastructure for the command surface: a fake SPN2 server, a fake
// IA-S3 endpoint, a fake archive.org web host, a host-rewriting recording
// transport wired through the newHTTPClient seam, and runRoot which
// executes the real root command with captured streams and maps ExitError
// to a process exit code exactly like main.go does.
package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/stretchr/testify/require"
)

// --- configuration fixtures --------------------------------------------------

const testConfigFull = `
[auth]
s3_access = "testaccesskey11"
s3_secret = "testsecretvalue1"

[spn2]
auth_mode = "auto"

[limits]
save_per_minute = 100000
save_concurrency = 3
upload_concurrency = 2
per_host_concurrency = 3
job_wait = "20m"
`

const testConfigNoAuth = `
[limits]
save_per_minute = 100000
save_concurrency = 1
upload_concurrency = 1
per_host_concurrency = 1
job_wait = "20m"
`

// writeTestConfig writes a TOML fixture with 0600 permissions (the real
// config contract) and returns its path.
func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wauploader.toml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// --- runRoot: execute the real root command like main does -------------------

// cmdResult is one executed command: the mapped exit code, both captured
// streams and the raw Execute error.
type cmdResult struct {
	code   int
	out    string
	errOut string
	err    error
}

// syncBuffer is a mutex-guarded bytes.Buffer: the console contract is
// "safe iff the injected writers are" (Task 3 parked minor), and the
// runners write status lines from several worker goroutines. Production
// injects *os.File (concurrency-safe); tests inject syncBuffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runRoot builds the real root command, sets the args, captures both
// streams and maps the outcome to an exit code the way main.go does
// (ExitError.Code wins; plain errors are 1; the Russian text prints through
// the console to stderr).
func runRoot(t *testing.T, args ...string) cmdResult {
	t.Helper()
	var cfgPath, journalPath string
	var jsonOut, noColor, dryRun bool
	root := NewRootCommand(&cfgPath, &journalPath, &jsonOut, &noColor, &dryRun)
	var out, errOut syncBuffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()

	res := cmdResult{out: out.String(), errOut: errOut.String(), err: err}
	var ee *ExitError
	switch {
	case err == nil:
		res.code = ExitOK
	case errors.As(err, &ee):
		res.code = ee.Code
		if ee.Err != nil {
			con := console.New(&errOut, &errOut, false, true)
			con.Errorf("%v", ee.Err)
			res.errOut = errOut.String()
		}
	default:
		res.code = ExitSomeFailed
		con := console.New(&errOut, &errOut, false, true)
		con.Errorf("%v", err)
		res.errOut = errOut.String()
	}
	return res
}

// --- fakes -------------------------------------------------------------------

// fakeBackend spins up three httptest servers (SPN2 web.archive.org
// surface, the s3.us.archive.org endpoint, the archive.org web host) and
// repoints the newHTTPClient seam at a recording transport that rewrites
// the documented hostnames onto them.
type fakeBackend struct {
	spN2Srv *httptest.Server
	s3Srv   *httptest.Server
	webSrv  *httptest.Server
	tr      *recordingTransport

	mu            sync.Mutex
	saves         int
	puts          int
	putPaths      []string
	nextJob       int
	jobTargets    map[string]string
	failTargets   map[string]bool
	userStatusBod string
}

// recordingTransport rewrites the three documented hosts onto the fakes and
// logs every request (method host path) so tests can assert that dry-run
// issues zero requests.
type recordingTransport struct {
	inner         http.RoundTripper
	spN2, s3, web *url.URL
	mu            sync.Mutex
	log           []string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.log = append(rt.log, req.Method+" "+req.URL.Host+req.URL.Path)
	rt.mu.Unlock()
	clone := req.Clone(req.Context())
	switch req.URL.Host {
	case "web.archive.org":
		clone.URL.Scheme, clone.URL.Host, clone.Host = rt.spN2.Scheme, rt.spN2.Host, rt.spN2.Host
	case "s3.us.archive.org":
		clone.URL.Scheme, clone.URL.Host, clone.Host = rt.s3.Scheme, rt.s3.Host, rt.s3.Host
	case "archive.org":
		clone.URL.Scheme, clone.URL.Host, clone.Host = rt.web.Scheme, rt.web.Host, rt.web.Host
	}
	return rt.inner.RoundTrip(clone)
}

// requests returns a snapshot of the request log.
func (fb *fakeBackend) requests() []string {
	fb.tr.mu.Lock()
	defer fb.tr.mu.Unlock()
	return append([]string(nil), fb.tr.log...)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// newFakeBackend starts the fakes and installs the transport seam override
// for the duration of the test.
func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{
		jobTargets:    map[string]string{},
		failTargets:   map[string]bool{},
		userStatusBod: `{"available":2,"processing":0}`,
	}

	spN2Mux := http.NewServeMux()
	spN2Mux.HandleFunc("POST /save", fb.handleSave)
	spN2Mux.HandleFunc("GET /save/status/user", fb.handleUser)
	spN2Mux.HandleFunc("GET /save/status/system", fb.handleSystem)
	spN2Mux.HandleFunc("/save/status/", fb.handleStatus)
	fb.spN2Srv = httptest.NewServer(spN2Mux)
	t.Cleanup(fb.spN2Srv.Close)

	s3Mux := http.NewServeMux()
	s3Mux.HandleFunc("/", fb.handleS3)
	fb.s3Srv = httptest.NewServer(s3Mux)
	t.Cleanup(fb.s3Srv.Close)

	webMux := http.NewServeMux()
	webMux.HandleFunc("/metadata/", fb.handleMetadata)
	webMux.HandleFunc("/services/tasks.php", fb.handleTasks)
	fb.webSrv = httptest.NewServer(webMux)
	t.Cleanup(fb.webSrv.Close)

	cl := httpx.NewClient(nil, "wauploader-tests")
	fb.tr = &recordingTransport{
		inner: cl.HTTP.Transport,
		spN2:  mustURL(t, fb.spN2Srv.URL),
		s3:    mustURL(t, fb.s3Srv.URL),
		web:   mustURL(t, fb.webSrv.URL),
	}
	cl.HTTP.Transport = fb.tr

	old := newHTTPClient
	newHTTPClient = func() *httpx.Client { return cl }
	t.Cleanup(func() { newHTTPClient = old })
	return fb
}

// failTarget makes the fake answer the target's status polling with a
// terminal job-state error (error:blocked-url).
func (fb *fakeBackend) failTarget(target string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.failTargets[target] = true
}

// setUserStatus replaces the canned /save/status/user body.
func (fb *fakeBackend) setUserStatus(body string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.userStatusBod = body
}

func (fb *fakeBackend) handleSave(w http.ResponseWriter, r *http.Request) {
	target := r.FormValue("url")
	fb.mu.Lock()
	fb.saves++
	fb.nextJob++
	job := fmt.Sprintf("job-%d", fb.nextJob)
	fb.jobTargets[job] = target
	fb.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fmt.Sprintf(`{"job_id":%q,"status":"pending"}`, job))
}

func (fb *fakeBackend) handleStatus(w http.ResponseWriter, r *http.Request) {
	job := strings.TrimPrefix(r.URL.Path, "/save/status/")
	fb.mu.Lock()
	target, ok := fb.jobTargets[job]
	failed := ok && fb.failTargets[target]
	fb.mu.Unlock()
	if !ok {
		target = "https://example.com/unknown"
	}
	w.Header().Set("Content-Type", "application/json")
	if failed {
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"status":"error","job_id":%q,"original_url":%q,"status_ext":"error:blocked-url","message":"URL в блок-листе Wayback"}`,
			job, target))
		return
	}
	_, _ = io.WriteString(w, fmt.Sprintf(
		`{"status":"success","job_id":%q,"original_url":%q,"timestamp":"20260101010101","duration_sec":5}`,
		job, target))
}

func (fb *fakeBackend) handleUser(w http.ResponseWriter, _ *http.Request) {
	fb.mu.Lock()
	body := fb.userStatusBod
	fb.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func (fb *fakeBackend) handleSystem(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (fb *fakeBackend) handleS3(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("check_limit") == "1" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"bucket":"bucket","accesskey":"access","over_limit":0}`)
		return
	}
	if r.Method == http.MethodPut {
		fb.mu.Lock()
		fb.puts++
		fb.putPaths = append(fb.putPaths, r.URL.Path)
		fb.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	http.NotFound(w, r)
}

func (fb *fakeBackend) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"files":[]}`)
}

func (fb *fakeBackend) handleTasks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"value":{"catalog":[],"history":[]}}`)
}

// --- shared assertions -------------------------------------------------------

// journalFileAbsent asserts the dry-run purity contract: the journal file
// was never created.
func journalFileAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.True(t, errors.Is(err, fs.ErrNotExist), "journal file must not exist, stat err: %v", err)
}

// nonEmptyLines splits captured output into non-empty lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// decodeJSONObjects asserts every captured line is one JSON object and
// returns the decoded maps.
func decodeJSONObjects(t *testing.T, out string) []map[string]any {
	t.Helper()
	lines := nonEmptyLines(out)
	require.NotEmpty(t, out, "JSON mode must produce output")
	maps := make([]map[string]any, 0, len(lines))
	for _, ln := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(ln), &m), "every line must be a JSON object: %q", ln)
		require.NotContains(t, ln, "✓", "JSON output must not carry console glyphs")
		maps = append(maps, m)
	}
	return maps
}
