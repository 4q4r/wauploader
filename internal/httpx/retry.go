package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryMode selects which failure classes DoWithRetry may recover from
// by resending the request.
type RetryMode int

const (
	// RetryNone sends exactly one attempt and never resends. It is the
	// zero value, so a forgotten Mode can never duplicate a capture or
	// re-PUT a multi-gigabyte file: opting out of retries is the safe
	// default.
	RetryNone RetryMode = iota
	// RetryIdempotent runs the full ladder — transport errors, 429, and
	// 5xx except 501. Use only for requests whose replay is safe
	// (GET/HEAD) or whose body is GetBody-replayable AND whose server
	// semantics tolerate a resend after a status response.
	RetryIdempotent
	// RetryConnectOnly resends ONLY when the failure happened in the
	// connect phase (dial/TLS handshake) — nothing was written, so a
	// resend cannot duplicate work. This is the mode for non-idempotent
	// POSTs such as SPN2 /save (r1 F7.7: a timed-out POST may still have
	// created a capture). Status responses are returned untouched for
	// the caller to classify.
	RetryConnectOnly
)

// RetryOpts configures one DoWithRetry call. Zero MaxAttempts, BaseWait
// and MaxWait are filled with the defaults below (5 attempts, 1s, 60s).
type RetryOpts struct {
	Mode        RetryMode
	MaxAttempts int
	BaseWait    time.Duration
	MaxWait     time.Duration
}

// DefaultRetryOpts returns the documented defaults: 5 attempts,
// exponential base 1s, capped at 60s, full ladder.
func DefaultRetryOpts() RetryOpts {
	return RetryOpts{
		Mode:        RetryIdempotent,
		MaxAttempts: 5,
		BaseWait:    time.Second,
		MaxWait:     60 * time.Second,
	}
}

// normalize fills unset fields with the documented defaults.
func (o RetryOpts) normalize() RetryOpts {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.BaseWait <= 0 {
		o.BaseWait = time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 60 * time.Second
	}
	return o
}

// Sentinel errors — branch with errors.Is.
var (
	// ErrRetryBudgetExceeded reports that every attempt returned a
	// retryable failure. Intermediate response bodies are closed; the
	// last status code is embedded in the message.
	ErrRetryBudgetExceeded = errors.New("httpx: retry budget exceeded")
	// ErrContextDeadline reports that the context ended (deadline or
	// cancellation) during the retry cycle. The context error itself is
	// chained, so errors.Is(err, context.Canceled) also holds.
	ErrContextDeadline = errors.New("httpx: context ended during retry")
)

// DoWithRetry executes req honoring o and returns the first response
// that is not a retryable failure for the selected mode.
//
// Replayability contract (load-bearing for consumers):
//   - RetryIdempotent requires req.GetBody != nil whenever a body is
//     present, and fails loudly upfront otherwise — resending an
//     already-written body without rewind would corrupt requests.
//   - RetryConnectOnly never rewrites a body: a connect-phase failure
//     means nothing was written, so the original body reader is
//     untouched for the resend.
//   - RetryNone performs exactly one attempt and returns whatever the
//     server answered, including non-2xx responses with live bodies
//     (SPN2 envelope parsing depends on this).
func (c *Client) DoWithRetry(ctx context.Context, req *http.Request, o RetryOpts) (*http.Response, error) {
	o = o.normalize()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrContextDeadline, err)
	}
	if o.Mode == RetryIdempotent && req.Body != nil && req.GetBody == nil {
		return nil, errors.New("httpx: RetryIdempotent requires a replayable body " +
			"(http.NewRequest sets GetBody for bytes/string readers); set req.GetBody, " +
			"or use RetryConnectOnly for connect-phase-only resends, or RetryNone")
	}

	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		resp, err := c.HTTP.Do(req)
		if err == nil {
			if !retryableStatus(o.Mode, resp.StatusCode) {
				return resp, nil
			}
			// Retryable status: keep the Retry-After instruction, then
			// drain and close the body so the connection can be reused.
			wait := retryAfterWait(resp.Header, o.MaxWait)
			status := resp.StatusCode
			drainAndClose(resp)
			if attempt == o.MaxAttempts {
				return nil, fmt.Errorf("%w after %d attempts: last HTTP status %d (%s)",
					ErrRetryBudgetExceeded, attempt, status, http.StatusText(status))
			}
			if wait == 0 {
				wait = backoff(o.BaseWait, o.MaxWait, attempt)
			}
			if !sleepCtx(ctx, wait) {
				return nil, fmt.Errorf("%w: %w", ErrContextDeadline, ctx.Err())
			}
			if err := rewindBody(req); err != nil {
				return nil, err
			}
			continue
		}

		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w: %w", ErrContextDeadline, ctx.Err(), err)
		}
		if !retryableTransport(o.Mode, err) {
			return nil, err
		}
		if attempt == o.MaxAttempts {
			return nil, fmt.Errorf("%w after %d attempts: %w", ErrRetryBudgetExceeded, attempt, err)
		}
		if !sleepCtx(ctx, backoff(o.BaseWait, o.MaxWait, attempt)) {
			return nil, fmt.Errorf("%w: %w", ErrContextDeadline, ctx.Err())
		}
		if err := rewindBody(req); err != nil {
			return nil, err
		}
	}
	// Unreachable: every loop path returns.
	return nil, errors.New("httpx: retry loop exited unexpectedly")
}

// retryableStatus reports whether code is a retryable response under
// the mode: 429 and every 5xx except 501, and only for the full ladder
// (RetryIdempotent). RetryConnectOnly never retries on statuses — a
// status response means the request was received and answered.
func retryableStatus(mode RetryMode, code int) bool {
	if mode != RetryIdempotent {
		return false
	}
	return code == http.StatusTooManyRequests ||
		(code >= http.StatusInternalServerError && code != http.StatusNotImplemented)
}

// retryableTransport reports whether a transport error may be retried
// under the mode.
func retryableTransport(mode RetryMode, err error) bool {
	switch mode {
	case RetryIdempotent:
		// Replay of an idempotent request is safe, but a redirect loop
		// is a diagnosis, not a transient fault: resending cannot help,
		// so surface it after the first attempt.
		return !errors.Is(err, ErrTooManyRedirects)
	case RetryConnectOnly:
		return connectPhase(err)
	default:
		return false
	}
}

// connectPhase reports whether err occurred before any request bytes
// were written to the connection (dial failure, TLS handshake
// failure). Classification is deliberately conservative: unknown error
// shapes return false, because a wrong "safe" answer for a POST could
// duplicate a capture while a wrong "unsafe" answer only skips a retry.
func connectPhase(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	var unknown net.UnknownNetworkError
	if errors.As(err, &unknown) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return true
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return true
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	return false
}

// retryAfterWait parses the Retry-After header (delay-seconds or
// HTTP-date) and returns the wait capped to limit. A missing,
// unparseable, or already-expired header returns 0. Oversized
// delay-seconds values (including strconv overflow) are capped instead
// of overflowing the Duration conversion.
func retryAfterWait(h http.Header, limit time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		wait := time.Duration(secs) * time.Second
		if secs > int(limit/time.Second)+1 || wait > limit || wait < 0 {
			return limit
		}
		return wait
	} else if allDigits(v) {
		// A strconv-overflowing delay-seconds value is still a valid
		// (absurdly large) instruction: cap it instead of overflowing
		// the Duration conversion or ignoring the header.
		return limit
	}
	if t, err := http.ParseTime(v); err == nil {
		wait := time.Until(t)
		if wait <= 0 {
			return 0
		}
		if wait > limit {
			return limit
		}
		return wait
	}
	return 0
}

// allDigits reports whether s consists solely of ASCII digits (an
// integer too large for strconv.Atoi).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// backoff computes the bounded exponential ladder with full jitter:
// a uniform sample from [0, min(BaseWait * 2^(attempt-1), max)).
func backoff(base, max time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	bound := base
	if shift := attempt - 1; shift > 0 {
		if shift > 20 { // cap before shift overflow; max dominates anyway
			bound = max
		} else {
			bound = base << shift
			if bound <= 0 || bound > max { // overflow guard
				bound = max
			}
		}
	}
	if bound > max {
		bound = max
	}
	if bound <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(bound)))
}

// sleepCtx waits for d, returning false as soon as ctx ends. Every
// retry wait must be interruptible: a Ctrl-C or deadline during a
// backoff sleep returns promptly instead of sleeping past it.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// rewindBody rebuilds req.Body from GetBody before a resend.
func rewindBody(req *http.Request) error {
	if req.Body == nil || req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("httpx: rewind request body for retry: %w", err)
	}
	req.Body = body
	return nil
}

// drainAndClose discards the remaining response body so the underlying
// connection returns to the idle pool.
func drainAndClose(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
