// Package runner drives batch pipelines over the journal: the save
// pipeline (SPN2 captures, save.go) and the shared plumbing here —
// limits validation and the per-host politeness pool. Every pipeline
// reports progress as an Event stream and finishes with a summary
// plus an error for fatal conditions only; per-target failures are
// journaled, never fatal.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// Limits bounds pacing and concurrency for a runner batch. PerMinute,
// Concurrency, PerHost and JobWait map 1:1 onto config.LimitsConfig;
// PollStart and PollMax are optional poll-backoff bounds (zero applies
// the documented 2s→30s ladder) so tests can shrink every wait.
//
// Unit note: PerMinute carries a golang.org/x/time/rate.Limit, whose
// unit is events per SECOND despite the field name (the plan fixes the
// struct verbatim). The command layer converts the config key with
// rate.Every(time.Minute / save_per_minute); rate.Inf disables pacing.
type Limits struct {
	PerMinute   rate.Limit
	Concurrency int
	PerHost     int
	JobWait     time.Duration
	PollStart   time.Duration
	PollMax     time.Duration
}

const (
	// maxPerHost is the hard per-host ceiling. The documented SPN2
	// spam trigger sits above 20 concurrent captures of one host
	// (r1); the plan caps the configured value at 5 so the trigger
	// can never be approached, even in misconfiguration.
	maxPerHost = 5
	// maxSubmitAttempts bounds resubmits per target. The counter is
	// journaled (Record.Attempts), so the budget also persists across
	// runs; a resubmit happens only after a classified retryable
	// failure, and auth refusals never consume an attempt.
	maxSubmitAttempts = 3
	// defaultPollStart and defaultPollMax form the documented poll
	// ladder: exponential 2s → 30s.
	defaultPollStart = 2 * time.Second
	defaultPollMax   = 30 * time.Second
)

// Validate enforces the numeric [limits] bounds (T2 ruling: the
// runner owns them, config only parses). Zero poll fields apply the
// documented defaults — the TOML has no poll keys, so a preflight on
// config-derived limits must pass. The first violated rule comes back
// as a Russian fail-fast error naming the key.
func (l Limits) Validate() error {
	if l.PerMinute <= 0 {
		return fmt.Errorf("save_per_minute должен быть не меньше 1 (сейчас %v)", l.PerMinute)
	}

	if l.Concurrency < 1 {
		return fmt.Errorf("save_concurrency должен быть не меньше 1 (сейчас %d)", l.Concurrency)
	}

	if l.PerHost < 1 || l.PerHost > maxPerHost {
		return fmt.Errorf("per_host_concurrency должен быть от 1 до %d (сейчас %d)", maxPerHost, l.PerHost)
	}

	if l.JobWait <= 0 {
		return errors.New("job_wait должен быть положительным (например \"20m\")")
	}

	pollStart, pollMax := l.PollStart, l.PollMax
	if pollStart <= 0 {
		pollStart = defaultPollStart
	}

	if pollMax <= 0 {
		pollMax = defaultPollMax
	}

	if pollMax < pollStart {
		return fmt.Errorf("poll_max (%v) должен быть не меньше poll_start (%v)", pollMax, pollStart)
	}

	return nil
}

// withDefaults applies the documented poll ladder to zero fields. The
// clamp keeps the ladder ascending when a caller sets only PollStart.
func (l Limits) withDefaults() Limits {
	if l.PollStart <= 0 {
		l.PollStart = defaultPollStart
	}

	if l.PollMax <= 0 {
		l.PollMax = defaultPollMax
	}

	if l.PollMax < l.PollStart {
		l.PollMax = l.PollStart
	}

	return l
}

// hostLimiters is a mutex-guarded pool of per-host semaphores. A slot
// is held for a whole item (submit plus polling), so simultaneous
// captures of one host never exceed the cap — well below the
// documented >20-same-host trigger.
type hostLimiters struct {
	cap   int64
	mu    sync.Mutex
	hosts map[string]*semaphore.Weighted
}

// newHostLimiters builds the pool with the given per-host capacity.
func newHostLimiters(capacity int) *hostLimiters {
	return &hostLimiters{cap: int64(capacity), hosts: map[string]*semaphore.Weighted{}}
}

// acquire blocks until a slot for host frees up or ctx is done. The
// returned release function is never nil when err is nil.
func (h *hostLimiters) acquire(ctx context.Context, host string) (func(), error) {
	h.mu.Lock()

	sem, ok := h.hosts[host]
	if !ok {
		sem = semaphore.NewWeighted(h.cap)
		h.hosts[host] = sem
	}
	h.mu.Unlock()

	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}

	return func() { sem.Release(1) }, nil
}

// hostOf extracts the politeness key for a target: the URL host. An
// unparseable target falls back to the raw string, which still bounds
// it (the fallback key is a singleton and the global caps hold).
func hostOf(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return target
	}

	return strings.ToLower(u.Host)
}

// sleepCtx waits d or until ctx is done; it reports whether the full
// wait elapsed. A non-positive d returns immediately.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// untilMidnight reports the time to the next local midnight — the
// reset hint for parked quota messages.
func untilMidnight() time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)

	return next.Sub(now)
}
