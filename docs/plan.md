# wauploader Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go CLI (`wauploader`) that batch-saves pages/files to web.archive.org (SPN2 API) and batch-uploads files to archive.org items (IA-S3 API) — API-only, no browser emulation, every documented parameter exposed, robust against the services' slowness and error storms, with resume across runs.

**Architecture:** Thin in-house API clients (`internal/spn2`, `internal/ias3`) over a shared hardened HTTP layer (`internal/httpx`: HTTP/1.1 transport, cookie jar, custom retry ladder, per-host rate limiting). A bbolt-backed journal (`internal/journal`) drives idempotent resume; worker-pool runners (`internal/runner`) execute save/upload state machines; a Russian colored console (`internal/console`) is the only user-facing surface (plain in non-TTY, `--json` for machines). Credentials come from TOML (no env), with an `xauthn` bootstrap (`wauploader init`).

**Tech Stack:** Go 1.27, cobra v1.10.x, net/http (stdlib) + x/term + x/time/rate + x/sync, bbolt v1.5, go-toml/v2, mpb/v8, google/uuid, testify (test-only), golangci-lint v2 (tool, not dep). Full selection rationale: `docs/research/r4-libs-report.md`.

**Spec:** the four research reports in `docs/research/` (r1 SPN2, r2 IA-S3, r3 prior art, r4 libraries). They are verified against primary sources (official SPN2 doc 2026-07-22 + changelog, ias3.html, internetarchive sources) and are the binding authority for every wire-level constant below.

---

## Global Constraints

- Module `github.com/4q4r/wauploader`; Go toolchain ≥ 1.27 (`Transport.Protocols` needs ≥ 1.24); builds with `CGO_ENABLED=0`.
- License MIT (`LICENSE` at repo root). Only the 8 runtime deps + testify from the r4 table; adding any dependency requires explicit owner approval.
- Config: single file `~/.config/wauploader/wauploader.toml` (perm 0600 when the tool writes it). **No environment variables for config or credentials, ever.** Flags override config; unset required values = fail-fast with a Russian message.
- Console: Russian, human-readable, no time=/level= prefixes, no slang, full words. Color gate: `TTY && !--no-color && NO_COLOR=="" && TERM!="dumb"`; non-TTY output is byte-for-byte plain. `--json` global flag switches to machine output (one JSON object per event/result).
- Secrets (S3 keys, cookies, passwords) are never logged, never printed, never put into error messages or JSON events. `wauploader init` never stores the password.
- Every HTTP request carries a descriptive User-Agent: `wauploader/<version> (+https://github.com/4q4r/wauploader)` (archive.org Bots policy, r3 F3.9), overridable in config.
- Retries: only via `internal/httpx` ladder — bounded exponential with full jitter, honor `Retry-After` (capped), context-aware sleeps; POST retried only on connect-phase failures. Default client pacing ≤ 80 % of server limits (save: 5/min authenticated, 2/min anonymous; upload: `check_limit` preflight).
- Tests never touch live services: no real `/save` submissions, no real IA-S3 PUTs. Wire behavior is tested with `httptest` fakes and the documented `x-archive-simulate-error` semantics. Live smoke tests are gated behind `-tags live` AND an explicit flag, default off.
- Git: one topic = one branch = one PR, squash-merge; never push to main directly. Subagent worktrees only in `/tmp/wauploader-wtNN`, scratch in `/tmp/wauploader-scratch-*`.
- Code comments/identifiers in English; console strings and config validation messages in Russian.

## Review Focus

1. **SPN2 refusal arrives as HTTP 200 with `{"status":"error"}` and no `job_id`** — a client checking only HTTP status + job_id would report success and poll forever. Expect: journaled terminal failure with the parsed `status_ext`. Test in Task 6 (`TestSubmitRefusalEnvelope`), exercised in Task 10.
2. **IA-S3 503 whose body contains `appears to be spam`** — must be terminal, never retried, unlike plain 503 SlowDown. Test in Task 7 (`TestUploadSpamSlowDownTerminal`).
3. **Backoff sleep vs context deadline / Retry-After** — a Ctrl-C or deadline during a retry wait must return promptly, never sleep past the deadline. Test in Task 4 (`TestRetryContextCancelDuringBackoff`, `TestRetryAfterCapped`).
4. **Interrupted multi-GB PUT** — no resume exists; re-run must restart that file from byte 0 safely (idempotent overwrite with `x-archive-keep-old-version:1`) while other files of the item keep their journal state. Test in Task 11 (`TestUploadResumeRestartsFailedFileOnly`).
5. **Cookie expiry mid-batch (SPN2 with cookie auth)** — cookies "tend to expire after a few days" (official doc); a mid-run 401 must surface as a clear auth error and pause the runner, not loop. Test in Task 10 (`TestRunnerAuthFailurePausesQueue`).
6. **Typo'd/unknown TOML key** — silently ignored keys hide misconfiguration. Expect: unknown-key detection with a Russian warning naming the key. Test in Task 2 (`TestLoadUnknownKeyReported`).

---

## File Structure

```
wauploader/
├── go.mod, go.sum, LICENSE, README.md, Makefile, .gitignore, .golangci.yml
├── main.go                      # cobra root wiring, global flags, exit codes
├── internal/
│   ├── version/version.go       # version string (ldflags-overridable)
│   ├── config/config.go         # TOML structs, Load, Validate, flag-merge helpers
│   ├── console/console.go       # TTY/color gate, ANSI status dictionary, FormatBytesRU
│   ├── httpx/client.go          # h1-only transport, cookie jar, UA, redirect policy
│   ├── httpx/retry.go           # retry ladder (jitter, Retry-After, ctx-aware)
│   ├── auth/auth.go             # credential resolution, whoami check, xauthn bootstrap
│   ├── spn2/client.go           # POST /save + options, GET/POST status endpoints
│   ├── spn2/errors.go           # status_ext taxonomy → Retryable/Quota/Terminal
│   ├── ias3/headers.go          # x-archive-* header builder (meta00/uri() encoding)
│   ├── ias3/upload.go           # PUT, check_limit preflight, SlowDown/spam handling
│   ├── ias3/meta.go             # Metadata API read, tasks/derive polling, whoami
│   ├── journal/journal.go       # bbolt store, versioned records, state machine
│   ├── runner/save.go           # save pipeline: submit→poll→result, resume, stalls
│   ├── runner/upload.go         # upload pipeline: hash→preflight→PUT→verify→derive
│   ├── runner/runner.go         # worker pool, limiters, progress hooks
│   └── commands/                # init.go, save.go, upload.go, status.go, jobs.go
└── docs/
    ├── plan.md                  # this file
    └── research/r1..r4*.md      # spec basis (committed)
```

---

### Task 1: Repository scaffold

**Files:** Create `go.mod`, `LICENSE` (MIT, copyright `4q4r`), `.gitignore` (Go + `wauploader` binary + `*.bbolt`), `main.go`, `internal/version/version.go`, `docs/` (already present — commit it).

**Interfaces:**
- Produces: `internal/version/version.go` → `const Name = "wauploader"`; `var Version = "dev"` (overridable via `-ldflags "-X ...version.Version=x"`); `func UserAgent() string` → `wauploader/<Version> (+https://github.com/4q4r/wauploader)`.

- [ ] **Step 1:** `git init` in `~/projects/wauploader`; create files above; `go mod init github.com/4q4r/wauploader` with `go 1.27`; add cobra `v1.10.2`; `main.go` builds a root cobra command `wauploader` with global persistent flags `--config string` (default `~/.config/wauploader/wauploader.toml`), `--json`, `--no-color`, and subcommand `version`.
- [ ] **Step 2:** `go build ./...` → binary compiles; `./wauploader version` prints `wauploader version dev`.
- [ ] **Step 3:** Commit `chore: scaffold module, cobra root, MIT license, docs`.

### Task 2: Config package

**Files:** Create `internal/config/config.go`, `internal/config/config_test.go`.

**Interfaces:**
- Produces:
```go
type Config struct {
    Auth    AuthConfig    // s3_access, s3_secret, cookie_user, cookie_sig
    SPN2    SPN2Config    // if_not_archived_within string; capture_all, capture_outlinks, capture_screenshot, delay_wb_availability, force_get, skip_first_archive, outlinks_availability, email_result bool; js_behavior_timeout int (default 5); auth_mode string ∈ {"auto","keys","cookies"} (default "auto": keys when present, else cookies)
    Upload  UploadConfig  // collection, mediatype string (REQUIRED, no defaults); queue_derive bool (default true); keep_old_version bool (default true); interactive_priority bool (default false)
    Limits  LimitsConfig  // save_per_minute (default 5), save_concurrency (default 4), upload_concurrency (default 2), per_host_concurrency (default 3), job_wait (default "20m")
    Console ConsoleConfig // reserved (color handled by flags/env)
}
func Default() *Config
func Load(path string) (*Config, error)                       // missing file = zero-config, not an error; parse errors carry TOML position, wrapped with Russian prefix
func (c *Config) Validate(mode Mode) error                    // ModeSave | ModeUpload | ModeAny; Russian fail-fast messages
func (c *Config) MergeFlag(fn func(*Config))                  // flag-over-config hook
```
- Validation rules (exact): upload without `collection` → error naming the flag `--collection` and explaining items must belong to a collection (fail-fast, owner decision 2026-10-19); `mediatype` must be one of `texts, etree, audio, movies, software, image, data, web`; both `s3_access` and `s3_secret` must be set together; `js_behavior_timeout` ∈ [0, 30]; unknown duration formats rejected with examples (`"3d 5h 20m"`, `"120"`).
- Produces: `func DetectUnknownKeys(path string) ([]string, error)` — decodes into `map[string]any` and diffs against known key paths (Review Focus #6).

- [ ] **Step 1:** Failing tests: `TestDefaultValues`, `TestValidateUploadRequiresCollection` (message contains `--collection`), `TestValidateMediatypeEnum`, `TestValidateKeysPaired`, `TestLoadUnknownKeyReported`, `TestLoadParseErrorHasPosition`.
- [ ] **Step 2:** `go test ./internal/config/ -v` → FAIL (not implemented).
- [ ] **Step 3:** Implement. TOML via `pelletier/go-toml/v2`; durations accepted in IA timedelta form — store raw string in `SPN2Config`, parse helper `ParseIATimedelta(s string) (time.Duration, error)` supporting `"120"` seconds and `"3d 5h 20m"` units `d h m s`.
- [ ] **Step 4:** `go test ./internal/config/ -v` → PASS.
- [ ] **Step 5:** Commit `feat: config package with fail-fast validation`.

### Task 3: Console package

**Files:** Create `internal/console/console.go`, `internal/console/console_test.go`.

**Interfaces:**
- Produces:
```go
func New(out, errOut io.Writer, isTTY bool, noColor bool) *Console
func IsTTY(f *os.File) bool                                    // via x/term IsTerminal
func (c *Console) Infof(format string, a ...any)               // cyan
func (c *Console) Successf(format string, a ...any)            // green
func (c *Console) Warnf(format string, a ...any)               // yellow
func (c *Console) Errorf(format string, a ...any)              // red → errOut
func (c *Console) Dimf(format string, a ...any)                // dim (hashes, ids)
func (c *Console) JSON(v any)                                  // one compact JSON object per call, colors never applied
func FormatBytesRU(n int64) string                             // "1,5 ГБ" style: RU units Б/КБ/МБ/ГБ/ТБ, one decimal, comma separator
```
- Color gate exactly: `isTTY && !noColor && os.Getenv("NO_COLOR")=="" && os.Getenv("TERM")!="dumb"`; when false, every method writes the same text with zero escape bytes (byte-parity test).
- Status glyphs: `✓` success, `✗` error, `?` warning/attention, `·` dim info (hhauto T53 dictionary).

- [ ] **Step 1:** Failing tests: `TestColorGatePlainInNonTTY` (no 0x1b bytes when gate false), `TestColorGateDumbTerm`, `TestJSONNeverColored`, `TestFormatBytesRUBoundaries` (0, 1023, 1024, 1048576, 5*2^30, negative → error path uses abs).
- [ ] **Step 2:** `go test ./internal/console/` → FAIL.
- [ ] **Step 3:** Implement with raw ANSI SGR constants; `t.Setenv` for env cases.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit `feat: Russian console surface with TTY color gate`.

### Task 4: HTTP layer (transport, cookies, redirects, retry)

**Files:** Create `internal/httpx/client.go`, `internal/httpx/retry.go`, `internal/httpx/client_test.go`, `internal/httpx/retry_test.go`.

**Interfaces:**
- Consumes: `config.Config` (UA override), `version.UserAgent()`.
- Produces:
```go
type Client struct { HTTP *http.Client }
func NewClient(jar http.CookieJar, userAgent string) *Client   // h1-only transport: tr.Protocols SetHTTP1(true)/SetHTTP2(false), ForceAttemptHTTP2=false
func (c *Client) DoWithRetry(ctx context.Context, req *http.Request, o RetryOpts) (*http.Response, error)
type RetryOpts struct { MaxAttempts int; BaseWait, MaxWait time.Duration } // defaults: 5 attempts, 1s, 60s
```
- Redirect policy: follow up to 10; re-attach `Authorization` header iff redirect target host is `archive.org` or ends with `.archive.org` (IA-S3 307s; mirrors internetarchive's `rebuild_auth`); strip it otherwise.
- Retry ladder: bounded exponential `BaseWait * 2^(n-1)` + full jitter, cap `MaxWait`; explicit `Retry-After` honored (seconds or HTTP-date) and capped to `MaxWait`; every sleep is `select { <-timer.C; <-ctx.Done() }`; retries on transport connect errors, `429`, and `5xx` except `501`; request body must be replayable (caller passes `GetBody`-capable requests; document that POST retried only when nothing was written — connect-phase only, per SPN2 ambiguity r1 F7.7).
- Produces: `ErrRetryBudgetExceeded`, `ErrContextDeadline` sentinel errors (`errors.Is`-able).

- [ ] **Step 1:** Failing tests: `TestTransportForcesHTTP1` (inspect cloned transport protocols), `TestRedirectKeepsAuthOnArchiveOrg` / `TestRedirectStripsAuthElsewhere` (httptest 302 chains), `TestRetryOn502ThenSuccess`, `TestRetryAfterCapped` (server sends `Retry-After: 3600`; test asserts total wait < 5s), `TestRetryContextCancelDuringBackoff` (cancel mid-wait; error within 100 ms), `TestRetryBudgetExceeded`.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement. No third-party retry library (r4 verdict: retryablehttp sharp edges #272/#284/#292/#295 are load-bearing here).
- [ ] **Step 4:** PASS (incl. `go test -race`).
- [ ] **Step 5:** Commit `feat: hardened HTTP client with HTTP/1.1 pinning and ctx-aware retry ladder`.

### Task 5: Auth package + `wauploader init`

**Files:** Create `internal/auth/auth.go`, `internal/auth/auth_test.go`, `internal/commands/init.go`.

**Interfaces:**
- Consumes: `config.Config.Auth`, `httpx.Client`, `config.Load/Validate`.
- Produces:
```go
type Credentials struct { Access, Secret, CookieUser, CookieSig string }
func Resolve(cfg *config.Config) (Credentials, error)           // keys present → use; else clear Russian error with exact TOML keys to fill or "wauploader init" hint
func (c Credentials) ValidateKeys(ctx context.Context, cl *httpx.Client) error // GET https://archive.org/services/user.php?op=whoami, Authorization: LOW; success:false → invalid keys
func (c Credentials) SPN2Request(req *http.Request)             // sets Authorization: LOW (preferred) — cookie path only when keys empty
func Bootstrap(ctx context.Context, cl *httpx.Client, email, password string) (Credentials, error)
// POST https://archive.org/services/xauthn/?op=login, form email+password, timeout 30s
// success: values.s3.access/secret + values.cookies.logged-in-user/logged-in-sig (+ values.screenname)
// failure: values.reason ∈ {account_not_found, account_bad_password} → Russian messages; sleep 2s after the call (reference behavior)
func SaveToConfig(path string, c Credentials, screenname string) error // merges [auth] into TOML, chmod 0600, never writes password
```
- Command: `wauploader init` — prompts email + password (terminal, no echo via `x/term.ReadPassword`; non-TTY → actionable error: how to edit TOML by hand, with the exact keys and the s3.php URL). Never prints keys/cookies back in full (mask: first 4 chars + length).

- [ ] **Step 1:** Failing tests with httptest fake xauthn: `TestBootstrapSuccess` (parses all four values), `TestBootstrapBadPassword` (Russian message), `TestBootstrapMissingReasonField` (raw error surfaced), `TestSaveToConfigPerms0600` + merge preserves unrelated sections, `TestResolveFailsFastWithHint`, `TestValidateKeysWhoami` (success + invalid cases). Secrets never appear in test stderr (helper asserts).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit `feat: auth resolution, xauthn bootstrap, wauploader init`.

### Task 6: SPN2 client

**Files:** Create `internal/spn2/client.go`, `internal/spn2/errors.go`, `internal/spn2/spn2_test.go`, test fixtures `internal/spn2/testdata/*.json` (doc-verbatim payloads).

**Interfaces:**
- Consumes: `httpx.Client`, `auth.Credentials`.
- Produces:
```go
const BaseURL = "https://web.archive.org/save"
type Options struct { // every documented POST parameter; false/empty = omitted
    CaptureAll, CaptureOutlinks, CaptureScreenshot, DelayWBAvailability bool
    ForceGet, SkipFirstArchive, OutlinksAvailability, EmailResult bool
    IfNotArchivedWithin string        // raw IA timedelta passthrough, validated by config.ParseIATimedelta
    JSBehaviorTimeout int             // 0..30; 0 → send js_behavior_timeout=0
    CaptureCookie, UseUserAgent, TargetUsername, TargetPassword string
}
func (o Options) Encode() url.Values                              // "1"/"on" semantics: true → "1"; anything else is OFF server-side (doc warning)
type SubmitResult struct { JobID string; Deduped bool; Message string }
func (c *Client) Submit(ctx context.Context, targetURL string, o Options) (SubmitResult, error)
func (c *Client) Status(ctx context.Context, jobID string) (Status, error)
func (c *Client) StatusBatch(ctx context.Context, jobIDs []string) ([]Status, error)   // POST job_ids=csv
func (c *Client) StatusOutlinks(ctx context.Context, parentJobID string) ([]Status, error)
func (c *Client) UserStatus(ctx context.Context) (available, processing int, err error) // GET /save/status/user?_t=<epoch-ms>
func (c *Client) SystemStatus(ctx context.Context) (ok bool, overloaded bool, err error) // HTTP 502 = critical
type Status struct { State string /* pending|success|error */; JobID, OriginalURL, Timestamp string
    DurationSec float64; Resources []string; Outlinks []string
    OutlinkJobs map[string]string; OutlinkTimestamps map[string]string
    Screenshot string; DownloadSize int64; HTTPStatus int; FirstArchive bool
    StatusExt, Message, Exception string }
const SnapshotURLFmt = "https://web.archive.org/web/%s/%s"
```
- `Submit` rules (load-bearing): response parsed as JSON regardless of HTTP status; `{"status":"error", no job_id}` → typed error (Review Focus #1); `job_id:null` + dedup message → `SubmitResult{Deduped:true}` (success semantics, journal as done-with-existing); `job_id` treated as opaque string (UUID or `spn2-<hex>` forms). Auth selection: `config.SPN2Config.AuthMode` — `"auto"` uses `Authorization: LOW` when keys are configured, else the two cookies; `"keys"`/`"cookies"` force one path (cookie mode with no cookies → Russian fail-fast naming the exact TOML keys).
- Error taxonomy (`errors.go`): `type Kind int` = `KindRetryable | KindQuota | KindTerminal`; `type Error struct{ Kind; StatusExt, Message string; HTTP int }`; full `status_ext` map from r1 F4.1 (all ~35 codes) with classification: retryable — `cannot-fetch, proxy-error, protocol-error, read-timeout, browsing-timeout, celery, job-failed, internal-server-error, no-browsers-available, capture-location-error, too-many-requests`; quota (park until window) — `too-many-daily-captures, user-session-limit, max-daily-bandwidth, max-daily-bandwidth-from-ip, max-daily-bandwidth-host`; terminal — `blocked-url, blocked, blocked-client-ip, invalid-url-syntax, invalid-host-resolution, not-found, no-access, unauthorized, filesize-limit, ftp-access-denied, bandwidth-limit-exceeded, bad-request, http-version-not-supported, method-not-allowed, not-implemented, network-authentication-required, gateway-timeout, bad-gateway, service-unavailable, invalid-server-response, soft-time-limit-exceeded`. Unknown `status_ext` → `KindRetryable` with a warning (fail-open for new server codes, message logged). Message-based classification for codes the doc omits: parse delays from `"Your capture will begin in NNs."` → `func ParseDelayMessage(msg string) (time.Duration, bool)`; retryable markers `"Crawling this host is paused"`, `"Cannot start capture"`; terminal marker block-list sentence `"Save Page Now service block list"`; hard-limit marker `"currently facing some limitations"`.

- [ ] **Step 1:** Fixtures from the official doc verbatim: success (with outlinks map + screenshot), success (outlinks array + outlinks_availability shape), pending (with `download_size`), error (`invalid-host-resolution`), refusal envelope `{"message":"...","status":"error","status_ext":"error:invalid-host-resolution"}` (no job_id), dedup `job_id:null` message. Failing tests: `TestSubmitSuccess`, `TestSubmitRefusalEnvelope` (Kind inferred, no poll loop), `TestSubmitDedupMessage`, `TestOptionsEncodeTrue1FalseOmitted`, `TestOptionsJSZeroStillSent`, `TestStatusPayloadShapes` (both outlink forms), `TestErrorClassification` (table over all documented status_ext codes), `TestParseDelayMessage`, `TestUserStatusCacheBuster` (assert `?_t=` present and changes).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement. `Content-Type: application/x-www-form-urlencoded;charset=UTF-8`; `Accept: application/json`; auth per `auth.Credentials.SPN2Request`.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit `feat: SPN2 client with full parameter surface and error taxonomy`.

### Task 7: IA-S3 upload core

**Files:** Create `internal/ias3/headers.go`, `internal/ias3/upload.go`, `internal/ias3/ias3_test.go`.

**Interfaces:**
- Consumes: `httpx.Client`, `auth.Credentials`, `config.ParseIATimedelta` not needed here.
- Produces:
```go
const S3Base = "https://s3.us.archive.org"
func ValidateIdentifier(id string) error   // [A-Za-z0-9._-], first char alnum, len 5..80, reject <id>_meta.xml / <id>_files.xml key collisions
type Meta map[string][]string              // metadata fields → ordered values
type HeaderOpts struct {                   // one struct = one PUT's control surface
    Collection, Mediatype string           // mediatype required by config, collection required by config
    Title, Description string              // optional convenience metadata
    Extra Meta                             // arbitrary x-archive-meta-* fields (validated: ^[A-Za-z][.\-0-9A-Za-z_]+$, values uri()-encoded when non-ASCII)
    AutoMakeBucket, QueueDerive, KeepOldVersion, InteractivePriority, IgnorePreexistingBucket bool
    SizeHint int64                         // whole-item total bytes, set on first PUT
}
func (h HeaderOpts) headers() http.Header  // meta00/meta01 numbering for multi-values, "--"→"_" in names, uri(...) encoding for non-ASCII, exact names: x-archive-auto-make-bucket, x-archive-queue-derive, x-archive-size-hint, x-archive-meta-mediatype, x-archive-meta01-collection, x-archive-keep-old-version, x-archive-interactive-priority, x-archive-ignore-preexisting-bucket
type UploadRequest struct { Identifier, Key, Path string; Size int64; MD5Hex string }
func (c *Client) CheckLimit(ctx context.Context, identifier string) (overLimit bool, err error)
    // GET S3Base/?check_limit=1&accesskey=...&bucket=... ; JSON-parse failure = overloaded (fail-safe true)
func (c *Client) Upload(ctx context.Context, req UploadRequest, o HeaderOpts, progress func(sent int64)) error
    // single PUT; Content-MD5 = hex digest (IA accepts hex); Transfer-Encoding: chunked NEVER set (read file, known length)
    // 503 SlowDown → retry (caller's ladder off; dedicated loop: sleep 30s, re-check CheckLimit, re-PUT whole file)
    // 503 body contains "appears to be spam" → terminal ErrSpamSlowDown, NEVER retried
    // 403 InvalidAccessKeyId / "bad request" → ErrBadCredentials; XML <Error><Code>/<Message> parsed into typed error; invalid XML 5xx → retryable
func awaitNotOverLimit(ctx, c, identifier) error // poll loop for the preflight
```
- Identifier charset errors list every violated rule in the Russian message.

- [ ] **Step 1:** Failing tests: `TestHeadersExactNames` (golden header map incl. meta01 numbering + `uri()` encoding of `Тест ☃` + `--`→`_`), `TestValidateIdentifierRules` (too short, bad first char, bad charset, 81 chars, meta.xml collision), `TestUploadSuccessWithContentMD5` (httptest asserts header + body bytes), `TestUploadSlowDownRetryThenSuccess`, `TestUploadSpamSlowDownTerminal` (Review Focus #2), `TestUploadBadCredentials`, `TestUpload503InvalidXMLBodyRetryable`, `TestCheckLimitOverLimit`, `TestCheckLimitGarbageIsOverloaded`, `TestNoChunkedEncoding` (assert `Transfer-Encoding` absent + `Content-Length` set).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit `feat: IA-S3 PUT with full x-archive-* surface and SlowDown/spam handling`.

### Task 8: IA-S3 metadata + tasks + integrity

**Files:** Create `internal/ias3/meta.go`, `internal/ias3/meta_test.go`.

**Interfaces:**
- Consumes: `httpx.Client`, `auth.Credentials`.
- Produces:
```go
type ItemFile struct { Name, MD5, SHA1, CRC32 string; Size int64 }
func (c *Client) ItemFiles(ctx context.Context, identifier string) ([]ItemFile, error)
    // GET https://archive.org/metadata/<id> → files[]; empty item → empty slice, not error
func (c *Client) FileExistsMatchingMD5(ctx, identifier, key, md5Hex string) (bool, error)  // skip-if-exists primitive
func (c *Client) WhoAmI(ctx context.Context) error              // same endpoint as auth.ValidateKeys; kept private to ias3 tests — canonical validation lives in auth (Task 5)
func (c *Client) DeriveStatus(ctx context.Context, identifier string) (DeriveState, error)
    // GET https://archive.org/services/tasks.php?identifier=<id>&catalog=1&history=1 (S3 keys ONLY — cookies rejected by this API)
    // wait_admin: 0=queued 1=running 2=error 9=paused; left catalog + present in history = done
    // envelope {"success":bool,"error":...,"value":...}; rate-limits endpoint ?rate_limits=1&cmd=derive.php for diagnostics
func WaitDerive(ctx context.Context, c *Client, identifier string, poll time.Duration, timeout time.Duration) error
```

- [ ] **Step 1:** Failing tests with fixtures: `TestItemFilesParses`, `TestItemFilesEmptyItem`, `TestFileExistsMatchingMD5`, `TestDeriveStates` (queued/running/error/paused/done), `TestDeriveErrorEnvelope`, `TestWaitDeriveTimeout`.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit `feat: IA metadata read, skip-if-exists primitive, derive polling`.

### Task 9: Journal (bbolt)

**Files:** Create `internal/journal/journal.go`, `internal/journal/journal_test.go`.

**Interfaces:**
- Produces:
```go
func Open(path string) (*Store, error)     // bbolt single-writer flock: second open → Russian error "журнал занят другим процессом wauploader"
func (s *Store) Close() error
type Kind string  // "save" | "upload"
type State string // "queued" | "submitted" | "pending" | "done" | "failed" | "skipped" | "quota_parked"
type Record struct {
    Key string        // hex(sha256(kind + "\x00" + target)); save target = URL, upload target = abs path + "\x00" + identifier + "/" + key
    Kind Kind; Target string
    State State; Attempts int
    SPN2JobID string; SnapshotURL string; SnapshotTimestamp string
    MD5, SHA256 string; Size int64
    ErrKind string; ErrMsg string
    UpdatedAt time.Time
    SchemaVersion uint8 // = 1; unknown version on read → explicit Russian migration error, never silent
}
func (s *Store) Put(r *Record) error; func (s *Store) Get(kind Kind, target string) (*Record, bool, error)
func (s *Store) UpdateState(kind Kind, target string, st State, mutate func(*Record)) error
func (s *Store) ByStates(kinds []Kind, states []State) ([]Record, error)  // resume scan
func (s *Store) Counts() (map[Kind]map[State]int, error)                  // console summary
```

- [ ] **Step 1:** Failing tests: `TestPutGetRoundTrip`, `TestKeyStableAcrossReopen` (close, reopen, Get), `TestSecondOpenLocked` (Russian message), `TestByStatesResumeScan`, `TestCounts`, `TestUnknownSchemaVersionError`, `TestConcurrentSingleWriter` (one writer goroutine serialized OK; two `Open` → lock error).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement (bbolt v1.5: read its changelog first — r4 UNVERIFIED #1; gob or manual binary encoding behind one marshal function so schema bumps are one-file changes). Store path default `{config dir}/wauploader.bbolt` → `~/.local/share/wauploader/journal.bbolt` (XDG data dir), overridable by flag `--journal`.
- [ ] **Step 4:** PASS incl. `-race`.
- [ ] **Step 5:** Commit `feat: bbolt journal with versioned records and single-writer lock`.

### Task 10: Save runner

**Files:** Create `internal/runner/runner.go` (shared pool), `internal/runner/save.go`, `internal/runner/save_test.go`.

**Interfaces:**
- Consumes: `spn2.Client`, `journal.Store`, `config.LimitsConfig`, `console.Console`, `httpx` limiters.
- Produces:
```go
type Event struct { Kind string; Target string; State journal.State; JobID, SnapshotURL, ErrMsg string; Attempt int }
type SaveRunner struct { /* ... */ }
func NewSaveRunner(sp *spn2.Client, st *journal.Store, lim Limits, con *console.Console) *SaveRunner
type Limits struct { PerMinute rate.Limit; Concurrency int; PerHost int; JobWait time.Duration }
func (r *SaveRunner) Run(ctx context.Context, targets []string, opts spn2.Options) (<-chan Event, func() error)
    // returns event stream + a completion func returning Summary{Done, Failed, Parked, Skipped}
```
- Pipeline per target (state machine, journaled at every transition): `queued` → (global limiter + semaphore + per-host limiter keyed by target host, cap `PerHost` ≤ 5 so we never approach the >20-same-host trigger) → `Submit` → `submitted` (persist job_id immediately — status retention is 1h) → poll `/save/status/<job>` with exponential backoff 2s→30s, detect stalls via `download_size` plateau over 3 polls → `done` (persist `original_url`+`timestamp`, build `SnapshotURL`) / `failed` (KindTerminal) / `quota_parked` (KindQuota: leave queued for the next run, message with local-midnight hint) / dedup → `done` with `SnapshotURL` from existing capture. `user-session-limit` → back off: query `UserStatus`, wait while `processing >= available`.
- Resume: `Run` prepends journal records in `queued|pending|submitted` states; records with a live `SPN2JobID` skip straight to polling; targets already `done` are reported as `skipped` unless `--force`.
- Auth failure (`error:unauthorized`/HTTP 401 on any call): pause the whole queue (close limiter), emit one clear Russian event, return non-zero (Review Focus #5).

- [ ] **Step 1:** Failing tests against an in-process fake SPN2 server (`httptest`): `TestHappyPathSingleURL`, `TestPendingPollsUntilSuccess` (fake flips pending→success), `TestStallDetectionDownloadSizePlateau`, `TestQuotaParksUntilNextRun`, `TestDedupIsDone`, `TestResumeSkipsDoneAndPollsPending`, `TestPerHostLimiterCapsConcurrency` (fake asserts ≤ cap simultaneous), `TestGlobalRateLimit`, `TestAuthFailurePausesQueue`, `TestJobWaitTimeoutMarksFailed`, `TestChaos502StormRecovers` (fake emits 502s on submit then recovers).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement (errgroup + `x/sync/semaphore` + `x/time/rate`; per-host map guarded by mutex).
- [ ] **Step 4:** PASS incl. `-race -p 2`.
- [ ] **Step 5:** Commit `feat: save runner with resume, parking, stall detection and politeness caps`.

### Task 11: Upload runner

**Files:** Create `internal/runner/upload.go`, `internal/runner/upload_test.go`.

**Interfaces:**
- Consumes: `ias3` client functions, `journal.Store`, `Limits`, `console.Console`.
- Produces:
```go
type UploadTarget struct { Path string; Identifier, Key string }  // Key defaults to filepath.Base; identifier required
func NewUploadRunner(cl *ias3.Client, st *journal.Store, lim Limits, con *console.Console) *UploadRunner
func (r *UploadRunner) Run(ctx context.Context, targets []UploadTarget, o ias3.HeaderOpts, verify, skipExisting bool) (<-chan Event, func() error)
```
- Pipeline per file: hash once (streaming MD5+SHA256 in a single pass, chunked — r4 note) → journal → skip-if-exists (`FileExistsMatchingMD5`, plus "item has no running tasks" guard from r2 F4.2) → `CheckLimit` preflight (await while over limit) → PUT (whole file, restart from 0 on failure — no resume exists) → optional verify (re-fetch `files[].md5` after derive settles) → derive watch optional flag `--wait-derive`.
- Ordering: strict sequence within one identifier (never parallel PUTs into the same item — size-hint/mixed-header landmines), parallelism only across distinct identifiers (`upload_concurrency`).
- Item bootstrap: first file of a new item carries `AutoMakeBucket` + `SizeHint` = total bytes of all files bound for that item + collection/mediatype/meta headers; subsequent files carry `QueueDerive:false` except the last.

- [ ] **Step 1:** Failing tests: `TestSinglePassHashing` (stream from temp file; md5 matches reference), `TestItemFileOrderSequential` (fake asserts no overlapping PUTs per identifier), `TestParallelAcrossItems`, `TestSkipExistingByMD5`, `TestFailedFileRestartsFromZeroOthersKept` (Review Focus #4: fail file 2 of 3, re-run → only file 2 re-uploads, order preserved), `TestSizeHintOnFirstPUTOnly`, `TestQueueDeriveFalseExceptLast`, `TestProgressCallbackCounts`.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS incl. `-race`.
- [ ] **Step 5:** Commit `feat: upload runner with per-item sequencing, dedup and resume`.

### Task 12: Commands wiring + JSON mode

**Files:** Create `internal/commands/save.go`, `internal/commands/upload.go`, `internal/commands/status.go`, `internal/commands/jobs.go`; modify `main.go`.

**Interfaces:**
- Consumes: everything above.
- Produces (CLI surface, final):
```
wauploader init                                          # Task 5
wauploader save (URL... | --list FILE) [save flags]      # flags mirror spn2.Options 1:1:
    --capture-all --capture-outlinks --capture-screenshot --delay-wb-availability
    --force-get --skip-first-archive --outlinks-availability --email-result
    --if-not-archived-within DURATION --js-behavior-timeout N
    --capture-cookie COOKIE --use-user-agent UA --target-username U --target-password P
    --force          # re-run even if journal says done
wauploader upload (FILE... | --list FILE) --item ID [--key NAME] [upload flags]
    --collection C --mediatype M        # required (fail-fast), no defaults (owner decision)
    --meta NAME=VALUE (repeatable) --title T --description D
    --no-derive --no-keep-old-version --interactive-priority --ignore-preexisting-bucket
    --verify --wait-derive --checksum-skip
    --identifier-from PATH_TEMPLATE     # optional; else --item required
wauploader status [JOB_ID | --user | --system]
wauploader jobs [--state failed|queued|pending|done] [--clear]
```
- Global: `--json` emits one JSON object per Event / per status result (no colors, no RU decorations); `--dry-run` (on `save`/`upload`): resolve targets, apply journal/`--force` logic, validate config and credentials presence, print every would-be action («Сохранить: …», «Загрузить: … → item/key»), touch NOTHING on the network and NOT the journal; exit code 0 when all targets would run, 1 when some would skip/fail. Exit codes: `0` = all succeeded, `1` = some jobs failed/parked, `2` = config/usage error, `3` = auth error. `--list FILE` accepts one target per line, `#` comments; blank lines skipped.
- Console examples (implementer may refine wording, keep structure): `✓ Сохранено: https://example.com → https://web.archive.org/web/20260101010101/https://example.com`, `✗ Ошибка (error:blocked-url): https://… — URL в блок-листе Wayback`, `? Квота исчерпана (error:too-many-daily-captures) — задача отложена до завтра`, final summary `Итог: сохранено N · пропущено N · отложено N · ошибок N`.

- [ ] **Step 1:** Failing tests: `TestSaveFlagsMapToOptions` (each flag flips exactly one Options field), `TestListFileParsing` (comments/blanks), `TestExitCodes` (0/1/2/3), `TestJSONModeEvents`, `TestStatusUserJSON`, `TestUploadRequiresItemOrTemplate`, `TestDryRunTouchesNothing` (fake servers count zero mutation requests; journal file absent), `TestEndToEndFake` (spin both fake servers, run save of 3 URLs incl. one terminal error + upload of 2 files; assert journal states, exit code 1, summary line).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit `feat: command surface with JSON mode and exit-code contract`.

### Task 13: Docs + gates

**Files:** Create `README.md` (owner readme standard: centered header with emoji title + tagline, for-the-badge shields rows, quick-links line, emoji TOC, mermaid request-flow diagram, project tree with inline comments, `| :-- |` tables — reference style 4q4r/cindex), `docs/config-example.toml` (annotated), `Makefile`, `.golangci.yml`.

**Interfaces:**
- Makefile targets: `build` (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ...version.Version=$(VER)"`), `test` (`GOTMPDIR=$(HOME)/.cache/go-tmp go test -race -p 2 ./...`), `lint` (golangci-lint run), `vuln` (govulncheck ./...), `gate` = fmt → vet → lint → test → vuln, each a standalone blocking command (never chained with `&&` into a release step).
- `.golangci.yml` (v2 schema): enable `errcheck, govet, staticcheck, unused, gocritic, revive, ineffassign, noctx, testifylint, wsl, varnamelen, errchkjson, perfsprint`; caps `gocyclo: 20, gocognit: 30, funlen: 200, nestif: 5`; tests excluded; `issues.max-issues-per-linter: 0`, `issues.max-same-issues: 0` (v2 silent-cap landmine); binary version pinned in `docs/lint.md` note (must match go toolchain — memory landmine T96).

- [ ] **Step 1:** Write files; `make build` OK; `gofmt -l .` empty; `go vet ./...` clean.
- [ ] **Step 2:** `make lint` → 0 findings (fix what it finds, one commit per mechanical fix batch).
- [ ] **Step 3:** `make test` → full suite green with `-race -p 2`.
- [ ] **Step 4:** `make vuln` → no known vulns in deps.
- [ ] **Step 5:** Commit `docs: readme, config example, quality gates` (PR → squash-merge per repo pipeline).

### Task 14 (optional, owner-gated): Live smoke in test_collection

**Files:** Create `internal/commands/livetest.go` with `//go:build live` + manual runbook in `docs/live-runbook.md`.

- Only after owner provides credentials in local config. Sequence: `wauploader init` → upload one small file to `test_collection` with `--wait-derive` → verify via `ItemFiles` md5 → `save` one URL (`if_not_archived_within=1h` to stay dedup-polite) → verify status endpoint success. Deletes are NEVER automated (`delete.php` is irreversible — out of scope by design).
- [ ] **Step 1:** Runbook written; build tag makes the command invisible in normal builds.
- [ ] **Step 2:** Owner runs it manually; findings → follow-up tasks.

---

## Risks / open items carried from research

- SPN2 anonymous byte-quota doc self-contradicts (500MB vs 2GiB) — mitigated by treating `error:max-daily-bandwidth-*` as the only truth; no magic numbers in code.
- `js_behavior_timeout` doc inconsistency (7s table vs 30s param text) — we send what the user configures, validated 0..30, surface server errors verbatim.
- Doc-internal `error:bad-request` says "HTTP status=401" — treat as target-side wording, classify terminal as documented.
- bbolt v1.5 API drift unknown (r4 UNVERIFIED #1) — Task 9 step 3 starts with a changelog read.
- IA-S3 exact behavior for PUT-to-existing-bucket with meta headers (403 vs `_meta.xml` overwrite, r2 UNVERIFIED #9) — `--ignore-preexisting-bucket` exposed as a flag; default OFF; behavior documented in README; first live run (Task 14) observes and we amend if needed.
