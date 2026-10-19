package ias3

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/httpx"
)

// Upload loop budget (plan Task 7): a retryable failure re-PUTs the whole
// file (IA-S3 has no resume) at most maxUploadAttempts-1 times, sleeping
// slowDownWait and re-checking CheckLimit before every re-PUT. Five
// attempts total matches the httpx ladder default; the sleeps are
// package-level vars so tests can shrink them (tests run sequentially).
const (
	maxUploadAttempts = 5
	maxBodyRead       = 1 << 20 // response bodies are tiny; refuse to buffer a huge one
	maxErrorFragment  = 200     // raw body fragment cap embedded in error messages
)

var (
	// slowDownWait is the reference-client default pause between 503
	// SlowDown re-PUTs (ia upload -s/--sleep default: 30s).
	slowDownWait = 30 * time.Second
	// CheckLimitPoll is the cadence of the AwaitNotOverLimit poll loop
	// (the preflight and the SlowDown re-PUT path). Exported so the
	// upload runner's tests can shrink it — the ias3 tests shrink it
	// the same way (tests run sequentially).
	CheckLimitPoll = 15 * time.Second
	// retryAfterUnit converts a Retry-After delay-seconds value into a
	// wait; tests shrink it together with slowDownWait (tests run
	// sequentially).
	retryAfterUnit = time.Second
)

// Sentinel errors — branch with errors.Is. The spam case is deliberately
// its own error type: a 503 whose body contains "appears to be spam" is a
// hard block (Review Focus #2 of the plan) and must NEVER be retried,
// unlike a plain 503 SlowDown.
var (
	// ErrSpamSlowDown — archive.org classified the upload as spam. No
	// retry can help; the user must contact info@archive.org (IA help
	// center "503-slowdown-spam").
	ErrSpamSlowDown = errors.New("загрузка отклонена: archive.org пометил файл как спам (503, «appears to be spam»); повторные попытки бесполезны — обратитесь в info@archive.org, если считаете блокировку ошибочной")
	// ErrBadCredentials — the S3 key pair was rejected: HTTP 403 with
	// XML code InvalidAccessKeyId, or a 403 body "bad request"
	// (malformed LOW auth). Check s3_access/s3_secret in the config.
	ErrBadCredentials = errors.New("ключи s3_access/s3_secret отклонены archive.org (HTTP 403): проверьте s3_access и s3_secret в файле конфигурации или выполните wauploader init")
)

// UploadRequest describes one file PUT.
type UploadRequest struct {
	Identifier string // item (S3 bucket) name; validated with ValidateIdentifier
	Key        string // file name inside the item (flat, no slashes)
	Path       string // local file to read
	Size       int64  // declared size in bytes; <= 0 falls back to os.Stat
	MD5Hex     string // optional Content-MD5 hex digest (IA accepts hex, r2 §2.3)
}

// Client is the IA-S3 API client. It wraps the shared hardened httpx
// client (h1-only transport, descriptive User-Agent, Authorization kept
// on *.archive.org redirect hops) with the IA-S3 key credentials.
type Client struct {
	hc    *httpx.Client
	creds auth.Credentials

	// base is the S3 endpoint root; S3Base by default. Package-level so
	// the httptest fakes can repoint it (same override pattern as the
	// auth endpoint vars; tests run sequentially).
	base string

	// webBase is the archive.org host serving the Item Metadata API and
	// the Tasks API (meta.go) — a DIFFERENT host than base: S3Base for
	// PUT/CheckLimit, archive.org for /metadata/ and /services/tasks.php.
	// ArchiveBase by default; same httptest override seam as base.
	webBase string
}

// NewClient builds an IA-S3 client over the shared HTTP client. The S3
// keys are used as-is; key presence is enforced per call (in Russian)
// because the zero value must stay constructible for tests.
func NewClient(hc *httpx.Client, creds auth.Credentials) *Client {
	return &Client{hc: hc, creds: creds, base: S3Base, webBase: ArchiveBase}
}

// setAuth attaches the IA-S3 Authorization: LOW header (r2 §1.2 — SigV4
// is not supported). Kept local to this package so auth's public contract
// stays untouched; secrets travel only in the request header and never in
// logs or errors.
func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "LOW "+c.creds.Access+":"+c.creds.Secret)
}

// fileURL builds the PUT target <base>/<identifier>/<key>. Both segments
// are percent-escaped; IA-S3 keys are flat file names.
func (c *Client) fileURL(identifier, key string) string {
	return c.base + "/" + url.PathEscape(identifier) + "/" + url.PathEscape(key)
}

// --- Overload preflight ---------------------------------------------------

// checkLimitPayload mirrors the check_limit=1 JSON answer. over_limit
// arrives as 0/1 in the docs but is parsed tolerantly (bool or number).
type checkLimitPayload struct {
	Bucket    string          `json:"bucket"`
	AccessKey string          `json:"accesskey"`
	OverLimit json.RawMessage `json:"over_limit"`
	Detail    json.RawMessage `json:"detail"`
}

// CheckLimit asks the IA-S3 endpoint whether uploads for this access key
// and bucket are currently likely to get 503 SlowDown errors
// (GET /?check_limit=1&accesskey=...&bucket=..., r2 §4.2).
//
// Fail-safe semantics (r2 §4.2: s3_is_overloaded treats any doubt as
// overloaded): every failure to obtain a definite answer — transport
// error, non-200, unparseable body, unrecognized over_limit — returns
// overLimit=true together with a non-nil error describing why. Callers
// that ignore the error still get the safe boolean.
func (c *Client) CheckLimit(ctx context.Context, identifier string) (overLimit bool, err error) {
	if c.creds.Access == "" {
		return true, errors.New("префлайт check_limit невозможен: s3_access не задан в конфигурации")
	}

	q := url.Values{}
	q.Set("check_limit", "1")
	q.Set("accesskey", c.creds.Access)
	q.Set("bucket", identifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/?"+q.Encode(), nil)
	if err != nil {
		return true, fmt.Errorf("префлайт check_limit: построить запрос: %w", sanitizeReqErr(err))
	}
	// No Authorization header: the reference client sends the access key
	// in the query and nothing else for this endpoint (r2 §4.2).
	resp, err := c.hc.DoWithRetry(ctx, req, httpx.RetryOpts{Mode: httpx.RetryIdempotent})
	if err != nil {
		return true, fmt.Errorf("не удалось проверить лимит очереди для %s (считаем лимит исчерпанным): %w", identifier, sanitizeReqErr(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("префлайт check_limit: неожиданный HTTP %d (считаем лимит исчерпанным)", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return true, fmt.Errorf("префлайт check_limit: чтение ответа: %w", err)
	}

	var payload checkLimitPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return true, fmt.Errorf("префлайт check_limit: ответ не разобран (считаем лимит исчерпанным): %w", err)
	}

	over, perr := parseOverLimit(payload.OverLimit)
	if perr != nil {
		return true, fmt.Errorf("префлайт check_limit: поле over_limit не распознано (считаем лимит исчерпанным): %w", perr)
	}

	return over, nil
}

// parseOverLimit accepts over_limit as JSON bool or number (docs show 0/1;
// the field is also tolerated as true/false). Anything else is an error.
func parseOverLimit(raw json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}

	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0, nil
	}

	return false, fmt.Errorf("ожидается bool или число, получено: %s", truncateFragment(string(raw)))
}

// maxCheckLimitErrors caps CONSECUTIVE errored CheckLimit calls inside
// AwaitNotOverLimit. A legit over_limit=1 answer is valid server state
// and keeps polling unbounded; a stably failing endpoint is an outage,
// and polling it forever would hang the upload loop.
const maxCheckLimitErrors = 8

// awaitNotOverLimit → AwaitNotOverLimit export note: the upload runner
// (internal/runner) consumes this poll loop for its per-file preflight;
// the export is the sanctioned one-word change (Task 11).

// AwaitNotOverLimit polls CheckLimit until the overload flag clears or
// ctx ends. The loop is the fail-safe consumer of CheckLimit: an errored
// check counts as "still overloaded" and keeps polling at CheckLimitPoll
// cadence, so a flapping endpoint never produces a false green light.
// maxCheckLimitErrors consecutive errored checks (without a single
// success in between) end the loop with the last error: the endpoint is
// broken, not busy. A successful check resets the counter.
func AwaitNotOverLimit(ctx context.Context, c *Client, identifier string) error {
	consecutiveErrs := 0

	for {
		over, err := c.CheckLimit(ctx, identifier)
		if err == nil {
			consecutiveErrs = 0

			if !over {
				return nil
			}
		} else {
			consecutiveErrs++
			if consecutiveErrs >= maxCheckLimitErrors {
				return fmt.Errorf("проверка лимита очереди стабильно падает (%d попыток подряд), загрузка отменена: %w", maxCheckLimitErrors, err)
			}
		}

		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("ожидание снятия лимита загрузки прервано: %w", cerr)
		}

		if !sleepCtx(ctx, CheckLimitPoll) {
			return fmt.Errorf("ожидание снятия лимита загрузки прервано: %w", ctx.Err())
		}
	}
}

// sanitizeReqErr strips the *url.Error wrapper from transport and parse
// errors: its text prints the full request URL, and the check_limit URL
// carries the access key in its query (documented wire shape) — a global
// constraint forbids secrets in error messages. The underlying cause
// (dial failure, redirect cap, deadline) survives the unwrap.
func sanitizeReqErr(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}

// sleepCtx waits for d, returning false as soon as ctx ends.
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

// --- Upload ---------------------------------------------------------------

// s3ErrorXML mirrors the AWS-style IA-S3 error body (r2 §4.1):
// <Error><Code>..</Code><Message>..</Message><Resource/><RequestId/></Error>.
type s3ErrorXML struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

// S3Error is a typed IA-S3 error: HTTP status plus the parsed XML fields
// when the body was valid error XML. It is terminal — the retry budget
// lives in Upload, not in the caller.
type S3Error struct {
	StatusCode int
	Code       string
	Message    string
	Resource   string
	RequestID  string
	// bodyFragment carries a truncated raw-body snippet when the body
	// was not valid error XML (still bounded, secrets-free).
	bodyFragment string
}

func (e *S3Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "archive.org вернул ошибку (HTTP %d", e.StatusCode)

	if e.Code != "" {
		fmt.Fprintf(&b, ", код %s", e.Code)
	}

	b.WriteString("): ")

	switch {
	case e.Message != "":
		b.WriteString(e.Message)
	case e.bodyFragment != "":
		b.WriteString("тело ответа: ")
		b.WriteString(e.bodyFragment)
	default:
		b.WriteString(http.StatusText(e.StatusCode))
	}

	if e.RequestID != "" {
		fmt.Fprintf(&b, " [RequestID %s]", e.RequestID)
	}

	return b.String()
}

// parseS3ErrorBody classifies a response body: ok reports whether it was
// valid error XML with a Code element (invalid-XML bodies are treated as
// retryable on 5xx per r2 §4.1 — "either temporary, or requires the
// intervention of IA admins").
func parseS3ErrorBody(raw []byte) (parsed s3ErrorXML, ok bool) {
	var e s3ErrorXML
	if err := xml.Unmarshal(raw, &e); err != nil || e.Code == "" {
		return e, false
	}

	return e, true
}

func truncateFragment(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxErrorFragment {
		return s
	}

	return s[:maxErrorFragment] + "…"
}

// countingReader reports cumulative progress as bytes flow to the socket.
// A fresh counter is created per PUT attempt: each re-PUT re-sends the
// whole file, so per-attempt progress is the honest number.
type countingReader struct {
	r  io.Reader
	n  int64
	fn func(sent int64)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.n += int64(n)
		if cr.fn != nil {
			cr.fn(cr.n)
		}
	}

	return n, err
}

// Upload performs the single-PUT upload of req.Path into
// <identifier>/<key> with the o control surface and the Content-MD5 hex
// digest when provided.
//
// Retry semantics (plan Task 7, r2 §4.3): the httpx ladder is OFF for the
// PUT body (RetryNone — one wire attempt per loop iteration); retryable
// failures are handled by the dedicated loop below:
//
//   - 503 with "appears to be spam" in the body → terminal
//     ErrSpamSlowDown, NEVER retried;
//   - 429 Too Many Requests (owner ruling, rclone precedent) and plain
//     503 SlowDown, plus any 5xx whose body is not valid error XML →
//     sleep, re-check via AwaitNotOverLimit, re-PUT the whole file; at
//     most maxUploadAttempts PUTs total. A 429's Retry-After header is
//     honored (delay-seconds), capped at the 30s default sleep; the spam
//     sniff runs on 429 bodies too, before the retry decision;
//   - 403 with XML code InvalidAccessKeyId or body "bad request" →
//     terminal ErrBadCredentials;
//   - every other answered status → typed *S3Error, terminal;
//   - transport error (connection died mid-PUT) → returned immediately:
//     bytes may have been written, and the pacing loop must not re-PUT
//     automatically; the upload runner decides on a fresh attempt.
//
// Transfer-Encoding: chunked is never used: ContentLength is set from the
// declared/file size, so Go writes a plain Content-Length framing.
// Expect: 100-continue is set and honored by the httpx transport
// (ExpectContinueTimeout: 1s), so a rejection arriving before the body
// saves the full upload. GetBody is wired to re-seek the file so the
// frequent IA-S3 307 redirects are followed with replayable bodies and
// the httpx *.archive.org Authorization policy applies on every hop.
// progress (may be nil) reports bytes sent within the current attempt.
func (c *Client) Upload(ctx context.Context, req UploadRequest, o HeaderOpts, progress func(sent int64)) error { //nolint:gocognit,gocyclo // upload pipeline stages (preflight/PUT/SlowDown/spam) mirror the wire; 60 gocognit / 39 gocyclo vs caps 30/20
	if err := ValidateIdentifier(req.Identifier); err != nil {
		return err
	}

	if req.Key == "" {
		return errors.New("не указано имя файла (key) для загрузки в элемент")
	}
	// The item's <id>_meta.xml and <id>_files.xml are IA-managed: a PUT
	// would clobber the item's metadata/descriptor (realistic trigger:
	// re-uploading a mirrored item directory). Exact, case-sensitive match.
	if req.Key == req.Identifier+"_meta.xml" || req.Key == req.Identifier+"_files.xml" {
		return fmt.Errorf("файл «%s» — служебный файл archive.org — загрузка запрещена: его перезапись повредит элемент", req.Key)
	}

	if c.creds.Access == "" || c.creds.Secret == "" {
		return errors.New("загрузка невозможна: s3_access или s3_secret не заданы в конфигурации")
	}

	if err := ValidateExtraMeta(o.Extra); err != nil {
		return err
	}

	f, err := os.Open(req.Path) // #nosec G304 -- path comes from the operator's own request
	if err != nil {
		return fmt.Errorf("не удалось открыть файл %s: %w", req.Path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("не удалось получить размер файла %s: %w", req.Path, err)
	}

	size := req.Size
	if size <= 0 {
		size = st.Size()
	}

	if size != st.Size() {
		return fmt.Errorf("размер файла %s (%d байт) не совпадает с указанным (%d байт): файл изменился после подготовки — пересчитайте размер и контрольную сумму",
			req.Path, st.Size(), size)
	}

	hdrs := o.headers()

	for attempt := 1; attempt <= maxUploadAttempts; attempt++ {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("не удалось вернуться к началу файла %s: %w", req.Path, err)
		}

		body := &countingReader{r: f, fn: progress}

		put, err := http.NewRequestWithContext(ctx, http.MethodPut, c.fileURL(req.Identifier, req.Key), body)
		if err != nil {
			return fmt.Errorf("построить PUT-запрос: %w", err)
		}

		for k, vs := range hdrs {
			for _, v := range vs {
				put.Header.Set(k, v)
			}
		}

		if req.MD5Hex != "" {
			put.Header.Set("Content-MD5", req.MD5Hex)
		}
		// Known length → Content-Length on the wire, chunked never.
		put.ContentLength = size
		// IA-S3 clients need good 100-Continue support (r2 §1.5); the
		// httpx transport runs the continue dance (ExpectContinueTimeout 1s).
		put.Header.Set("Expect", "100-continue")
		// Replayable body for the frequent 307 redirects: re-seek and
		// re-wrap the file so net/http may follow the hop.
		put.GetBody = func() (io.ReadCloser, error) {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("не удалось вернуться к началу файла %s: %w", req.Path, err)
			}

			return io.NopCloser(&countingReader{r: f, fn: progress}), nil
		}
		c.setAuth(put)

		// The caller's ladder is OFF for the PUT body (plan Task 7): one
		// wire attempt per loop iteration; pacing below owns the retries.
		resp, err := c.hc.DoWithRetry(ctx, put, httpx.RetryOpts{Mode: httpx.RetryNone})
		if err != nil {
			return fmt.Errorf("загрузка %s не выполнена: %w", req.Key, err)
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
		_ = resp.Body.Close()

		status := resp.StatusCode
		if readErr != nil && len(raw) == 0 {
			return fmt.Errorf("загрузка %s: чтение ответа (HTTP %d): %w", req.Key, status, readErr)
		}

		if status >= 200 && status < 300 {
			return nil
		}

		// Terminal spam block — check BEFORE any retry decision. IA
		// delivers the spam block as a 503 SlowDown (r2 §4.2); it is
		// sniffed on 429 responses too now that those are retryable.
		if (status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests) &&
			bytes.Contains(raw, []byte("appears to be spam")) {
			return ErrSpamSlowDown
		}

		parsed, xmlOK := parseS3ErrorBody(raw)

		if status == http.StatusForbidden &&
			(parsed.Code == "InvalidAccessKeyId" || bytes.Contains(raw, []byte("bad request"))) {
			return ErrBadCredentials
		}

		// Retryable: 429 Too Many Requests (owner ruling, rclone
		// precedent), plain 503 SlowDown, and any 5xx whose body is not
		// valid error XML (r2 §4.1: probably temporary).
		retryable := status == http.StatusServiceUnavailable ||
			status == http.StatusTooManyRequests ||
			(status >= 500 && !xmlOK)
		if retryable {
			if attempt == maxUploadAttempts {
				// Budget spent: surface the last failure as a typed error.
				return budgetExhaustedError(status, parsed, xmlOK, raw)
			}
			// 429 honors Retry-After (capped at the SlowDown default);
			// 503/5xx use the default reference-client sleep.
			wait := slowDownWait
			if status == http.StatusTooManyRequests {
				wait = retryAfterPause(resp.Header)
			}

			if !sleepCtx(ctx, wait) {
				return fmt.Errorf("загрузка %s прервана во время ожидания после HTTP %d: %w", req.Key, status, ctx.Err())
			}

			if err := AwaitNotOverLimit(ctx, c, req.Identifier); err != nil {
				return fmt.Errorf("загрузка %s: %w", req.Key, err)
			}

			continue // whole-file re-PUT
		}

		if xmlOK {
			return &S3Error{
				StatusCode: status,
				Code:       parsed.Code,
				Message:    truncateFragment(parsed.Message),
				Resource:   parsed.Resource,
				RequestID:  parsed.RequestID,
			}
		}

		return &S3Error{
			StatusCode:   status,
			bodyFragment: truncateFragment(string(raw)),
		}
	}
	// Unreachable: the loop returns on every path.
	return errors.New("ias3: цикл загрузки завершился неожиданно")
}

// retryAfterPause converts a Retry-After delay-seconds header into a
// bounded wait: seconds × retryAfterUnit, capped at slowDownWait (the 30s
// default sleep). A missing, unparseable, non-positive, or overflowing
// value falls back to the default sleep — the cap also absorbs the
// Duration-overflow of an absurdly large value.
func retryAfterPause(h http.Header) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs <= 0 {
		return slowDownWait
	}

	wait := time.Duration(secs) * retryAfterUnit
	if wait <= 0 || wait > slowDownWait {
		return slowDownWait
	}

	return wait
}

// budgetExhaustedError builds the typed failure for a retryable status
// that survived the whole loop budget.
func budgetExhaustedError(status int, parsed s3ErrorXML, xmlOK bool, raw []byte) error {
	if xmlOK {
		return &S3Error{
			StatusCode: status,
			Code:       parsed.Code,
			Message:    truncateFragment(parsed.Message),
			Resource:   parsed.Resource,
			RequestID:  parsed.RequestID,
		}
	}

	return &S3Error{
		StatusCode:   status,
		Message:      fmt.Sprintf("исчерпан бюджет попыток (%d), archive.org продолжает отвечать ошибкой", maxUploadAttempts),
		bodyFragment: truncateFragment(string(raw)),
	}
}
