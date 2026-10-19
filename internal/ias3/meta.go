// meta.go extends the IA-S3 client with the archive.org-side surfaces of
// the upload workflow (r2-ia-s3-report.md §5–§6): Item Metadata reads for
// the skip-if-exists integrity primitive, and Tasks API polling for derive
// completion. Both live on the archive.org host (Client.webBase), NOT on
// s3.us.archive.org:
//
//   - Metadata Read: GET <webBase>/metadata/<identifier> → files[] with
//     name/source/format/mtime/size/md5/crc32/sha1 (§5.1). Keys OR cookies
//     are accepted for restricted items; this client always sends the S3
//     keys (Authorization: LOW) per the keys-everywhere ruling (§7).
//   - Tasks API: GET <webBase>/services/tasks.php?identifier=<id>&catalog=1
//     &history=1 — S3 keys REQUIRED, cookies REJECTED (§6.1). Run states
//     wait_admin: 0=queued 1=running 2=error 9=paused; a derive counts as
//     done when it has left catalog and appears in history SUBMITTED at or
//     after the caller's notBefore cutoff (r2 §6.2 submittime criteria —
//     the cutoff scopes the observation to the current batch, so a prior
//     batch's derive can never fake a result). Among catalog derives the
//     NEWEST by task_id defines the state: tasks serialize per item, the
//     newest is the live one.
package ias3

import (
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

	"github.com/4q4r/wauploader/internal/httpx"
)

// ArchiveBase is the archive.org web host for the Item Metadata API and
// the Tasks API. Distinct from S3Base by design: the package talks to two
// hosts, and the httptest fakes repoint both (Client.base, Client.webBase).
const ArchiveBase = "https://archive.org"

const (
	// maxMetaBodyRead bounds the buffered metadata answer. The API allows
	// up to 250,000 files per item (r2 §2.4), so a full files[] document
	// can reach tens of MB — far beyond the shared maxBodyRead cap, which
	// stays in force for the small tasks.php envelopes.
	maxMetaBodyRead = 64 << 20

	// tasksPageLimit is the explicit tasks.php page size: the documented
	// maximum (server clamps to 500, §6.2). Wauploader only needs task
	// PRESENCE (no paging cursor walk): 500 recent tasks cover any
	// realistic item. Direction of failure is analyzed fail-safe: a
	// derive that fell off both pages keeps the caller polling until the
	// wait timeout — never a false "done".
	tasksPageLimit = 500

	// deriveCmd is the Tasks API command name for derive jobs (§6.3);
	// matching is exact, as the API itself matches the cmd criterion.
	deriveCmd = "derive.php"
)

// ItemFile is one files[] entry of an item, reduced to the fields the
// upload pipeline needs for integrity checks (r2 §2.3, §5.1).
type ItemFile struct {
	Name  string // key (file name) inside the item, case-sensitive
	MD5   string // hex digest; empty for derivatives lacking one
	SHA1  string // hex digest; may be empty
	CRC32 string // hex digest; may be empty
	Size  int64  // bytes; the API sends it as a string or a number
}

// flexInt64 unmarshals a JSON number OR a JSON string carrying a decimal
// integer. The Metadata API returns size and mtime as strings in real
// answers while the docs show numbers (r2 §5.1); both spellings must parse
// into ItemFile.Size. Anything else is a parse error.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("ожидалось число (или строка с числом), получено: %s", truncateFragment(string(b)))
	}
	*f = flexInt64(n)
	return nil
}

// itemFileJSON mirrors one files[] entry on the wire; ItemFile is the
// cleaned public projection.
type itemFileJSON struct {
	Name  string    `json:"name"`
	MD5   string    `json:"md5"`
	SHA1  string    `json:"sha1"`
	CRC32 string    `json:"crc32"`
	Size  flexInt64 `json:"size"`
}

// ItemFiles reads the item's file list from the Item Metadata API
// (GET <webBase>/metadata/<identifier>, r2 §5.1). An empty or missing
// files[] — a freshly created or not-yet-ingested item — is a valid empty
// slice, not an error: the upload pipeline relies on that for its
// skip-if-exists decision.
//
// The S3 keys ride as Authorization: LOW (restricted items need them;
// cookies are deliberately never attached — keys everywhere, r2 §7).
func (c *Client) ItemFiles(ctx context.Context, identifier string) ([]ItemFile, error) {
	if err := ValidateIdentifier(identifier); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.webBase+"/metadata/"+url.PathEscape(identifier), nil)
	if err != nil {
		return nil, fmt.Errorf("чтение метаданных элемента %s: построить запрос: %w", identifier, sanitizeReqErr(err))
	}
	c.setAuth(req)
	resp, err := c.hc.DoWithRetry(ctx, req, httpx.RetryOpts{Mode: httpx.RetryIdempotent})
	if err != nil {
		return nil, fmt.Errorf("чтение метаданных элемента %s не выполнено: %w", identifier, sanitizeReqErr(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("чтение метаданных элемента %s: неожиданный HTTP %d", identifier, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBodyRead))
	if err != nil {
		return nil, fmt.Errorf("чтение метаданных элемента %s: чтение ответа: %w", identifier, err)
	}
	var payload struct {
		Files []itemFileJSON `json:"files"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("чтение метаданных элемента %s: ответ не разобран: %w", identifier, err)
	}
	files := make([]ItemFile, 0, len(payload.Files))
	for _, f := range payload.Files {
		files = append(files, ItemFile{
			Name:  f.Name,
			MD5:   f.MD5,
			SHA1:  f.SHA1,
			CRC32: f.CRC32,
			Size:  int64(f.Size),
		})
	}
	return files, nil
}

// FileExistsMatchingMD5 is the skip-if-exists primitive (reference client
// --checksum semantics, r2 §2.3): the file exists AND its stored md5 equals
// md5Hex. A missing key, an empty local digest, or a stored answer without
// a digest all return (false, nil) — identity can only be skipped when it
// is PROVEN, so every doubt re-uploads (the PUT is an idempotent overwrite).
// Key comparison is exact: IA-S3 keys are case-sensitive.
func (c *Client) FileExistsMatchingMD5(ctx context.Context, identifier, key, md5Hex string) (bool, error) {
	if key == "" {
		return false, errors.New("проверка «пропустить, если уже загружен» невозможна: не указано имя файла внутри элемента")
	}
	if md5Hex == "" {
		// No local digest — nothing to prove identity with; re-upload.
		return false, nil
	}
	files, err := c.ItemFiles(ctx, identifier)
	if err != nil {
		return false, fmt.Errorf("проверка «пропустить, если уже загружен»: %w", err)
	}
	for _, f := range files {
		if f.Name == key {
			return f.MD5 != "" && f.MD5 == md5Hex, nil
		}
	}
	return false, nil
}

// --- Derive polling ---------------------------------------------------------

// DeriveState is wauploader's view of a derive job's progress. The numeric
// values are OURS (stable for console dictionaries), not the wire's
// wait_admin codes; the mapping lives in deriveStatusDetailed.
type DeriveState int

const (
	// DeriveNone — no derive task found in catalog or history: the PUT's
	// auto-queued derive may not be visible yet (ingest runs first).
	DeriveNone DeriveState = iota
	// DeriveQueued — catalog: wait_admin 0.
	DeriveQueued
	// DeriveRunning — catalog: wait_admin 1 (also: unrecognized catalog
	// state — kept non-terminal so unknown future states stay "waiting").
	DeriveRunning
	// DeriveError — catalog: wait_admin 2. Terminal for WaitDerive.
	DeriveError
	// DerivePaused — catalog: wait_admin 9 (held by IA staff).
	DerivePaused
	// DeriveDone — left catalog, present in history (§6.2).
	DeriveDone
)

// String renders the state in Russian for the console (no English leaks).
func (s DeriveState) String() string {
	switch s {
	case DeriveNone:
		return "деривация не запущена"
	case DeriveQueued:
		return "деривация в очереди"
	case DeriveRunning:
		return "деривация выполняется"
	case DeriveError:
		return "деривация завершилась ошибкой"
	case DerivePaused:
		return "деривация приостановлена"
	case DeriveDone:
		return "деривация завершена"
	default:
		return fmt.Sprintf("неизвестное состояние деривации (%d)", int(s))
	}
}

// tasksTask is one task object of a tasks.php listing; only the fields
// Wauploader consumes are mapped. task_id is json.Number so a number or a
// numeric string both print verbatim in diagnostics; submittime keeps its
// raw wire text ("YYYY-MM-DD HH:MM:SS" per the API, r2 §6.2) and is parsed
// tolerantly only where a cutoff demands it.
type tasksTask struct {
	TaskID     json.Number `json:"task_id"`
	Cmd        string      `json:"cmd"`
	WaitAdmin  int         `json:"wait_admin"`
	SubmitTime string      `json:"submittime"`
}

// submittimeLayouts are the accepted submittime spellings: the Tasks API's
// "YYYY-MM-DD HH:MM:SS" (UTC) first, RFC3339 as tolerance for zone-stamped
// variants.
var submittimeLayouts = []string{"2006-01-02 15:04:05", time.RFC3339}

// parseSubmittime decodes a tasks.php submittime value; ok is false when
// the value proves nothing about WHEN the task was submitted.
func parseSubmittime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range submittimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// taskIsCurrent reports whether a task counts toward the observation. A
// zero notBefore keeps the pure presence behavior (every task counts). A
// real cutoff admits only tasks submitted at or after it; an unparseable
// submittime proves nothing and is filtered — fail-safe: the caller keeps
// polling instead of acting on a possibly-stale task.
func taskIsCurrent(t tasksTask, notBefore time.Time) bool {
	if notBefore.IsZero() {
		return true
	}
	st, ok := parseSubmittime(t.SubmitTime)
	return ok && !st.Before(notBefore)
}

// taskIDInt64 is a best-effort numeric view of a task_id for
// newest-first selection; an unparseable id ranks as 0 (still selectable
// when it is the only candidate).
func taskIDInt64(t tasksTask) int64 {
	if n, err := t.TaskID.Int64(); err == nil {
		return n
	}
	return 0
}

// DeriveStatus maps the item's derive job onto a DeriveState in one
// tasks.php listing call (GET ?identifier=<id>&catalog=1&history=1&limit=…,
// r2 §6.2) with no submission-time cutoff: every derive task counts,
// whatever batch submitted it. The Tasks API accepts ONLY the S3-key LOW
// header (cookies are rejected by design, §6.1); setAuth touches nothing
// else.
func (c *Client) DeriveStatus(ctx context.Context, identifier string) (DeriveState, error) {
	return c.DeriveStatusSince(ctx, identifier, time.Time{})
}

// DeriveStatusSince is DeriveStatus scoped to the current batch: derive
// tasks submitted BEFORE notBefore are ignored in both catalog and
// history, so a prior batch's done or errored derive can never be
// observed as this batch's result (controller amendment of the plan's
// presence-only done criterion; r2 §6.2 submittime criteria). The zero
// time.Time{} disables the cutoff. Callers pass the moment just before
// their PUT.
func (c *Client) DeriveStatusSince(ctx context.Context, identifier string, notBefore time.Time) (DeriveState, error) {
	st, _, err := c.deriveStatusDetailed(ctx, identifier, notBefore)
	return st, err
}

// deriveStatusDetailed is DeriveStatusSince plus the derive task_id for
// diagnostics (WaitDerive's terminal error names it when present).
func (c *Client) deriveStatusDetailed(ctx context.Context, identifier string, notBefore time.Time) (DeriveState, string, error) {
	if err := ValidateIdentifier(identifier); err != nil {
		return DeriveNone, "", err
	}
	q := url.Values{}
	q.Set("identifier", identifier)
	q.Set("catalog", "1")
	q.Set("history", "1")
	q.Set("limit", strconv.Itoa(tasksPageLimit))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.webBase+"/services/tasks.php?"+q.Encode(), nil)
	if err != nil {
		return DeriveNone, "", fmt.Errorf("опрос задач элемента %s: построить запрос: %w", identifier, sanitizeReqErr(err))
	}
	c.setAuth(req)
	resp, err := c.hc.DoWithRetry(ctx, req, httpx.RetryOpts{Mode: httpx.RetryIdempotent})
	if err != nil {
		return DeriveNone, "", fmt.Errorf("опрос задач элемента %s не выполнен: %w", identifier, sanitizeReqErr(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return DeriveNone, "", fmt.Errorf("опрос задач элемента %s: неожиданный HTTP %d", identifier, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return DeriveNone, "", fmt.Errorf("опрос задач элемента %s: чтение ответа: %w", identifier, err)
	}
	var payload struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Value   struct {
			Catalog []tasksTask `json:"catalog"`
			History []tasksTask `json:"history"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return DeriveNone, "", fmt.Errorf("опрос задач элемента %s: ответ не разобран: %w", identifier, err)
	}
	if !payload.Success {
		reason := strings.TrimSpace(payload.Error)
		if reason == "" {
			reason = "причина не указана"
		}
		return DeriveNone, "", fmt.Errorf("опрос задач элемента %s не выполнен, archive.org вернул ошибку: %s",
			identifier, truncateFragment(reason))
	}

	// Catalog first: an active derive defines the present state. Tasks
	// serialize per item, so among the current-batch catalog derives the
	// NEWEST by task_id is the live one — an older errored task must not
	// mask its queued successor.
	var newest tasksTask
	var haveNewest bool
	var newestID int64
	for _, t := range payload.Value.Catalog {
		if t.Cmd != deriveCmd || !taskIsCurrent(t, notBefore) {
			continue
		}
		if id := taskIDInt64(t); !haveNewest || id > newestID {
			newest, haveNewest, newestID = t, true, id
		}
	}
	if haveNewest {
		switch newest.WaitAdmin {
		case 0:
			return DeriveQueued, newest.TaskID.String(), nil
		case 1:
			return DeriveRunning, newest.TaskID.String(), nil
		case 2:
			return DeriveError, newest.TaskID.String(), nil
		case 9:
			return DerivePaused, newest.TaskID.String(), nil
		default:
			// Unrecognized catalog state (server-side addition): stay
			// non-terminal and keep the caller waiting rather than
			// report a false error or a false success.
			return DeriveRunning, newest.TaskID.String(), nil
		}
	}
	// Done: the derive left catalog and shows in history, submitted
	// within the observed batch (§6.2 + the notBefore cutoff). While any
	// current-batch derive is active the catalog branch above takes
	// precedence.
	for _, t := range payload.Value.History {
		if t.Cmd == deriveCmd && taskIsCurrent(t, notBefore) {
			return DeriveDone, t.TaskID.String(), nil
		}
	}
	return DeriveNone, "", nil
}

// WaitDerive polls DeriveStatusSince every poll until the derive is done,
// the context ends, or timeout elapses. The notBefore cutoff (same
// semantics as DeriveStatusSince; pass the moment just before the PUT)
// keeps prior-batch derives from ending the wait. DeriveError is terminal
// immediately and its Russian error names the derive task_id when the API
// supplied one (the item history page is the diagnostic surface).
// DeriveNone keeps polling: a freshly PUT item's derive only becomes
// visible after ingest. poll and timeout must be positive — invalid values
// fail fast in Russian before any request.
func WaitDerive(ctx context.Context, c *Client, identifier string, notBefore time.Time, poll, timeout time.Duration) error {
	if poll <= 0 {
		return errors.New("ожидание деривации: интервал опроса должен быть положительным")
	}
	if timeout <= 0 {
		return errors.New("ожидание деривации: время ожидания должно быть положительным")
	}
	deadline := time.Now().Add(timeout)
	for {
		st, taskID, err := c.deriveStatusDetailed(ctx, identifier, notBefore)
		if err != nil {
			return err
		}
		switch st {
		case DeriveDone:
			return nil
		case DeriveError:
			if taskID != "" {
				return fmt.Errorf("деривация элемента «%s» завершилась ошибкой (задача %s): подробности на странице истории элемента",
					identifier, taskID)
			}
			return fmt.Errorf("деривация элемента «%s» завершилась ошибкой: подробности на странице истории элемента", identifier)
		}
		if !sleepCtx(ctx, poll) {
			return fmt.Errorf("ожидание деривации элемента «%s» прервано: %w", identifier, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("деривация элемента «%s» не завершилась за отведённое время ожидания", identifier)
		}
	}
}
