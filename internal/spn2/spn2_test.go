package spn2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/httpx"
)

// overrideBaseURL swaps the package-level endpoint root for the test
// duration and registers the restore via t.Cleanup (mirrors the T5
// auth override pattern). Tests in this package never run in parallel,
// so mutating the unexported var is race-free under -race.
func overrideBaseURL(t *testing.T, value string) {
	t.Helper()
	old := baseURL
	baseURL = value
	t.Cleanup(func() { baseURL = old })
}

// loadFixture reads a doc-verbatim payload from testdata/.
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// recordedRequest captures one request seen by the fake SPN2 server.
type recordedRequest struct {
	method string
	uri    string
	query  url.Values
	form   url.Values
	header http.Header
}

// fakeSPN2 is an httptest server wired as the SPN2 base URL. It records
// every request in arrival order. Accessors take the mutex, so tests may
// read after any number of responses without racing the server goroutine.
type fakeSPN2 struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (f *fakeSPN2) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeSPN2) at(i int) recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.requests) {
		return recordedRequest{}
	}
	return f.requests[i]
}

// newFake starts the fake SPN2 server; respond is called for every
// request after it has been recorded.
func newFake(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *fakeSPN2 {
	t.Helper()
	fake := &fakeSPN2{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{
			method: r.Method,
			uri:    r.URL.RequestURI(),
			query:  r.URL.Query(),
			header: r.Header.Clone(),
		}
		if err := r.ParseForm(); err == nil {
			form := url.Values{}
			for k := range r.Form {
				form.Set(k, r.Form.Get(k))
			}
			rec.form = form
		}
		fake.mu.Lock()
		fake.requests = append(fake.requests, rec)
		fake.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	overrideBaseURL(t, srv.URL+"/save")
	return fake
}

// newTestClient builds a Client whose HTTP traffic never leaves the
// overridden baseURL.
func newTestClient(creds auth.Credentials) *Client {
	return NewClient(httpx.NewClient(nil, "wauploader-test"), creds)
}

func TestSubmitSuccess(t *testing.T) {
	body := loadFixture(t, "submit_success.json")
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	got, err := c.Submit(context.Background(), "http://brewster.kahle.org/", Options{
		CaptureAll:        true,
		JSBehaviorTimeout: 10,
	})
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}
	want := SubmitResult{JobID: "ac58789b-f3ca-48d0-9ea6-1d1225e98695"}
	if got != want {
		t.Errorf("SubmitResult = %+v, want %+v", got, want)
	}

	req := fake.at(0)
	if req.method != http.MethodPost {
		t.Errorf("request method = %q, want POST", req.method)
	}
	if req.uri != "/save" {
		t.Errorf("request URI = %q, want /save", req.uri)
	}
	if got := req.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept header = %q, want application/json", got)
	}
	if got := req.header.Get("Content-Type"); got != "application/x-www-form-urlencoded;charset=UTF-8" {
		t.Errorf("Content-Type header = %q, want application/x-www-form-urlencoded;charset=UTF-8", got)
	}
	if got := req.header.Get("Authorization"); got != "LOW testaccess:testsecret" {
		t.Errorf("Authorization header = %q, want LOW testaccess:testsecret", got)
	}
	if got := req.form.Get("url"); got != "http://brewster.kahle.org/" {
		t.Errorf("form url = %q, want http://brewster.kahle.org/", got)
	}
	if got := req.form.Get("capture_all"); got != "1" {
		t.Errorf("form capture_all = %q, want 1", got)
	}
	if got := req.form.Get("js_behavior_timeout"); got != "10" {
		t.Errorf("form js_behavior_timeout = %q, want 10", got)
	}
	if _, ok := req.form["capture_outlinks"]; ok {
		t.Errorf("form capture_outlinks sent for a false option; false options must be omitted")
	}
}

func TestSubmitRefusalEnvelope(t *testing.T) {
	body := loadFixture(t, "submit_refusal.json")
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 refusal envelope WITHOUT job_id (Review Focus #1):
		// must become a typed error immediately, never a poll loop.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	got, err := c.Submit(context.Background(), "https://nxdomain.fake.tld/", Options{})
	if err == nil {
		t.Fatalf("Submit returned %+v for a refusal envelope, want typed error", got)
	}
	var spnErr *Error
	if !errors.As(err, &spnErr) {
		t.Fatalf("error type = %T, want *spn2.Error", err)
	}
	if spnErr.Kind != KindTerminal {
		t.Errorf("error Kind = %v, want KindTerminal (invalid-host-resolution)", spnErr.Kind)
	}
	if spnErr.StatusExt != "error:invalid-host-resolution" {
		t.Errorf("StatusExt = %q, want error:invalid-host-resolution (raw wire form)", spnErr.StatusExt)
	}
	if spnErr.HTTP != 200 {
		t.Errorf("HTTP = %d, want 200 (refusal arrives as HTTP 200)", spnErr.HTTP)
	}
	if !strings.Contains(spnErr.Message, "Cannot resolve host nxdomain.fake.tld.") {
		t.Errorf("error message %q must carry the server message", spnErr.Message)
	}
	if fake.count() != 1 {
		t.Errorf("request count = %d, want 1 (refusal must end the flow: no resend, no poll loop)", fake.count())
	}
}

func TestSubmitDedupMessage(t *testing.T) {
	body := loadFixture(t, "submit_dedup.json")
	newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	got, err := c.Submit(context.Background(), "https://example.com/", Options{})
	if err != nil {
		t.Fatalf("Submit returned error for a dedup payload: %v", err)
	}
	want := SubmitResult{
		Deduped: true,
		Message: "The same snapshot had been made 3 minutes ago. You can make new capture of this URL after 2 hours.",
	}
	if got != want {
		t.Errorf("SubmitResult = %+v, want %+v", got, want)
	}
}

func TestSubmitNoStatusRetry(t *testing.T) {
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		// HTML 500 error page: POST /save must never be resent on a
		// status response (r1 F7.7: the capture may already exist).
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>The server encountered an internal error</html>"))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	_, err := c.Submit(context.Background(), "https://example.com/", Options{})
	if err == nil {
		t.Fatal("Submit returned success for an HTML 500 error page")
	}
	if fake.count() != 1 {
		t.Errorf("request count = %d, want 1 (POST /save must not retry on status responses)", fake.count())
	}
	var spnErr *Error
	if !errors.As(err, &spnErr) {
		t.Fatalf("error type = %T, want *spn2.Error", err)
	}
	if spnErr.Kind != KindRetryable {
		t.Errorf("error Kind = %v, want KindRetryable (unparseable response is fail-open)", spnErr.Kind)
	}
	if spnErr.HTTP != 500 {
		t.Errorf("error HTTP = %d, want 500", spnErr.HTTP)
	}
}

func TestSubmit429Propagates(t *testing.T) {
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		// Per-minute cap reached: the answer must reach the caller as a
		// typed error carrying HTTP 429 (pacing decisions belong to the
		// runner), and POST /save must never be resent on a status.
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("429 Too Many Requests"))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	_, err := c.Submit(context.Background(), "https://example.com/", Options{})
	if err == nil {
		t.Fatal("Submit returned success for a 429 response")
	}
	var spnErr *Error
	if !errors.As(err, &spnErr) {
		t.Fatalf("error type = %T, want *spn2.Error", err)
	}
	if spnErr.HTTP != http.StatusTooManyRequests {
		t.Errorf("error HTTP = %d, want 429", spnErr.HTTP)
	}
	if spnErr.Kind != KindRetryable {
		t.Errorf("error Kind = %v, want KindRetryable (non-JSON 429 body is fail-open)", spnErr.Kind)
	}
	if fake.count() != 1 {
		t.Errorf("request count = %d, want 1 (RetryConnectOnly never resends on a status response)", fake.count())
	}
}

func TestSubmitCookieAuth(t *testing.T) {
	body := loadFixture(t, "submit_success.json")
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c := newTestClient(auth.Credentials{CookieUser: "user%40example.org", CookieSig: "sigvalue"})

	if _, err := c.Submit(context.Background(), "https://example.com/", Options{}); err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}
	req := fake.at(0)
	if got := req.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want empty for cookie credentials", got)
	}
	cookie := req.header.Get("Cookie")
	if !strings.Contains(cookie, "logged-in-user=user%40example.org") {
		t.Errorf("Cookie header %q must contain logged-in-user", cookie)
	}
	if !strings.Contains(cookie, "logged-in-sig=sigvalue") {
		t.Errorf("Cookie header %q must contain logged-in-sig", cookie)
	}
}

func TestOptionsEncodeTrue1FalseOmitted(t *testing.T) {
	o := Options{
		CaptureAll:           true,
		CaptureOutlinks:      false,
		CaptureScreenshot:    true,
		DelayWBAvailability:  false,
		ForceGet:             true,
		SkipFirstArchive:     false,
		OutlinksAvailability: true,
		EmailResult:          false,
		IfNotArchivedWithin:  "3d 5h 20m",
		JSBehaviorTimeout:    30,
		CaptureCookie:        "session=abc",
		UseUserAgent:         "",
		TargetUsername:       "user",
		TargetPassword:       "",
	}
	want := url.Values{
		"capture_all":            {"1"},
		"capture_screenshot":     {"1"},
		"force_get":              {"1"},
		"outlinks_availability":  {"1"},
		"if_not_archived_within": {"3d 5h 20m"},
		"js_behavior_timeout":    {"30"},
		"capture_cookie":         {"session=abc"},
		"target_username":        {"user"},
	}
	if got := o.Encode(); !reflect.DeepEqual(got, want) {
		t.Errorf("Encode() = %v, want %v", got, want)
	}
}

func TestOptionsJSZeroStillSent(t *testing.T) {
	// js_behavior_timeout=0 disables JS behaviors server-side, so the
	// zero value must be sent explicitly, never omitted.
	want := url.Values{"js_behavior_timeout": {"0"}}
	if got := (Options{}).Encode(); !reflect.DeepEqual(got, want) {
		t.Errorf("Encode() = %v, want %v", got, want)
	}
}

func TestStatusPayloadShapes(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		check   func(t *testing.T, st Status)
	}{
		{
			name:    "success with outlinks map and screenshot",
			fixture: "status_success_outlinks_map.json",
			check: func(t *testing.T, st Status) {
				if st.State != "success" {
					t.Errorf("State = %q, want success", st.State)
				}
				if st.JobID != "ac58789b-f3ca-48d0-9ea6-1d1225e98695" {
					t.Errorf("JobID = %q", st.JobID)
				}
				if st.OriginalURL != "http://brewster.kahle.org/" {
					t.Errorf("OriginalURL = %q", st.OriginalURL)
				}
				if st.Timestamp != "20200101010101" {
					t.Errorf("Timestamp = %q", st.Timestamp)
				}
				if st.DurationSec != 12.3 {
					t.Errorf("DurationSec = %v, want 12.3", st.DurationSec)
				}
				if !reflect.DeepEqual(st.Resources, []string{"http://brewster.kahle.org/", "https://archive.org/images/logo.png"}) {
					t.Errorf("Resources = %v", st.Resources)
				}
				wantJobs := map[string]string{
					"https://archive.org/":      "spn2-0123456789abcdef0123456789abcdef12345678",
					"https://help.archive.org/": "spn2-ffffffffffffffffffffffffffffffffffffffff",
				}
				if !reflect.DeepEqual(st.OutlinkJobs, wantJobs) {
					t.Errorf("OutlinkJobs = %v, want %v", st.OutlinkJobs, wantJobs)
				}
				if st.Outlinks != nil {
					t.Errorf("Outlinks = %v, want nil for the map form", st.Outlinks)
				}
				if st.Screenshot != "http://web.archive.org/screenshot/http://brewster.kahle.org/" {
					t.Errorf("Screenshot = %q", st.Screenshot)
				}
				if !st.FirstArchive {
					t.Errorf("FirstArchive = false, want true")
				}
				if st.HTTPStatus != 200 {
					t.Errorf("HTTPStatus = %d, want 200", st.HTTPStatus)
				}
			},
		},
		{
			name:    "success with outlinks array",
			fixture: "status_success_outlinks_array.json",
			check: func(t *testing.T, st Status) {
				if st.State != "success" {
					t.Errorf("State = %q, want success", st.State)
				}
				want := []string{"https://example.com/about", "https://example.com/contact"}
				if !reflect.DeepEqual(st.Outlinks, want) {
					t.Errorf("Outlinks = %v, want %v", st.Outlinks, want)
				}
				if st.OutlinkJobs != nil {
					t.Errorf("OutlinkJobs = %v, want nil for the array form", st.OutlinkJobs)
				}
				if st.FirstArchive {
					t.Errorf("FirstArchive = true, want false")
				}
			},
		},
		{
			name:    "success with outlinks_availability timestamps",
			fixture: "status_outlinks_availability.json",
			check: func(t *testing.T, st Status) {
				want := map[string]string{
					"https://example.com/about": "20251231235959",
					"https://example.com/empty": "",
				}
				if !reflect.DeepEqual(st.OutlinkTimestamps, want) {
					t.Errorf("OutlinkTimestamps = %v, want %v (null timestamp maps to empty string)", st.OutlinkTimestamps, want)
				}
			},
		},
		{
			name:    "pending with download_size",
			fixture: "status_pending.json",
			check: func(t *testing.T, st Status) {
				if st.State != "pending" {
					t.Errorf("State = %q, want pending", st.State)
				}
				if st.DownloadSize != 1048576 {
					t.Errorf("DownloadSize = %d, want 1048576", st.DownloadSize)
				}
				if st.OriginalURL != "https://example.com/big-file.zip" {
					t.Errorf("OriginalURL = %q, want the pending payload url field", st.OriginalURL)
				}
			},
		},
		{
			name:    "error state keeps status_ext and exception",
			fixture: "status_error.json",
			check: func(t *testing.T, st Status) {
				if st.State != "error" {
					t.Errorf("State = %q, want error", st.State)
				}
				if st.StatusExt != "error:invalid-host-resolution" {
					t.Errorf("StatusExt = %q", st.StatusExt)
				}
				if st.Message != "Cannot resolve host nxdomain.fake.tld." {
					t.Errorf("Message = %q", st.Message)
				}
				if st.Exception != "requests.exceptions.ConnectionError: nxdomain.fake.tld" {
					t.Errorf("Exception = %q", st.Exception)
				}
				if st.JobID != "dd1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
					t.Errorf("JobID = %q", st.JobID)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := loadFixture(t, tc.fixture)
			fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

			st, err := c.Status(context.Background(), "ac58789b-f3ca-48d0-9ea6-1d1225e98695")
			if err != nil {
				t.Fatalf("Status returned error: %v", err)
			}
			req := fake.at(0)
			if req.method != http.MethodGet {
				t.Errorf("request method = %q, want GET", req.method)
			}
			if !strings.HasPrefix(req.uri, "/save/status/") {
				t.Errorf("request URI = %q, want /save/status/<job_id>", req.uri)
			}
			if got := req.header.Get("Accept"); got != "application/json" {
				t.Errorf("Accept header = %q, want application/json", got)
			}
			tc.check(t, st)
		})
	}
}

func TestStatusBatchAndOutlinks(t *testing.T) {
	batch := `[
  {"status":"pending","job_id":"job-1","url":"https://example.com/a"},
  {"status":"success","job_id":"job-2","original_url":"https://example.com/b","timestamp":"20260101040404","duration_sec":1.5,"resources":["https://example.com/b"],"outlinks":["https://example.com/about"]}
]`
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(batch))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	sts, err := c.StatusBatch(context.Background(), []string{"job-1", "job-2"})
	if err != nil {
		t.Fatalf("StatusBatch returned error: %v", err)
	}
	if len(sts) != 2 {
		t.Fatalf("StatusBatch len = %d, want 2", len(sts))
	}
	if sts[0].State != "pending" || sts[0].JobID != "job-1" {
		t.Errorf("sts[0] = %+v, want pending job-1", sts[0])
	}
	if sts[0].OriginalURL != "https://example.com/a" {
		t.Errorf("sts[0].OriginalURL = %q, want the url field", sts[0].OriginalURL)
	}
	if sts[1].State != "success" || len(sts[1].Outlinks) != 1 {
		t.Errorf("sts[1] = %+v, want success with one outlink", sts[1])
	}
	req := fake.at(fake.count() - 1)
	if req.method != http.MethodPost {
		t.Errorf("batch request method = %q, want POST", req.method)
	}
	if got := req.form.Get("job_ids"); got != "job-1,job-2" {
		t.Errorf("form job_ids = %q, want comma-separated job-1,job-2", got)
	}

	sts, err = c.StatusOutlinks(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("StatusOutlinks returned error: %v", err)
	}
	if len(sts) != 2 {
		t.Fatalf("StatusOutlinks len = %d, want 2", len(sts))
	}
	req = fake.at(fake.count() - 1)
	if req.method != http.MethodPost {
		t.Errorf("outlinks request method = %q, want POST", req.method)
	}
	if got := req.form.Get("job_id_outlinks"); got != "parent-1" {
		t.Errorf("form job_id_outlinks = %q, want parent-1", got)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		code string
		kind Kind
	}{
		// Retryable (11 documented codes).
		{"error:cannot-fetch", KindRetryable},
		{"error:proxy-error", KindRetryable},
		{"error:protocol-error", KindRetryable},
		{"error:read-timeout", KindRetryable},
		{"error:browsing-timeout", KindRetryable},
		{"error:celery", KindRetryable},
		{"error:job-failed", KindRetryable},
		{"error:internal-server-error", KindRetryable},
		{"error:no-browsers-available", KindRetryable},
		{"error:capture-location-error", KindRetryable},
		{"error:too-many-requests", KindRetryable},
		// Quota (5 documented codes, park until the window resets).
		{"error:too-many-daily-captures", KindQuota},
		{"error:user-session-limit", KindQuota},
		{"error:max-daily-bandwidth", KindQuota},
		{"error:max-daily-bandwidth-from-ip", KindQuota},
		{"error:max-daily-bandwidth-host", KindQuota},
		// Terminal (22 documented codes).
		{"error:blocked-url", KindTerminal},
		{"error:blocked", KindTerminal},
		{"error:blocked-client-ip", KindTerminal},
		{"error:invalid-url-syntax", KindTerminal},
		{"error:invalid-host-resolution", KindTerminal},
		{"error:not-found", KindTerminal},
		{"error:no-access", KindTerminal},
		{"error:unauthorized", KindTerminal},
		{"error:filesize-limit", KindTerminal},
		{"error:ftp-access-denied", KindTerminal},
		{"error:bandwidth-limit-exceeded", KindTerminal},
		{"error:bad-request", KindTerminal},
		{"error:http-version-not-supported", KindTerminal},
		{"error:method-not-allowed", KindTerminal},
		{"error:not-implemented", KindTerminal},
		{"error:network-authentication-required", KindTerminal},
		{"error:gateway-timeout", KindTerminal},
		{"error:bad-gateway", KindTerminal},
		{"error:service-unavailable", KindTerminal},
		{"error:invalid-server-response", KindTerminal},
		{"error:soft-time-limit-exceeded", KindTerminal},
		{"error:too-many-redirects", KindTerminal},
	}
	for _, tc := range cases {
		kind, known := classifyStatusExt(tc.code)
		if !known {
			t.Errorf("classifyStatusExt(%q): not classified, want a documented code", tc.code)
			continue
		}
		if kind != tc.kind {
			t.Errorf("classifyStatusExt(%q) = %v, want %v", tc.code, kind, tc.kind)
		}
	}

	// Normalization: bare code without the error: prefix.
	if kind, known := classifyStatusExt("invalid-host-resolution"); !known || kind != KindTerminal {
		t.Errorf("classifyStatusExt bare code = (%v, %v), want (KindTerminal, true)", kind, known)
	}

	// Unknown code: fail-open retryable with a warning.
	kind, known := classifyStatusExt("error:some-future-code")
	if known {
		t.Errorf("classifyStatusExt(unknown) known = true, want false")
	}
	if kind != KindRetryable {
		t.Errorf("classifyStatusExt(unknown) = %v, want KindRetryable (fail-open)", kind)
	}

	// Message-based classification for codes the doc omits.
	msgCases := []struct {
		msg  string
		kind Kind
	}{
		{"Sorry. This URL is in the Save Page Now service block list and cannot be captured.", KindTerminal},
		{"Sorry. We're currently facing some limitations when it comes to archiving this site.", KindTerminal},
		{"Please try again in ~10 min. Crawling this host is paused because they notified us that they are overloaded right now.", KindRetryable},
		{"Cannot start capture", KindRetryable},
	}
	for _, tc := range msgCases {
		kind, known := classifyMessage(tc.msg)
		if !known {
			t.Errorf("classifyMessage(%q): marker not recognized", tc.msg)
			continue
		}
		if kind != tc.kind {
			t.Errorf("classifyMessage(%q) = %v, want %v", tc.msg, kind, tc.kind)
		}
	}
	if _, known := classifyMessage("The same snapshot had been made 3 minutes ago."); known {
		t.Errorf("classifyMessage(dedup text) known = true, want false (no marker)")
	}
}

func TestErrorMessageRussian(t *testing.T) {
	err := &Error{Kind: KindTerminal, StatusExt: "error:invalid-host-resolution", Message: "Cannot resolve host nxdomain.fake.tld.", HTTP: 200}
	got := err.Error()
	if !strings.Contains(got, "сохранение") {
		t.Errorf("Error() = %q, want a Russian lead-in naming the failure", got)
	}
	if !strings.Contains(got, "error:invalid-host-resolution") {
		t.Errorf("Error() = %q, want the raw status_ext code", got)
	}
	if !strings.Contains(got, "Cannot resolve host nxdomain.fake.tld.") {
		t.Errorf("Error() = %q, want the server message", got)
	}
	if !strings.Contains(got, "200") {
		t.Errorf("Error() = %q, want the HTTP status", got)
	}
}

func TestParseDelayMessage(t *testing.T) {
	cases := []struct {
		msg  string
		want time.Duration
		ok   bool
	}{
		{"Your capture will begin in 42s.", 42 * time.Second, true},
		{"Your capture will begin in 20s", 20 * time.Second, true},
		{"Your capture will begin in 1 minute 30 seconds.", 90 * time.Second, true},
		{"Your capture will start in 2 hours.", 2 * time.Hour, true},
		{"The same snapshot had been made 3 minutes ago. You can make new capture of this URL after 2 hours.", 0, false},
		{"Your capture will begin in a few seconds.", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseDelayMessage(tc.msg)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseDelayMessage(%q) = (%v, %v), want (%v, %v)", tc.msg, got, ok, tc.want, tc.ok)
		}
	}
}

func TestUserStatusCacheBuster(t *testing.T) {
	body := loadFixture(t, "user_status.json")
	fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

	avail, processing, err := c.UserStatus(context.Background())
	if err != nil {
		t.Fatalf("UserStatus returned error: %v", err)
	}
	if avail != 12 || processing != 3 {
		t.Errorf("UserStatus = (%d, %d), want (12, 3)", avail, processing)
	}
	// Strictly advance the millisecond clock so the second buster is
	// guaranteed to differ from the first.
	time.Sleep(2 * time.Millisecond)
	if _, _, err := c.UserStatus(context.Background()); err != nil {
		t.Fatalf("second UserStatus returned error: %v", err)
	}

	first, second := fake.at(0), fake.at(1)
	for i, req := range []recordedRequest{first, second} {
		if req.method != http.MethodGet {
			t.Errorf("request %d method = %q, want GET", i, req.method)
		}
		if !strings.HasPrefix(req.uri, "/save/status/user") {
			t.Errorf("request %d URI = %q, want /save/status/user", i, req.uri)
		}
		raw := req.query.Get("_t")
		if raw == "" {
			t.Errorf("request %d has no ?_t= cache buster", i)
			continue
		}
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			t.Errorf("request %d _t = %q is not an epoch-ms integer: %v", i, raw, err)
		}
	}
	n1, _ := strconv.ParseInt(first.query.Get("_t"), 10, 64)
	n2, _ := strconv.ParseInt(second.query.Get("_t"), 10, 64)
	if n2 <= n1 {
		t.Errorf("cache buster did not change: first _t=%d, second _t=%d", n1, n2)
	}
}

func TestSystemStatusCritical502(t *testing.T) {
	t.Run("critical problem is an error", func(t *testing.T) {
		fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("Bad Gateway"))
		})
		c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

		ok, overloaded, err := c.SystemStatus(context.Background())
		if err == nil {
			t.Fatalf("SystemStatus on 502 = (ok=%v, overloaded=%v, nil error), want a critical error", ok, overloaded)
		}
		var spnErr *Error
		if !errors.As(err, &spnErr) {
			t.Fatalf("error type = %T, want *spn2.Error (the 502 must surface typed, not as a transport error)", err)
		}
		if spnErr.HTTP != http.StatusBadGateway {
			t.Errorf("error HTTP = %d, want 502", spnErr.HTTP)
		}
		if ok || overloaded {
			t.Errorf("ok = %v, overloaded = %v, want both false on error", ok, overloaded)
		}
		if !strings.Contains(err.Error(), "502") {
			t.Errorf("error %q must name the HTTP status 502", err)
		}
		if fake.count() != 1 {
			t.Errorf("request count = %d, want 1 (502 surfaces without a retry storm)", fake.count())
		}
	})

	t.Run("502 with a JSON body must not read as overloaded", func(t *testing.T) {
		newFake(t, func(w http.ResponseWriter, r *http.Request) {
			// A 502 is critical by HTTP status alone: even a body that
			// parses as "ok" must not become the overload notice.
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

		ok, overloaded, err := c.SystemStatus(context.Background())
		if err == nil {
			t.Fatalf("SystemStatus on 502 with a JSON body = (ok=%v, overloaded=%v, nil error), want a status-driven critical error", ok, overloaded)
		}
		var spnErr *Error
		if !errors.As(err, &spnErr) {
			t.Fatalf("error type = %T, want *spn2.Error", err)
		}
		if spnErr.HTTP != http.StatusBadGateway {
			t.Errorf("error HTTP = %d, want 502", spnErr.HTTP)
		}
		if ok || overloaded {
			t.Errorf("ok = %v, overloaded = %v, want both false: 502 is status-driven critical, not the overload notice", ok, overloaded)
		}
	})

	t.Run("ok", func(t *testing.T) {
		body := loadFixture(t, "system_ok.json")
		newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

		ok, overloaded, err := c.SystemStatus(context.Background())
		if err != nil {
			t.Fatalf("SystemStatus returned error: %v", err)
		}
		if !ok || overloaded {
			t.Errorf("SystemStatus = (ok=%v, overloaded=%v), want (true, false)", ok, overloaded)
		}
	})

	t.Run("overloaded", func(t *testing.T) {
		body := loadFixture(t, "system_overloaded.json")
		newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})

		ok, overloaded, err := c.SystemStatus(context.Background())
		if err != nil {
			t.Fatalf("SystemStatus returned error: %v", err)
		}
		if ok || !overloaded {
			t.Errorf("SystemStatus = (ok=%v, overloaded=%v), want (false, true)", ok, overloaded)
		}
	})
}

func TestEmptyInputGuards(t *testing.T) {
	c := newTestClient(auth.Credentials{Access: "testaccess", Secret: "testsecret"})
	ctx := context.Background()

	if _, err := c.Submit(ctx, "", Options{}); err == nil {
		t.Error("Submit with an empty URL must fail fast")
	}
	if _, err := c.Status(ctx, ""); err == nil {
		t.Error("Status with an empty job id must fail fast")
	}
	if _, err := c.StatusBatch(ctx, nil); err == nil {
		t.Error("StatusBatch with no job ids must fail fast")
	}
	if _, err := c.StatusOutlinks(ctx, ""); err == nil {
		t.Error("StatusOutlinks with an empty parent id must fail fast")
	}
}

// TestProbeNon2xxTypedError pins the review round-1 fix: the read-only
// probes (Status, StatusBatch, StatusOutlinks, UserStatus) must never
// return a parsed document with a nil error for a non-2xx transport
// status — a 401 must reach the runner's auth-pause path, not
// terminal-fail or spin. 401/403 are not retried by the idempotent
// ladder, so they surface as typed *spn2.Error; 429/5xx are ladder-
// retried and end in the budget-exceeded error, which still honors the
// never-a-zero-error-doc invariant (the runner classifies it fail-open
// retryable, identical net behavior to KindRetryable). Body content
// like a job's target http_status is unaffected: only the HTTP
// transport status gates here.
func TestProbeNon2xxTypedError(t *testing.T) {
	cases := []struct {
		code  int
		kind  Kind
		typed bool
	}{
		{http.StatusUnauthorized, KindTerminal, true},
		{http.StatusForbidden, KindTerminal, true},
		{http.StatusTooManyRequests, KindRetryable, false},
		{http.StatusInternalServerError, KindRetryable, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"status":"error","status_ext":"error:unauthorized"}`))
			})
			c := newTestClient(auth.Credentials{Access: "a", Secret: "b"})
			// Shrink the idempotent ladder so the retryable cases stay
			// fast while keeping the retry semantics.
			c.retry = httpx.RetryOpts{Mode: httpx.RetryIdempotent, MaxAttempts: 2, BaseWait: time.Millisecond, MaxWait: 2 * time.Millisecond}

			_, err := c.Status(context.Background(), "job-1")
			if err == nil {
				t.Fatalf("Status returned a document for HTTP %d", tc.code)
			}
			var spnErr *Error
			if tc.typed {
				if !errors.As(err, &spnErr) {
					t.Fatalf("error type = %T, want *spn2.Error", err)
				}
				if spnErr.HTTP != tc.code || spnErr.Kind != tc.kind {
					t.Errorf("error = HTTP %d kind %v, want HTTP %d kind %v", spnErr.HTTP, spnErr.Kind, tc.code, tc.kind)
				}
				if !strings.Contains(spnErr.Message, "SPN2 вернул HTTP") {
					t.Errorf("error Message = %q, want the Russian transport notice", spnErr.Message)
				}
			} else if !errors.Is(err, httpx.ErrRetryBudgetExceeded) {
				t.Fatalf("error = %v, want the retry budget sentinel", err)
			}

			if _, _, uerr := c.UserStatus(context.Background()); uerr == nil {
				t.Fatalf("UserStatus returned values for HTTP %d", tc.code)
			}
			if _, berr := c.StatusBatch(context.Background(), []string{"job-1"}); berr == nil {
				t.Fatalf("StatusBatch returned documents for HTTP %d", tc.code)
			}
			if _, oerr := c.StatusOutlinks(context.Background(), "job-1"); oerr == nil {
				t.Fatalf("StatusOutlinks returned documents for HTTP %d", tc.code)
			}
			_ = fake.count()
		})
	}
}

// TestProbeTransportErrorKinds pins the probeTransportError mapping and
// its bounded Russian message for every transport class, including the
// codes the retry ladder would otherwise consume.
func TestProbeTransportErrorKinds(t *testing.T) {
	cases := []struct {
		code int
		kind Kind
	}{
		{http.StatusUnauthorized, KindTerminal},
		{http.StatusForbidden, KindTerminal},
		{http.StatusTooManyRequests, KindRetryable},
		{http.StatusInternalServerError, KindRetryable},
		{http.StatusBadGateway, KindRetryable},
	}
	for _, tc := range cases {
		err := probeTransportError(tc.code, "статуса задания", []byte("boom body"))
		if err.Kind != tc.kind {
			t.Errorf("code %d: kind = %v, want %v", tc.code, err.Kind, tc.kind)
		}
		if err.HTTP != tc.code {
			t.Errorf("code %d: HTTP = %d", tc.code, err.HTTP)
		}
		if !strings.Contains(err.Message, "SPN2 вернул HTTP") || !strings.Contains(err.Message, "boom body") {
			t.Errorf("code %d: message = %q, want the Russian notice with the body snippet", tc.code, err.Message)
		}
		if len(err.Message) > 220 {
			t.Errorf("code %d: message %q exceeds the 200-rune body bound", tc.code, err.Message)
		}
	}
}
