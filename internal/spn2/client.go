// Package spn2 implements the Save Page Now 2 capture client: submitting
// capture jobs to POST /save and reading job, user, and system status.
// The wire map is docs/research/r1-spn2-report.md.
//
// Load-bearing behaviors (plan Task 6, Review Focus #1):
//   - SPN2 can refuse with HTTP 200 and a {"status":"error"} body without
//     a job_id. The response is therefore parsed as JSON regardless of
//     HTTP status, and a refusal becomes a typed *Error immediately —
//     never a job to poll.
//   - job_id:null with a dedup message means a recent capture already
//     exists: Submit reports SubmitResult{Deduped:true} as success.
//   - POST /save is never resent after a status response (Submit pins
//     httpx.RetryConnectOnly): an ambiguous timeout may still have
//     created a capture, and only connect-phase failures are safe to
//     replay.
package spn2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/httpx"
)

const (
	// BaseURL is the SPN2 capture endpoint.
	BaseURL = "https://web.archive.org/save"
	// SnapshotURLFmt renders the created capture: the 14-digit timestamp
	// then the original URL (doc: "You can access the created capture
	// using the following URL pattern").
	SnapshotURLFmt = "https://web.archive.org/web/%s/%s"
)

// baseURL is the endpoint root every method builds on. A package-level
// var so httptest fakes can override it; tests never touch live
// archive.org and run sequentially, making the mutation race-free.
var baseURL = BaseURL

// maxBodyRead caps how many response bytes are read before parsing:
// SPN2 answers are small JSON documents, and a runaway body must not
// exhaust memory.
const maxBodyRead = 1 << 20

// Client talks to SPN2 with one resolved identity (auth.Resolve output).
// Construct with NewClient; the zero value is not usable.
type Client struct {
	HTTP  *httpx.Client
	Creds auth.Credentials
	// retry is the ladder for the read-only status endpoints. Submit
	// never consults it: its mode is pinned to httpx.RetryConnectOnly
	// for capture safety, and SystemStatus pins RetryNone so the HTTP
	// 502 critical signal reaches the caller.
	retry httpx.RetryOpts
}

// NewClient wires the HTTP client and credentials into an SPN2 client.
// The job status endpoints use the default idempotent retry ladder.
func NewClient(cl *httpx.Client, creds auth.Credentials) *Client {
	return &Client{HTTP: cl, Creds: creds, retry: httpx.DefaultRetryOpts()}
}

// Options carries every documented POST /save parameter (r1 §2).
// False booleans and empty strings are omitted from the request.
type Options struct {
	CaptureAll, CaptureOutlinks, CaptureScreenshot, DelayWBAvailability bool
	ForceGet, SkipFirstArchive, OutlinksAvailability, EmailResult       bool
	// IfNotArchivedWithin is the raw IA timedelta passthrough ("120" or
	// "3d 5h 20m"); config.ParseIATimedelta validates it upstream. Empty
	// omits the parameter so the server default (1 hour) applies.
	IfNotArchivedWithin string
	// JSBehaviorTimeout in seconds, 0..30. Unlike the booleans, 0 is a
	// meaningful value (it disables JS behaviors), so it is always sent.
	JSBehaviorTimeout int
	// CaptureCookie, UseUserAgent, TargetUsername and TargetPassword
	// pass through to the capture browser for the target site.
	CaptureCookie, UseUserAgent, TargetUsername, TargetPassword string
}

// Encode renders the options as form values. The doc warns that only
// "1" or "on" mean on ("If you use \"01\" or \"True\" it means
// \"off\""), so true renders as "1" and false options are omitted
// entirely.
func (o Options) Encode() url.Values {
	v := url.Values{}
	set1 := func(name string, on bool) {
		if on {
			v.Set(name, "1")
		}
	}
	set1("capture_all", o.CaptureAll)
	set1("capture_outlinks", o.CaptureOutlinks)
	set1("capture_screenshot", o.CaptureScreenshot)
	set1("delay_wb_availability", o.DelayWBAvailability)
	set1("force_get", o.ForceGet)
	set1("skip_first_archive", o.SkipFirstArchive)
	set1("outlinks_availability", o.OutlinksAvailability)
	set1("email_result", o.EmailResult)
	if o.IfNotArchivedWithin != "" {
		v.Set("if_not_archived_within", o.IfNotArchivedWithin)
	}
	v.Set("js_behavior_timeout", strconv.Itoa(o.JSBehaviorTimeout))
	if o.CaptureCookie != "" {
		v.Set("capture_cookie", o.CaptureCookie)
	}
	if o.UseUserAgent != "" {
		v.Set("use_user_agent", o.UseUserAgent)
	}
	if o.TargetUsername != "" {
		v.Set("target_username", o.TargetUsername)
	}
	if o.TargetPassword != "" {
		v.Set("target_password", o.TargetPassword)
	}
	return v
}

// SubmitResult reports one POST /save outcome. JobID is an opaque string
// (UUID or spn2-<hex> shapes both occur in the wild). Deduped marks the
// documented dedup shape (job_id:null plus a message): a recent capture
// already exists, so the URL is done — journal it as done-with-existing.
type SubmitResult struct {
	JobID   string
	Deduped bool
	Message string
}

// submitEnvelope mirrors the POST /save JSON body. JobID is a pointer so
// the code documents the "job_id":null dedup wire shape, though the
// decision tree below only needs present-and-non-empty.
type submitEnvelope struct {
	Status    string  `json:"status"`
	JobID     *string `json:"job_id"`
	URL       string  `json:"url"`
	Message   string  `json:"message"`
	StatusExt string  `json:"status_ext"`
}

// Submit posts one capture request and classifies the answer. The body
// is parsed as JSON whatever the HTTP status says, because SPN2 refuses
// with HTTP 200 envelopes and answers failures with readable bodies:
//
//  1. job_id present → the job exists; return it (a real error will
//     surface from Status; job_id forms are UUID or spn2-<hex>).
//  2. status:"error" without job_id → refusal envelope → typed *Error
//     classified by status_ext, then message markers, then fail-open.
//  3. no job_id, no error status, message present → the documented dedup
//     shape → SubmitResult{Deduped:true} with the message preserved.
//  4. anything else (HTML error pages, empty bodies) → fail-open
//     retryable *Error carrying the HTTP status.
func (c *Client) Submit(ctx context.Context, targetURL string, o Options) (SubmitResult, error) {
	if targetURL == "" {
		return SubmitResult{}, errors.New("не указан адрес страницы (URL) для сохранения")
	}
	form := o.Encode()
	form.Set("url", targetURL)
	code, data, err := c.doForm(ctx, http.MethodPost, baseURL, form, httpx.RetryOpts{Mode: httpx.RetryConnectOnly})
	if err != nil {
		return SubmitResult{}, fmt.Errorf("отправка запроса на сохранение не выполнена: %w", err)
	}

	var env submitEnvelope
	if jerr := json.Unmarshal(data, &env); jerr != nil {
		return SubmitResult{}, &Error{
			Kind:    KindRetryable,
			HTTP:    code,
			Message: "ответ не является JSON: " + oneLine(data),
		}
	}
	if env.JobID != nil && *env.JobID != "" {
		return SubmitResult{JobID: *env.JobID, Message: env.Message}, nil
	}
	if env.Status == "error" {
		return SubmitResult{}, classifyEnvelope(env.StatusExt, env.Message, code)
	}
	if env.Message != "" {
		return SubmitResult{Deduped: true, Message: env.Message}, nil
	}
	return SubmitResult{}, &Error{
		Kind:    KindRetryable,
		HTTP:    code,
		Message: "неожиданный ответ без идентификатора задания: " + oneLine(data),
	}
}

// Status reads one job's status document from GET /save/status/<job_id>.
// An "error" job state is data, not a Go error: the caller inspects
// State/StatusExt and classifies with the runner's policy.
func (c *Client) Status(ctx context.Context, jobID string) (Status, error) {
	if jobID == "" {
		return Status{}, errors.New("не указан идентификатор задания")
	}
	endpoint := baseURL + "/status/" + url.PathEscape(jobID)
	code, data, err := c.doForm(ctx, http.MethodGet, endpoint, nil, c.retry)
	if err != nil {
		return Status{}, fmt.Errorf("получение статуса задания не выполнено: %w", err)
	}
	var st Status
	if jerr := json.Unmarshal(data, &st); jerr != nil {
		return Status{}, &Error{
			Kind:    KindRetryable,
			HTTP:    code,
			Message: "статус задания: ответ не является JSON: " + oneLine(data),
		}
	}
	return st, nil
}

// StatusBatch reads several job statuses with one POST job_ids=<csv>.
func (c *Client) StatusBatch(ctx context.Context, jobIDs []string) ([]Status, error) {
	if len(jobIDs) == 0 {
		return nil, errors.New("не указаны идентификаторы заданий")
	}
	return c.postStatusList(ctx, url.Values{"job_ids": {strings.Join(jobIDs, ",")}})
}

// StatusOutlinks reads the statuses of every outlink job captured under
// one parent job (POST job_id_outlinks=<parent_id>).
func (c *Client) StatusOutlinks(ctx context.Context, parentJobID string) ([]Status, error) {
	if parentJobID == "" {
		return nil, errors.New("не указан идентификатор родительского задания")
	}
	return c.postStatusList(ctx, url.Values{"job_id_outlinks": {parentJobID}})
}

// postStatusList POSTs a status query form and decodes the JSON array of
// per-job status documents; the batch and outlinks forms answer with the
// same payloads as the per-job endpoint.
func (c *Client) postStatusList(ctx context.Context, form url.Values) ([]Status, error) {
	code, data, err := c.doForm(ctx, http.MethodPost, baseURL+"/status", form, c.retry)
	if err != nil {
		return nil, fmt.Errorf("получение статусов заданий не выполнено: %w", err)
	}
	var sts []Status
	if jerr := json.Unmarshal(data, &sts); jerr != nil {
		return nil, &Error{
			Kind:    KindRetryable,
			HTTP:    code,
			Message: "статусы заданий: ответ не является JSON-массивом: " + oneLine(data),
		}
	}
	return sts, nil
}

// UserStatus reads the session quota ({"available":N,"processing":M}).
// The ?_t= epoch-millisecond cache buster is the documented way to avoid
// a stale cached answer.
func (c *Client) UserStatus(ctx context.Context) (available, processing int, err error) {
	endpoint := fmt.Sprintf("%s/status/user?_t=%d", baseURL, time.Now().UnixMilli())
	code, data, err := c.doForm(ctx, http.MethodGet, endpoint, nil, c.retry)
	if err != nil {
		return 0, 0, fmt.Errorf("получение квоты сеансов не выполнено: %w", err)
	}
	var payload struct {
		Available  int `json:"available"`
		Processing int `json:"processing"`
	}
	if jerr := json.Unmarshal(data, &payload); jerr != nil {
		return 0, 0, &Error{
			Kind:    KindRetryable,
			HTTP:    code,
			Message: "квота сеансов: ответ не является JSON: " + oneLine(data),
		}
	}
	return payload.Available, payload.Processing, nil
}

// SystemStatus probes the service health endpoint. The documented
// "ok" answer reports (true, false); any other parseable answer is the
// overload notice and reports (false, true). The probe sends a single
// attempt: the runner polls health on a schedule, and the documented
// HTTP 502 critical signal must reach the status-driven check below —
// never be consumed by the retry ladder or inferred from the body.
func (c *Client) SystemStatus(ctx context.Context) (ok bool, overloaded bool, err error) {
	code, data, err := c.doForm(ctx, http.MethodGet, baseURL+"/status/system", nil, httpx.RetryOpts{Mode: httpx.RetryNone})
	if err != nil {
		return false, false, fmt.Errorf("проверка состояния службы не выполнена: %w", err)
	}
	if code == http.StatusBadGateway {
		return false, false, &Error{
			Kind:    KindRetryable,
			HTTP:    code,
			Message: "служба SPN2 в критическом состоянии",
		}
	}
	var payload struct {
		Status string `json:"status"`
	}
	if jerr := json.Unmarshal(data, &payload); jerr != nil {
		return false, false, &Error{
			Kind:    KindRetryable,
			HTTP:    code,
			Message: "состояние службы: ответ не является JSON: " + oneLine(data),
		}
	}
	if payload.Status == "ok" {
		return true, false, nil
	}
	return false, true, nil
}

// doForm executes one SPN2 API request. A nil form sends a GET without a
// body; otherwise the form is URL-encoded as the request body. It returns
// the HTTP status and raw body so callers can parse the JSON envelope
// regardless of the status code.
func (c *Client) doForm(ctx context.Context, method, endpoint string, form url.Values, opts httpx.RetryOpts) (int, []byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, fmt.Errorf("построить запрос spn2: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	}
	c.Creds.SPN2Request(req)

	resp, err := c.HTTP.DoWithRetry(ctx, req, opts)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
		_ = resp.Body.Close()
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("чтение ответа spn2 (HTTP %d): %w", resp.StatusCode, err)
	}
	return resp.StatusCode, data, nil
}

// Status is one SPN2 job status document (pending, success, or error).
// The outlinks field has three documented shapes, split across Outlinks,
// OutlinkJobs, and OutlinkTimestamps by UnmarshalJSON.
type Status struct {
	State             string
	JobID             string
	OriginalURL       string
	Timestamp         string
	DurationSec       float64
	Resources         []string
	Outlinks          []string
	OutlinkJobs       map[string]string
	OutlinkTimestamps map[string]string
	Screenshot        string
	DownloadSize      int64
	HTTPStatus        int
	FirstArchive      bool
	StatusExt         string
	Message           string
	Exception         string
}

// UnmarshalJSON decodes the documented status fields plus the wild ones
// (first_archive, http_status, download_size). Pending payloads carry
// "url" instead of "original_url", so the fallback keeps the target
// reachable in every state. The outlinks field has three shapes:
//   - JSON array of URLs → Outlinks;
//   - object outlink → child job_id string → OutlinkJobs (capture_outlinks=1);
//   - object outlink → {"timestamp": "..."} → OutlinkTimestamps
//     (outlinks_availability=1), where a null timestamp maps to "".
func (s *Status) UnmarshalJSON(data []byte) error {
	var raw struct {
		Status       string          `json:"status"`
		JobID        string          `json:"job_id"`
		OriginalURL  string          `json:"original_url"`
		URL          string          `json:"url"`
		Timestamp    string          `json:"timestamp"`
		DurationSec  float64         `json:"duration_sec"`
		Resources    []string        `json:"resources"`
		Outlinks     json.RawMessage `json:"outlinks"`
		Screenshot   string          `json:"screenshot"`
		DownloadSize int64           `json:"download_size"`
		HTTPStatus   int             `json:"http_status"`
		FirstArchive bool            `json:"first_archive"`
		StatusExt    string          `json:"status_ext"`
		Message      string          `json:"message"`
		Exception    string          `json:"exception"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.State = raw.Status
	s.JobID = raw.JobID
	s.OriginalURL = raw.OriginalURL
	if s.OriginalURL == "" {
		s.OriginalURL = raw.URL
	}
	s.Timestamp = raw.Timestamp
	s.DurationSec = raw.DurationSec
	s.Resources = raw.Resources
	s.Screenshot = raw.Screenshot
	s.DownloadSize = raw.DownloadSize
	s.HTTPStatus = raw.HTTPStatus
	s.FirstArchive = raw.FirstArchive
	s.StatusExt = raw.StatusExt
	s.Message = raw.Message
	s.Exception = raw.Exception

	trimmed := bytes.TrimSpace(raw.Outlinks)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return fmt.Errorf("разобрать outlinks (массив): %w", err)
		}
		s.Outlinks = list
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return fmt.Errorf("разобрать outlinks (объект): %w", err)
		}
		for link, v := range obj {
			val := bytes.TrimSpace(v)
			if len(val) > 0 && val[0] == '{' {
				var avail struct {
					Timestamp *string `json:"timestamp"`
				}
				if err := json.Unmarshal(val, &avail); err != nil {
					return fmt.Errorf("разобрать outlinks_availability для %s: %w", link, err)
				}
				if s.OutlinkTimestamps == nil {
					s.OutlinkTimestamps = map[string]string{}
				}
				if avail.Timestamp != nil {
					s.OutlinkTimestamps[link] = *avail.Timestamp
				} else {
					s.OutlinkTimestamps[link] = ""
				}
				continue
			}
			var jobID string
			if err := json.Unmarshal(val, &jobID); err != nil {
				return fmt.Errorf("разобрать outlink job_id для %s: %w", link, err)
			}
			if s.OutlinkJobs == nil {
				s.OutlinkJobs = map[string]string{}
			}
			s.OutlinkJobs[link] = jobID
		}
	}
	return nil
}

// oneLine collapses a response body into a bounded single-line fragment
// for error messages.
func oneLine(data []byte) string {
	s := strings.Join(strings.Fields(string(data)), " ")
	if s == "" {
		return "(пустое тело ответа)"
	}
	return truncate(s)
}

// truncate caps s to 200 runes so error messages stay bounded.
func truncate(s string) string {
	const maxRunes = 200
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
