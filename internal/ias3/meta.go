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
//     wait_admin: 0=queued 1=running 2=error 9=paused. The post-upload
//     observation is scoped by a task_id BASELINE (controller ruling on
//     CodeRabbit PR#11, superseding the round-1 submittime approach): the
//     caller captures the largest pre-upload derive task_id via
//     NewestDeriveTaskID just before the PUT; a history derive counts as
//     done only when its task_id is NEWER than the baseline, and ANY
//     catalog task — including the non-derive archive.php ingest — means
//     pending and never falls through to history. Among catalog derives
//     the NEWEST by task_id defines the state (tasks serialize per item).

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

// HasActiveTasks is the reference-client skip guard (r2 §2.3
// --checksum semantics): it reports whether the item currently has ANY
// task in the catalog — any command counts, the derive AND the
// archive.php ingest alike — because while the item is busy the Item
// Metadata answer a skip decision relies on may be stale. History is
// ignored: finished tasks cannot make metadata stale. Envelope and
// transport failures come back as (false, err) — the caller decides
// fail-safe (an undecidable skip is not a skip).
func (c *Client) HasActiveTasks(ctx context.Context, identifier string) (bool, error) {
	listing, err := c.fetchTaskListing(ctx, identifier)
	if err != nil {
		return false, err
	}

	return len(listing.Value.Catalog) > 0, nil
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
	// DeriveDone — left catalog, present in history with a task_id newer
	// than the caller's baseline (§6.2 + the baseline contract).
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
// numeric string both print verbatim in diagnostics and compare
// numerically for the baseline contract.
type tasksTask struct {
	TaskID    json.Number `json:"task_id"`
	Cmd       string      `json:"cmd"`
	WaitAdmin int         `json:"wait_admin"`
}

// tasksListing mirrors the tasks.php response envelope (r2 §6.1) with the
// per-category task arrays of a catalog+history listing.
type tasksListing struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Value   struct {
		Catalog []tasksTask `json:"catalog"`
		History []tasksTask `json:"history"`
	} `json:"value"`
}

// taskIDInt64 is a numeric view of a task_id for baseline comparison and
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
// r2 §6.2) with baseline 0 — the diagnostics view where every derive
// counts, whatever batch submitted it. The Tasks API accepts ONLY the
// S3-key LOW header (cookies are rejected by design, §6.1); setAuth
// touches nothing else.
func (c *Client) DeriveStatus(ctx context.Context, identifier string) (DeriveState, error) {
	return c.DeriveStatusSince(ctx, identifier, 0)
}

// DeriveStatusSince is DeriveStatus scoped to the current upload batch by
// the task_id baseline: the largest derive task_id the item already had
// BEFORE the PUT (capture it with NewestDeriveTaskID just before the
// upload). Resolution: ANY catalog task — including the non-derive
// archive.php ingest — means pending (the derive state comes from the
// newest catalog derive; with no derive queued yet it is DeriveNone) and
// history is never consulted while the catalog is non-empty; with an
// empty catalog, a history derive counts as done only when its task_id is
// NEWER than the baseline — an older one belongs to a prior batch. The
// zero baseline (fresh item / no prior derives) admits any history derive.
func (c *Client) DeriveStatusSince(ctx context.Context, identifier string, baselineTaskID int64) (DeriveState, error) {
	st, _, err := c.deriveStatusDetailed(ctx, identifier, baselineTaskID)
	return st, err
}

// NewestDeriveTaskID returns the largest derive task_id currently visible
// for the item across catalog and history — the baseline a caller
// captures JUST BEFORE a PUT so that WaitDerive can afterwards recognize
// only newer derives as this upload's result. Non-derive tasks (the
// archive.php ingest) are ignored; 0 means the item has no derives at all
// (fresh item).
func (c *Client) NewestDeriveTaskID(ctx context.Context, identifier string) (int64, error) {
	listing, err := c.fetchTaskListing(ctx, identifier)
	if err != nil {
		return 0, err
	}

	maxID := int64(0)

	for _, t := range append(listing.Value.Catalog, listing.Value.History...) {
		if t.Cmd != deriveCmd {
			continue
		}

		if id := taskIDInt64(t); id > maxID {
			maxID = id
		}
	}

	return maxID, nil
}

// fetchTaskListing performs the one catalog+history listing call shared by
// the derive observation and the baseline capture: identifier validation,
// LOW-key auth (cookies rejected by this API), the full retry ladder, and
// the envelope parse with its Russian failure surfaces.
func (c *Client) fetchTaskListing(ctx context.Context, identifier string) (tasksListing, error) {
	var payload tasksListing
	if err := ValidateIdentifier(identifier); err != nil {
		return payload, err
	}

	q := url.Values{}
	q.Set("identifier", identifier)
	q.Set("catalog", "1")
	q.Set("history", "1")
	q.Set("limit", strconv.Itoa(tasksPageLimit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.webBase+"/services/tasks.php?"+q.Encode(), nil)
	if err != nil {
		return payload, fmt.Errorf("опрос задач элемента %s: построить запрос: %w", identifier, sanitizeReqErr(err))
	}

	c.setAuth(req)

	resp, err := c.hc.DoWithRetry(ctx, req, httpx.RetryOpts{Mode: httpx.RetryIdempotent})
	if err != nil {
		return payload, fmt.Errorf("опрос задач элемента %s не выполнен: %w", identifier, sanitizeReqErr(err))
	}

	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return payload, fmt.Errorf("опрос задач элемента %s: неожиданный HTTP %d", identifier, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return payload, fmt.Errorf("опрос задач элемента %s: чтение ответа: %w", identifier, err)
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("опрос задач элемента %s: ответ не разобран: %w", identifier, err)
	}

	if !payload.Success {
		reason := strings.TrimSpace(payload.Error)
		if reason == "" {
			reason = "причина не указана"
		}

		return payload, fmt.Errorf("опрос задач элемента %s не выполнен, archive.org вернул ошибку: %s",
			identifier, truncateFragment(reason))
	}

	return payload, nil
}

// deriveStatusDetailed is DeriveStatusSince plus the derive task_id for
// diagnostics (WaitDerive's terminal error names it when present). The
// resolution order encodes the wire reality that tasks serialize per item
// and a PUT's own ingest (archive.php) runs before the derive queues:
//
//  1. catalog non-empty → pending, never history (state from the newest
//     catalog derive, or DeriveNone while only ingest work is visible);
//  2. catalog empty → newest history derive with task_id > baseline → done;
//  3. otherwise → DeriveNone.
func (c *Client) deriveStatusDetailed(ctx context.Context, identifier string, baselineTaskID int64) (DeriveState, string, error) {
	payload, err := c.fetchTaskListing(ctx, identifier)
	if err != nil {
		return DeriveNone, "", err
	}

	// Catalog first: ANY task means the item is busy — the observation
	// stays non-terminal and must never fall through to history while
	// work is in flight.
	if len(payload.Value.Catalog) > 0 {
		var newest tasksTask

		var haveNewest bool

		var newestID int64

		for _, t := range payload.Value.Catalog {
			if t.Cmd != deriveCmd {
				continue
			}

			if id := taskIDInt64(t); !haveNewest || id > newestID {
				newest, haveNewest, newestID = t, true, id
			}
		}

		if !haveNewest {
			// Busy with other work (e.g. the PUT's own ingest); the
			// derive has not queued yet — keep the caller waiting.
			return DeriveNone, "", nil
		}

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

	// Catalog empty: done means a derive NEWER than the pre-PUT baseline
	// finished into history — an older one belongs to a prior batch.
	var done tasksTask

	var haveDone bool

	var doneID int64

	for _, t := range payload.Value.History {
		if t.Cmd != deriveCmd {
			continue
		}

		if id := taskIDInt64(t); id > baselineTaskID && (!haveDone || id > doneID) {
			done, haveDone, doneID = t, true, id
		}
	}

	if haveDone {
		return DeriveDone, done.TaskID.String(), nil
	}

	return DeriveNone, "", nil
}

// WaitDerive polls DeriveStatusSince every poll until the derive is done,
// the context ends, or the wait budget (timeout) is spent. The baseline
// task_id (same semantics as DeriveStatusSince; capture it with
// NewestDeriveTaskID just before the PUT) keeps prior-batch derives from
// ending the wait. DeriveError is terminal immediately and its Russian
// error names the derive task_id when the API supplied one (the item
// history page is the diagnostic surface). DeriveNone keeps polling: a
// freshly PUT item's derive only becomes visible after its ingest task
// finishes. poll and timeout must be positive — invalid values fail fast
// in Russian before any request.
//
// The budget is a real context: every poll runs under it, so an in-flight
// request is cut off when the budget expires (the wait never overshoots
// by a full poll cycle), and a cancelled parent reads as an interruption,
// distinct from budget exhaustion.
func WaitDerive(ctx context.Context, c *Client, identifier string, baselineTaskID int64, poll, timeout time.Duration) error {
	if poll <= 0 {
		return errors.New("ожидание деривации: интервал опроса должен быть положительным")
	}

	if timeout <= 0 {
		return errors.New("ожидание деривации: время ожидания должно быть положительным")
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	budgetErr := func() error {
		return fmt.Errorf("деривация элемента «%s» не завершилась за отведённое время ожидания", identifier)
	}
	interruptedErr := func() error {
		return fmt.Errorf("ожидание деривации элемента «%s» прервано: %w", identifier, ctx.Err())
	}

	for {
		st, taskID, err := c.deriveStatusDetailed(wctx, identifier, baselineTaskID)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return interruptedErr()
			case errors.Is(err, context.DeadlineExceeded):
				return budgetErr()
			default:
				return err
			}
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

		if !sleepCtx(wctx, poll) {
			if ctx.Err() != nil {
				return interruptedErr()
			}

			return budgetErr()
		}
	}
}
