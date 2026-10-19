package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastOpts returns retry options with millisecond-scale waits so
// sleep-based tests stay fast while still exercising the ladder.
func fastOpts(mode RetryMode, attempts int) RetryOpts {
	return RetryOpts{
		Mode:        mode,
		MaxAttempts: attempts,
		BaseWait:    2 * time.Millisecond,
		MaxWait:     20 * time.Millisecond,
	}
}

// alwaysStatus returns a server that answers every request with code.
func alwaysStatus(t *testing.T, code int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.WriteHeader(code)
	}))
}

// TestRetryOn502ThenSuccess: a 502 is a retryable status; the second
// attempt succeeds.
func TestRetryOn502ThenSuccess(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(nethttp.StatusBadGateway)
			return
		}
		w.WriteHeader(nethttp.StatusOK)
	}))
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	req, _ := nethttp.NewRequest(nethttp.MethodGet, srv.URL, nil)
	resp, err := cl.DoWithRetry(t.Context(), req, fastOpts(RetryIdempotent, 3))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != nethttp.StatusOK {
		t.Fatalf("status = %d, want 200 after retry", resp.StatusCode)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestRetryAfterCapped: Retry-After: 3600 must be capped to MaxWait —
// the whole call finishes in well under 5 seconds.
func TestRetryAfterCapped(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(nethttp.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(nethttp.StatusOK)
	}))
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	req, _ := nethttp.NewRequest(nethttp.MethodGet, srv.URL, nil)
	start := time.Now()
	resp, err := cl.DoWithRetry(t.Context(), req, RetryOpts{
		Mode:        RetryIdempotent,
		MaxAttempts: 3,
		BaseWait:    2 * time.Millisecond,
		MaxWait:     500 * time.Millisecond, // cap far below the 3600s demand
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want < 5s (Retry-After: 3600 must be capped to MaxWait)", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %v, want ≈ the 500ms cap, not a longer jitter wait", elapsed)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestRetryContextCancelDuringBackoff: cancelling the context mid-wait
// returns promptly (well under 100 ms) with an errors.Is-able
// ErrContextDeadline error.
func TestRetryContextCancelDuringBackoff(t *testing.T) {
	srv := alwaysStatus(t, nethttp.StatusBadGateway)
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cl := NewClient(nil, "test-ua")
	req, _ := nethttp.NewRequest(nethttp.MethodGet, srv.URL, nil)
	opts := RetryOpts{ // 10s backoff: the ladder is guaranteed to be sleeping
		Mode:        RetryIdempotent,
		MaxAttempts: 5,
		BaseWait:    10 * time.Second,
		MaxWait:     10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := cl.DoWithRetry(ctx, req, opts)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond) // first attempt done, ladder now inside its backoff sleep
	cancelAt := time.Now()
	cancel()
	err := <-errCh
	elapsed := time.Since(cancelAt)

	if !errors.Is(err, ErrContextDeadline) {
		t.Fatalf("err = %v, want errors.Is(err, ErrContextDeadline)", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want chained context.Canceled", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("returned %v after cancel, want < 100ms", elapsed)
	}
}

// TestRetryBudgetExceeded: when every attempt returns a retryable
// status, the final error wraps ErrRetryBudgetExceeded and exactly
// MaxAttempts requests were made.
func TestRetryBudgetExceeded(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		attempts.Add(1)
		w.WriteHeader(nethttp.StatusBadGateway)
	}))
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	req, _ := nethttp.NewRequest(nethttp.MethodGet, srv.URL, nil)
	_, err := cl.DoWithRetry(t.Context(), req, fastOpts(RetryIdempotent, 3))
	if !errors.Is(err, ErrRetryBudgetExceeded) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRetryBudgetExceeded)", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want exactly MaxAttempts (3)", got)
	}
}

// TestRetryConnectOnlyPostNotRetriedOn502 is the load-bearing SPN2
// contract (r1 F7.7): a POST body was already written when the server
// answers 502, so RetryConnectOnly must NOT resend — the ambiguity of
// whether a capture was created is resolved above this layer.
func TestRetryConnectOnlyPostNotRetriedOn502(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		attempts.Add(1)
		w.WriteHeader(nethttp.StatusBadGateway)
	}))
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	body, _ := nethttp.NewRequest(nethttp.MethodPost, srv.URL, strings.NewReader("url=https://example.com"))
	resp, err := cl.DoWithRetry(t.Context(), body, fastOpts(RetryConnectOnly, 5))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (POST on 502 must never be resent)", got)
	}
}

// TestRetryConnectOnlyRetriesConnectPhase: a dial failure means nothing
// was written, so RetryConnectOnly resends until the budget runs out.
// The dead port is a real listener that was closed — deterministic
// connection-refused, no real network.
func TestRetryConnectOnlyRetriesConnectPhase(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	dead.Close() // port now refuses connections

	cl := NewClient(nil, "test-ua")
	req, _ := nethttp.NewRequest(nethttp.MethodPost, "http://"+deadAddr+"/save", strings.NewReader("url=https://example.com"))
	_, err = cl.DoWithRetry(t.Context(), req, fastOpts(RetryConnectOnly, 3))
	if !errors.Is(err, ErrRetryBudgetExceeded) {
		t.Fatalf("err = %v, want ErrRetryBudgetExceeded after connect-phase retries", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Errorf("err = %v, want wrapped *url.Error from the final attempt", err)
	}
}

// TestRetryIdempotentRejectsNonReplayableBody: fail loud at call time —
// an idempotent retry with a body that cannot be rewound would either
// silently lose retries or resend garbage.
func TestRetryIdempotentRejectsNonReplayableBody(t *testing.T) {
	srv := alwaysStatus(t, nethttp.StatusOK)
	defer srv.Close()

	cl := NewClient(nil, "test-ua")
	req, _ := nethttp.NewRequest(nethttp.MethodGet, srv.URL, nil)
	req.Body = io.NopCloser(strings.NewReader("non-replayable"))
	req.GetBody = nil // strip what NewRequest would have set
	if _, err := cl.DoWithRetry(t.Context(), req, fastOpts(RetryIdempotent, 3)); err == nil {
		t.Fatal("DoWithRetry accepted a non-replayable body under RetryIdempotent, want upfront error")
	}
}

// TestRetryAfterHeaderParsing covers the wire forms of Retry-After:
// delay-seconds (including overflow-sized values) and HTTP-date, plus
// the cap and the ignore-unparseable fallback.
func TestRetryAfterHeaderParsing(t *testing.T) {
	now := time.Now()
	in1h := now.Add(time.Hour).UTC().Format(nethttp.TimeFormat)
	cases := []struct {
		name    string
		value   string
		cap     time.Duration
		want    time.Duration
		wantLow bool // accept [0, want] because a date in the past yields 0
	}{
		{"seconds", "3600", 500 * time.Millisecond, 500 * time.Millisecond, false},
		{"seconds small", "2", 10 * time.Second, 2 * time.Second, false},
		{"seconds zero", "0", 10 * time.Second, 0, false},
		{"overflow seconds", "9999999999999999999999", 500 * time.Millisecond, 500 * time.Millisecond, false},
		{"negative seconds", "-5", 10 * time.Second, 0, false},
		{"http-date future", in1h, 750 * time.Millisecond, 750 * time.Millisecond, false},
		{"http-date past", "Mon, 02 Jan 2006 15:04:05 GMT", 10 * time.Second, 0, true},
		{"unparseable", "soon", 10 * time.Second, 0, false},
		{"empty", "", 10 * time.Second, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := nethttp.Header{}
			h.Set("Retry-After", tc.value)
			got := retryAfterWait(h, tc.cap)
			if tc.wantLow {
				if got < 0 || got > tc.want {
					t.Errorf("retryAfterWait = %v, want in [0, %v]", got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Errorf("retryAfterWait = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBackoffBoundedAndJittered pins the ladder math: attempt n waits
// in [0, min(BaseWait*2^(n-1), MaxWait)) and never overflows.
func TestBackoffBoundedAndJittered(t *testing.T) {
	base := 100 * time.Millisecond
	max := 400 * time.Millisecond
	for attempt := 1; attempt <= 20; attempt++ {
		// Expected bound: min(base * 2^(attempt-1), max), computed
		// independently of the implementation's shift shortcuts.
		bound := base << (attempt - 1)
		if bound <= 0 || bound > max {
			bound = max
		}
		for range 50 {
			got := backoff(base, max, attempt)
			if got < 0 || got >= bound {
				t.Fatalf("attempt %d: backoff = %v, outside [0, %v)", attempt, got, bound)
			}
		}
	}
	if got := backoff(0, max, 3); got != 0 {
		t.Errorf("backoff with zero base = %v, want 0", got)
	}
}
