package spn2

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind classifies an SPN2 failure for the runner: KindRetryable failures
// may succeed on a later attempt, KindQuota failures should park the URL
// until the quota window resets, and KindTerminal failures will never
// succeed for this target. KindRetryable is the zero value so an
// unclassified failure is always fail-open.
type Kind int

const (
	KindRetryable Kind = iota
	KindQuota
	KindTerminal
)

// String renders the kind for logs and test output.
func (k Kind) String() string {
	switch k {
	case KindQuota:
		return "quota"
	case KindTerminal:
		return "terminal"
	default:
		return "retryable"
	}
}

// Error is a typed SPN2 API failure. StatusExt keeps the raw wire code
// ("error:invalid-host-resolution") for the journal; Message keeps the
// server's own text; HTTP is the response status of the rejected call.
// The human text is a Russian lead-in quoting the server verbatim, so
// console output stays Russian while diagnostics stay precise.
type Error struct {
	Kind      Kind
	StatusExt string
	Message   string
	HTTP      int
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	switch e.Kind {
	case KindQuota:
		b.WriteString("квота archive.org исчерпана (повторите после обновления лимита)")
	case KindTerminal:
		b.WriteString("сохранение невозможно (повтор не поможет)")
	default:
		b.WriteString("ошибка сохранения (возможен повтор)")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.StatusExt != "" {
		fmt.Fprintf(&b, " (код %s", e.StatusExt)
		if e.HTTP != 0 {
			fmt.Fprintf(&b, ", HTTP %d", e.HTTP)
		}
		b.WriteString(")")
	} else if e.HTTP != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.HTTP)
	}
	return b.String()
}

// statusExtKinds maps every status_ext code documented in the official
// SPN2 doc (r1 F4.1) to its failure kind, keyed by the bare code (the
// "error:" wire prefix is stripped before lookup). The three buckets are
// the plan's taxonomy: retryable targets transient SPN2/infrastructure
// faults, quota marks daily and bandwidth windows, terminal marks target
// problems no retry can fix.
var statusExtKinds = map[string]Kind{
	// Retryable.
	"cannot-fetch":           KindRetryable,
	"proxy-error":            KindRetryable,
	"protocol-error":         KindRetryable,
	"read-timeout":           KindRetryable,
	"browsing-timeout":       KindRetryable,
	"celery":                 KindRetryable,
	"job-failed":             KindRetryable,
	"internal-server-error":  KindRetryable,
	"no-browsers-available":  KindRetryable,
	"capture-location-error": KindRetryable,
	"too-many-requests":      KindRetryable,
	// Quota (park until the window resets).
	"too-many-daily-captures":     KindQuota,
	"user-session-limit":          KindQuota,
	"max-daily-bandwidth":         KindQuota,
	"max-daily-bandwidth-from-ip": KindQuota,
	"max-daily-bandwidth-host":    KindQuota,
	// Terminal.
	"blocked-url":                     KindTerminal,
	"blocked":                         KindTerminal,
	"blocked-client-ip":               KindTerminal,
	"invalid-url-syntax":              KindTerminal,
	"invalid-host-resolution":         KindTerminal,
	"not-found":                       KindTerminal,
	"no-access":                       KindTerminal,
	"unauthorized":                    KindTerminal,
	"filesize-limit":                  KindTerminal,
	"ftp-access-denied":               KindTerminal,
	"bandwidth-limit-exceeded":        KindTerminal,
	"bad-request":                     KindTerminal,
	"http-version-not-supported":      KindTerminal,
	"method-not-allowed":              KindTerminal,
	"not-implemented":                 KindTerminal,
	"network-authentication-required": KindTerminal,
	"gateway-timeout":                 KindTerminal,
	"bad-gateway":                     KindTerminal,
	"service-unavailable":             KindTerminal,
	"invalid-server-response":         KindTerminal,
	"soft-time-limit-exceeded":        KindTerminal,
}

// classifyStatusExt maps a wire status_ext value ("error:codel" or the
// bare code) to its kind. The second result is false for codes absent
// from the documented taxonomy: SPN2 may add new codes at any time, and
// the fail-open policy classifies those as retryable with a warning
// instead of silently dropping a job.
func classifyStatusExt(statusExt string) (Kind, bool) {
	code := strings.TrimPrefix(strings.TrimSpace(strings.ToLower(statusExt)), "error:")
	kind, ok := statusExtKinds[code]
	return kind, ok
}

// terminalMessageMarkers and retryableMessageMarkers classify error
// messages for refusals the doc describes only in prose (r1 F4.3):
// block-listed and hard-limited targets never become capturable, while
// paused hosts and failed task starts recover on their own.
var (
	terminalMessageMarkers = []string{
		"Save Page Now service block list",
		"currently facing some limitations",
	}
	retryableMessageMarkers = []string{
		"Crawling this host is paused",
		"Cannot start capture",
	}
)

// classifyMessage matches a server message against the known markers.
// The second result is false when no marker applies.
func classifyMessage(msg string) (Kind, bool) {
	for _, m := range terminalMessageMarkers {
		if strings.Contains(msg, m) {
			return KindTerminal, true
		}
	}
	for _, m := range retryableMessageMarkers {
		if strings.Contains(msg, m) {
			return KindRetryable, true
		}
	}
	return KindRetryable, false
}

// classifyEnvelope builds the typed error for a {"status":"error"}
// envelope: the status_ext taxonomy first, message markers second, and a
// fail-open retryable error with a visible warning when nothing matches.
func classifyEnvelope(statusExt, message string, httpStatus int) *Error {
	if statusExt != "" {
		kind, known := classifyStatusExt(statusExt)
		msg := message
		if !known {
			msg = "неизвестный код status_ext — классифицировано как повторяемая ошибка: " + message
		}
		return &Error{Kind: kind, StatusExt: statusExt, Message: msg, HTTP: httpStatus}
	}
	if kind, known := classifyMessage(message); known {
		return &Error{Kind: kind, Message: message, HTTP: httpStatus}
	}
	return &Error{
		Kind:    KindRetryable,
		Message: "причина не распознана — классифицировано как повторяемая ошибка: " + message,
		HTTP:    httpStatus,
	}
}

// delayPhraseRe isolates the delay tail of the documented host-pause
// messages ("Your capture will begin in 42s." / "capture will start in
// 2 hours 3 minutes"), leaving any trailing period behind.
var delayPhraseRe = regexp.MustCompile(`(?i)\b(?:begin|start) in\s+(.+?)\.?\s*$`)

// delayTokenRe scans "<number> <unit>" pairs inside the delay tail.
var delayTokenRe = regexp.MustCompile(`(\d+)\s*([A-Za-z]+)`)

// delayUnits maps the accepted unit spellings to durations.
var delayUnits = map[string]time.Duration{
	"s":       time.Second,
	"sec":     time.Second,
	"secs":    time.Second,
	"second":  time.Second,
	"seconds": time.Second,
	"m":       time.Minute,
	"min":     time.Minute,
	"mins":    time.Minute,
	"minute":  time.Minute,
	"minutes": time.Minute,
	"h":       time.Hour,
	"hr":      time.Hour,
	"hrs":     time.Hour,
	"hour":    time.Hour,
	"hours":   time.Hour,
}

// ParseDelayMessage extracts the host-level pause from "Your capture will
// begin in NNs." and its word-form sibling ("... start in 2 hours 30
// minutes"). SPN2 states the exact wait for hosts with too many
// concurrent captures; the runner sleeps that long before resubmitting.
// The second result is false when the message carries no parseable delay.
func ParseDelayMessage(msg string) (time.Duration, bool) {
	m := delayPhraseRe.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	var total time.Duration
	found := false
	for _, tok := range delayTokenRe.FindAllStringSubmatch(m[1], -1) {
		unit, ok := delayUnits[strings.ToLower(tok[2])]
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(tok[1])
		if err != nil || n < 0 {
			return 0, false
		}
		// Guard the Duration conversion: an absurd server value must not
		// wrap negative and turn a wait into an instant pass.
		if int64(n) > math.MaxInt64/int64(unit) {
			return 0, false
		}
		total += time.Duration(n) * unit
		found = true
	}
	if !found {
		return 0, false
	}
	return total, true
}
