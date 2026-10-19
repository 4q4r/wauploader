package httpx

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/4q4r/wauploader/internal/version"
)

func newTestJar() (*cookiejar.Jar, error) {
	return cookiejar.New(nil)
}

// TestTransportForcesHTTP1 pins the transport posture: HTTP/1.1 on,
// HTTP/2 off, no implicit HTTP/2 upgrade attempts, no client-wide
// timeout (callers own deadlines via context), and the cookie jar
// wiring passes through.
func TestTransportForcesHTTP1(t *testing.T) {
	jar, err := newTestJar()
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	cl := NewClient(jar, "test-ua")
	if cl.HTTP == nil {
		t.Fatal("NewClient returned nil HTTP client")
	}
	if cl.HTTP.Jar != http.CookieJar(jar) {
		t.Error("cookie jar was not wired into the http.Client")
	}
	if cl.HTTP.Timeout != 0 {
		t.Errorf("client-wide Timeout = %v, want 0 (deadline ownership belongs to callers via ctx)", cl.HTTP.Timeout)
	}
	tr, ok := cl.HTTP.Transport.(*http.Transport)
	if ok && tr != cl.transport {
		t.Error("client.transport does not match the innermost transport")
	}
	tr = cl.transport
	if tr == nil {
		t.Fatal("NewClient did not retain the *http.Transport")
	}
	if !tr.Protocols.HTTP1() {
		t.Error("Protocols.HTTP1() = false, want true")
	}
	if tr.Protocols.HTTP2() {
		t.Error("Protocols.HTTP2() = true, want false (IA-S3 documents HTTP/1.1 only)")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true, want false")
	}
	clone := tr.Clone()
	if !clone.Protocols.HTTP1() || clone.Protocols.HTTP2() {
		t.Error("cloned transport lost the HTTP/1.1-only protocol pin")
	}
}

// TestUserAgentDefaultOverrideAndExplicitWins checks the global
// constraint that every request carries the descriptive User-Agent:
// empty NewClient userAgent falls back to version.UserAgent(), a
// config override is used verbatim, and an explicitly set per-request
// header is never clobbered.
func TestUserAgentDefaultOverrideAndExplicitWins(t *testing.T) {
	var seen atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Default: version.UserAgent().
	cl := NewClient(nil, "")
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.DoWithRetry(t.Context(), req, RetryOpts{Mode: RetryNone}); err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	if got := seen.Load().(string); got != version.UserAgent() {
		t.Errorf("default UA = %q, want %q", got, version.UserAgent())
	}

	// Config override.
	cl2 := NewClient(nil, "wauploader-test/9.9 (override)")
	req2, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := cl2.DoWithRetry(t.Context(), req2, RetryOpts{Mode: RetryNone}); err != nil {
		t.Fatalf("DoWithRetry override: %v", err)
	}
	if got := seen.Load().(string); got != "wauploader-test/9.9 (override)" {
		t.Errorf("override UA = %q, want %q", got, "wauploader-test/9.9 (override)")
	}

	// Explicit per-request header wins over the client default.
	req3, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req3.Header.Set("User-Agent", "explicit/1")
	if _, err := cl2.DoWithRetry(t.Context(), req3, RetryOpts{Mode: RetryNone}); err != nil {
		t.Fatalf("DoWithRetry explicit: %v", err)
	}
	if got := seen.Load().(string); got != "explicit/1" {
		t.Errorf("explicit UA = %q, want %q (must not be clobbered)", got, "explicit/1")
	}
}

// TestRedirectKeepsAuthOnArchiveOrg proves the IA-S3 307 contract: a
// redirect whose target host is archive.org re-attaches the original
// Authorization header. A dial override keeps the test fully offline —
// the request for http://archive.org/final is connected back to the
// local httptest server.
func TestRedirectKeepsAuthOnArchiveOrg(t *testing.T) {
	var finalAuth atomic.Value // string
	var finalHost atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "http://archive.org/final", http.StatusFound)
		default:
			finalAuth.Store(r.Header.Get("Authorization"))
			finalHost.Store(r.Host)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server addr: %v", err)
	}
	cl.HTTP.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
	req.Header.Set("Authorization", "LOW test-access:test-secret")
	resp, err := cl.DoWithRetry(t.Context(), req, RetryOpts{Mode: RetryNone})
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if got := finalAuth.Load().(string); got != "LOW test-access:test-secret" {
		t.Errorf("Authorization on archive.org hop = %q, want it preserved", got)
	}
	if got := finalHost.Load().(string); got != "archive.org" {
		t.Errorf("final Host = %q, want archive.org", got)
	}
}

// TestRedirectStripsAuthElsewhere proves the credential-leak guard:
// the Authorization header is stripped when the redirect target is not
// archive.org — including same-hostname different-port hops, which Go's
// default policy alone would keep.
func TestRedirectStripsAuthElsewhere(t *testing.T) {
	var finalAuth atomic.Value // string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/landed", http.StatusFound)
	}))
	defer start.Close()

	cl := NewClient(nil, "test-ua")
	req, _ := http.NewRequest(http.MethodGet, start.URL, nil)
	req.Header.Set("Authorization", "LOW test-access:test-secret")
	resp, err := cl.DoWithRetry(t.Context(), req, RetryOpts{Mode: RetryNone})
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if got := finalAuth.Load().(string); got != "" {
		t.Errorf("Authorization survived a non-archive.org redirect: %q, want stripped", got)
	}
}

// TestRedirectCapTen mirrors the net/http default: following stops
// with an error after the tenth hop.
func TestRedirectCapTen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := cl.DoWithRetry(t.Context(), req, RetryOpts{Mode: RetryNone})
	if err == nil {
		t.Fatal("redirect chain of unlimited hops did not error, want stop after 10")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("err = %v, want redirect-cap error", err)
	}
}
