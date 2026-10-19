DONE

# R4 — Library/Dependency Selection Report (wauploader)

Date: 2026-10-19. Scope: definitive per-component pick (adopt / stdlib / build-own) with fresh same-session verification of every candidate. All versions/licenses/dates verified TODAY via pkg.go.dev, proxy.golang.org `@latest`, GitHub API (license/pushed_at/open_issues), raw LICENSE files, and go.dev release notes. Owner constraints honored: MIT-compatible deps only, CGO_ENABLED=0, TOML config inline credentials (no env vars), Russian colored TTY with plain fallback + `--json` + `--no-color`, in-house SPN2/IA-S3 clients.

## Status line

**Status: DONE** (no blocking concerns; see UNVERIFIED for 5 open items flagged for the controller).

## Skills invoked (5)

1. `brainstorming` (mandated) — applied as spec-discipline: assumptions surfaced, goals/anti-goals stated per component; no interactive user exists in this subagent slice, so its question loop is satisfied by the UNVERIFIED section going back to the controller.
2. `grilling` (mandated) — applied as design-tree self-check over the 9 components; decision branches that depend on owner judgment are listed as open questions rather than silently assumed.
3. `build-with-exa` (mandated) — Exa used for the judgment-heavy checks; note: in this runtime `tools.exa.web_search_exa` returns a markdown STRING (not an object), first probes came back "empty" until decoded; structured primary-source data (versions/licenses/issues) was verified by direct fetch of proxy.golang.org / GitHub API / raw files, which is stronger evidence than search snippets.
4. `research` (own choice) — findings captured as a single cited Markdown file, per skill contract.
5. `karpathy-guidelines` (own choice) — minimum-dependency bias, no speculative picks, every claim traceable to a fetch made this session.

## Memory files read (coverage disclosure)

**Full reads (every line):**
- `~/.config/opencode/MEMORY.md` (index; read in full)
- `memory/projects/anicli-go.md` (26/26 lines)
- `memory/projects/teleparse.md` (28/28 lines)
- `memory/projects/cindex.md` (49/49 lines)
- `memory/projects/hhauto.md` (110/110 lines — a handful of very long lines were truncated at 2000 chars by the reader; dependency-relevant facts were not in the truncated tails)
- `memory/topics/stack-and-tools.md` (27/27)
- `memory/topics/tech-gotchas.md` (7/7)
- `memory/topics/workflow.md` (41/41)
- `memory/topics/workflow-git-pr.md` (30/30)
- `memory/topics/communication-behavior.md` (17/17)
- `memory/topics/code-gotchas.md` (78/78)

**Exhaustive keyword sweeps (grep across ALL remaining files in topics/ and projects/, hits read; zero-hit files honestly listed):**
- Hits read: `anicli-py.md` (Go-deps block: charm.land v2 versions, modernc v1.58, TOML precedent, netclient retry ladder), `ert.md` (golangci-lint v2 config-schema landmines), `traffic_mimicry.md` (x/net, x/sync, golangci 78→0 wave, govulncheck), `environment-infra.md` (tmpfs/GOTMPDIR landmine), `workflow-git-pr.md`, `home-global.md`, `vibecoding-course.md`, `2026-10-12-slides-novum-organum-plan.md`.
- Zero hits for the dependency keyword set (cobra/toml/sqlite/bbolt/golangci/isatty/lipgloss/bubbletea/progressbar/retryablehttp/humanize/uuid/testify/x-time/x-sync/goreleaser/tls-client/charm.land/fatih-color/mattn): `WMS.md`, `wms.md`, `secureway_bot.md`, `remnavawe_ansible.md`, `freeai-decompile.md`, `freepackages.md`, `habrparser.md`, `interview-kb.md`, `iris-proposal.md`, `kolosyug.md`, `labboardvent.md`, `labs-docker-stands.md`, `labs.md`, `shikimori-japanese-theme.md`, `xray-raw.md`, `tspu-research-2026.md`, `bos-pro-bugs.md`, `2026-10-09-telegram-serverless.md`, `2026-10-11-novum-organum-report-plan.md`, `2026-10-14-opencode-v2-storage.md`, `2026-10-15-binary-re-landmines.md`, `2026-10-15-vision-ocr-scans.md`, `hh-ranking-ats-research.md`, `job-search.md`, `remnawave.md`, `harness-server.md`.

Context-honesty note: the mandate said "read every file in full"; total memory is ~1.2 MB and physically does not fit one subagent context. The above is the maximum honest coverage: full reads of every file governing Go-CLI decisions, exhaustive sweeps proving the rest carry no dependency-relevant facts.

**Memory precedents mined (used as preferences, never as version facts):**
- hhauto deps: cobra 1.10.x, go-toml/v2, modernc sqlite 1.59+, x/net, x/time/rate, tls-client v1.16.0 BSD-4; gates: golangci 0 (gocyclo20/gocognit30/funlen200/nestif5, tests excluded), `-race -p 2`, CGO_ENABLED=0; TTY color gate contract (T53); no-env-vars owner rule; T49/T52 Russian plain console surface; golangci-lint binary must match go toolchain (T96: 2.13.1 fails on go1.27 export data, rebuilt 2.14.0).
- teleparse pinned versions: cobra v1.10.2, modernc sqlite v1.58.0, go-toml/v2 v2.4.3, charm.land v2 line; style rules: wsl, varnamelen≥3, err113 sentinels+%w, testifylint, t.Setenv; local golangci-lint strict gates (Actions banned account-wide).
- anicli-go: netclient custom retry ladder (3 attempts, bounded jitter [250–500 ms, 500 ms–1 s], Retry-After honored), Parallel[T] on errgroup, CGO_ENABLED=0, `-race -p 2` (full parallelism flakes), GOTMPDIR landmine.
- anicli-py (Go-deps section): TOML precedent noted as "either fine; BurntSushi chosen there (reference impl, zero deps)" — mixed with hhauto/teleparse go-toml/v2.
- ert: golangci-lint v2 `issues.max-issues-per-linter` / `max-same-issues` defaults silently cap output — must zero them; `//nolint:<linter-name>` matching rule.
- traffic_mimicry: x/net v0.58.0 / x/sync current-line precedent, golangci enforced gate + govulncheck separate step.

---

# Per-component verdicts

## 1. CLI framework — **ADOPT spf13/cobra**

| Candidate | Fresh facts (2026-10-19) | License | Verdict |
|---|---|---|---|
| spf13/cobra | v1.10.2 (2025-12-03); repo pushed 2026-07-11; 474 open issues; 44.7k★ | Apache-2.0 | **ADOPT** |
| urfave/cli v3 | v3.14.0 (2026-10-02); repo pushed 2026-10-19 (same-day activity); 30 open issues; 24.3k★ | MIT | reject |
| alecthomas/kong | v1.16.1 (2026-08-09); pushed 2026-10-14; 61 open issues; 3.2k★ | MIT | reject |

Justification: owner precedent is twice-deep (hhauto AND teleparse both shipped on cobra v1.10.x). Flag-over-config precedence is a plain pattern: config struct unmarshals first, then each persistent flag with a bound pointer applies only when changed (cobra/pflag `Changed`), a pattern hhauto/teleparse already prove. Shell completion is built-in (`completion bash|zsh|fish|powershell`). `--json` output is orthogonal to the framework (cobra prints; the command returns a typed result that the output layer marshals) — no framework couples `--json` better than another, so the deciding factors are precedent + ecosystem weight. urfave/cli v3 is healthy and lower-issue-count but offers no structural advantage for a two-command tool and breaks precedent for no gain. kong's struct-tag binding is elegant but couples the flag surface to the TOML struct (or forces a parallel struct), which is exactly the coupling a config-primary tool doesn't want.

Rejected: urfave/cli v3 (no structural gain vs owner precedent), kong (struct↔config coupling, smaller ecosystem).

## 2. Colored console — **BUILD-OWN: manual ANSI + golang.org/x/term for TTY detection**

| Candidate | Fresh facts | License | Verdict |
|---|---|---|---|
| manual ANSI (teleparse `internal/cli/style.go` precedent) | owner precedent, ~100 lines | n/a | **BUILD-OWN** |
| golang.org/x/term | v0.47.0 (2026-10-09) | BSD-3-Clause | ADOPT (TTY detect only) |
| mattn/go-isatty | v0.0.24 (2026-07-23); 1 open issue | MIT | reject (redundant) |
| fatih/color | v1.19.0 (2026-03-20); pushed 2026-10-12; 38 open issues | MIT | reject |
| charmbracelet/lipgloss | v1.1.0 (2025-03) / charm.land/lipgloss/v2 v2.0.6 (2026-08-11) | MIT | reject |

Justification: the requirement is exactly the hhauto T53 contract, already owner-approved: color gate = `TTY && !--no-color && NO_COLOR=="" && TERM!=dumb`; non-TTY output byte-for-byte plain; status dictionary red=error✗ / green=success✓ / cyan=info (owner's explicit choice) / yellow=WARN / dim=secondary. That is ~100 lines in an `internal/console` package. fatih/color was verified to auto-handle non-TTY + NO_COLOR (README fetched today: "automatically disable color output for non-tty output streams… also disables color output if NO_COLOR is set"), but its decision is a package-global `color.NoColor` computed at init — honoring a parsed `--no-color` flag still means mutating that global manually, at which point the library adds nothing but a dependency and a mutable global. lipgloss drags the termenv/reflow styling graph into a tool whose entire visual vocabulary is five status colors — disproportionate. RU text: ANSI SGR is encoding-agnostic; no library is needed for Cyrillic. x/term is adopted solely for `IsTerminal` (and optionally terminal width for wrap), which replaces both go-isatty and hand-rolled ioctl.

Rejected: fatih/color (init-time global fights flag parsing; adds a dep for 5 constants), lipgloss v2 (TUI-ecosystem styling graph, disproportionate), go-isatty (x/term covers it; one transport-adjacent dep fewer).

## 3. Batch progress UI — **ADOPT vbauerster/mpb/v8**

| Candidate | Fresh facts | License | Verdict |
|---|---|---|---|
| vbauerster/mpb/v8 | v8.16.2 (2026-09-25); repo pushed 2026-09-26; 12 open issues; 2.5k★; features verified today: multiple bars, dynamic add/remove, cancellation, dynamic total | **Unlicense** (public domain) | **ADOPT** |
| schollz/progressbar/v3 | v3.19.1 (2026-06-30); pushed 2026-09-19; 22 open issues | MIT | reject |
| charm.land/bubbletea/v2 | v2.1.0 (2026-10-08); 265 open issues; 45.5k★ | MIT | reject |

Justification: mpb's core competency is exactly our shape — one bar per concurrent capture/upload worker, N bars managed by one renderer, dynamic add/remove as jobs start/finish, cancellation on Ctrl-C. Byte counters and ETA are first-class (`decor.Counters` + `decor.EwmaETA` / `decor.AverageETA`), and a custom filler wrapping an `io.Reader` gives exact per-file byte accounting for 50+ GB uploads. Non-TTY degradation: mpb renders into an `io.Writer`; the console layer gates the renderer on the same TTY predicate as colors and emits periodic plain status lines instead — the same byte-parity pattern hhauto T53 pinned. License is Unlicense (public domain) — zero friction. schollz/progressbar is single-bar-centric; multiplexing N workers under it is manual work against the library's grain. bubbletea v2 is a full TUI runtime (event loop, update messages, view diffing) — a whole programming model for a rendering concern, and TUI-in-non-TTY is precisely the degradation we must avoid; anicli-go needs it because it is an interactive TUI app, wauploader is a batch CLI.

Rejected: schollz/progressbar/v3 (single-bar grain), bubbletea v2 (disproportionate TUI runtime; wrong degradation model for batch CLI).

## 4. HTTP transport — **STDLIB net/http + custom retry ladder + x/time/rate + x/sync**

### 4.1 Forcing HTTP/1.1 (IA-S3 constraint)

Verified today against go.dev/doc/go1.24: Go 1.24 added `Transport.Protocols` (and `Server.Protocols`) — "provide a simple way to configure what HTTP protocols a server or client use." The clean current idiom (machine toolchain is go 1.27.x, so the API is available):

```go
tr := http.DefaultTransport.(*http.Transport).Clone()
tr.Protocols.SetHTTP1(true)
tr.Protocols.SetHTTP2(false) // IA-S3 speaks HTTP/1.1 REST only
tr.ForceAttemptHTTP2 = false // keep off; Protocols is now the authority
```

Legacy idioms for context: `GODEBUG=http2client=0` (env-level kill switch — useful as a diagnostic override, not as the code path) and pre-1.24 `ForceAttemptHTTP2=false` alone (ambiguous: h2 can still auto-negotiate via TLS ALPN when TLS settings are default). `Protocols` is explicit, per-transport, and testable — cite go.dev/doc/go1.24#nethttp. SPN2 (web.archive.org) speaks h2 fine, but a single h1-only transport shared by both clients is simpler and harmless (h1 performs adequately for our request pattern; no need to run two transports).

### 4.2 Cookies — stdlib net/http/cookiejar suffices

`net/http/cookiejar` (RFC 6265) verified on pkg.go.dev today. `logged-in-sig` / `logged-in-user` are domain cookies on archive.org; one jar attached to one client across both archive.org and web.archive.org stores each host's cookies per RFC 6265 scoping rules — no cross-domain trickery needed because IA-S3 auth is header-based (S3 keys) and the cookie session is only for the web surfaces (xauthn bootstrap lands archive.org cookies). No third-party cookie library.

### 4.3 Retry — **BUILD-OWN ladder; hashicorp/go-retryablehttp REJECTED**

go-retryablehttp v0.7.8 (2025-06-18), MPL-2.0. Its known sharp edges were re-verified OPEN today via GitHub issue search:
- Context-deadline class (all open): **#272** "Backoff wait sleep should check that it wouldn't exceed the context Deadline", **#284** "Don't start backoff timer if it would exceed context deadline", **#292** "skip retry wait when context deadline is sooner".
- Retry-After class (all open): **#295** "DefaultBackoff ignores RetryWaitMax for Retry-After", **#283** "cap Retry-After sleep to RetryWaitMax in DefaultBackoff", **#298** "cap RateLimitLinearJitterBackoff Retry-After waits", **#297** "Reject a Retry-After that overflows the Duration conversion", plus **#167** "Retry after context cancel" (open, old).

For a tool whose whole job is thousands of polite, cancellable, rate-limited requests, both classes are load-bearing — and we'd have to wrap the library to fix them. Owner precedent is already in-house ladders (anicli-go netclient: bounded exponential + jitter + Retry-After; cindex ingestion ladder). ~80 lines in `internal/httpx`: bounded exponential with jitter, honor `Retry-After` capped to retry budget, context-aware sleeps (`timer + ctx.Done()`), per-request budget, retry only idempotent methods + explicit safe-listed POSTs (SPN2 submit is POST — retried only on connect errors before any bytes written, mirroring spn.sh behavior). MPL-2.0 would also be license-acceptable, but the semantics fight us; rejected on engineering grounds.

### 4.4 Rate limiting / concurrency — ADOPT x/time/rate + x/sync

- `golang.org/x/time/rate` — module v0.16.0 (2026-08-19), BSD-3-Clause, verified on pkg.go.dev today. Per-host limiter (SPN2 submission rate, IA-S3 S3-accelerated upload pacing).
- `golang.org/x/sync` — v0.24.0 (2026-09-23), BSD-3-Clause; `semaphore` package verified on pkg.go.dev today (worker-count cap for concurrent captures/uploads); `errgroup` rides in the same module (anicli-go `Parallel[T]` precedent).

## 5. Journal/resume store — **ADOPT go.etcd.io/bbolt**

| Candidate | Fresh facts | License | Verdict |
|---|---|---|---|
| go.etcd.io/bbolt | v1.5.0 (2026-06-03); pushed 2026-10-16; 29 open issues; 9.8k★; LICENSE file verified MIT | MIT | **ADOPT** |
| modernc.org/sqlite | v1.60.1 (2026-09-29), actively maintained (daily-cadence upstream per memory); license verified BSD-3-Clause on pkg.go.dev; pure-Go CGO=0 cross-compile is a proven owner precedent (anicli-go, hhauto, teleparse) | BSD-3-Clause | reject (for now) |
| JSONL + atomic rename | — | n/a | reject |

Reasoning for a thousands-of-URLs, single-writer CLI:
- **Crash recovery**: bbolt is a COW B+tree with two alternating meta pages — a torn write leaves the last consistent snapshot visible; no WAL replay, no journal daemon. This is the strongest zero-config crash story of the three.
- **Single-writer reality**: bbolt takes an exclusive flock on the file — a second concurrent `wauploader` process fails loudly with a clean "database locked" instead of racing. For a resume-oriented batch tool that is a feature, not a limitation.
- **Resume lookup**: keyed `Get` by URL-hash is O(log n) via mmap; thousands of records are trivial. No query planner, no pragmas, no migrations.
- **Zero-dep simplicity**: one dependency, one file, schema evolution = versioned records (first byte of the value = record schema version + re-marshal on read; migration is a scan-and-rewrite bucket pass, explicit and testable).
- **CGO is NOT the differentiator**: modernc sqlite is also pure-Go (CGO_ENABLED=0 verified in three owner projects). The real differentiators: the journal is a single-entity state machine (url → state + timestamps + md5/sha256 + result), not relational data — SQL buys aggregation flexibility we don't need in v1, at the cost of DSN pragmas, migrations, and a query layer. hhauto/anicli-go/teleparse needed relational multi-table stores; wauploader doesn't (yet).
- **JSONL rejected**: random lookup on resume = full scan; torn final line needs its own recovery code; compaction = rewrite-the-world I/O that grows with the job count; hhauto's own anti-YAGNI list (T113) explicitly names JSONL as not-worth-it for this tool class.
- **Flip condition**: if reporting/aggregation over the journal becomes a product feature (per-day success rates etc. beyond simple counters), modernc sqlite is the proven upgrade path — the store is behind an interface, so the swap is localized.

## 6. Config — **ADOPT pelletier/go-toml/v2**

| Candidate | Fresh facts | License | Verdict |
|---|---|---|---|
| pelletier/go-toml/v2 | v2.4.3 (2026-07-05); pushed 2026-08-03; LICENSE file verified MIT today | MIT | **ADOPT** |
| BurntSushi/toml | v1.6.0 (2025-12-18); pushed 2026-08-18; MIT | MIT | reject |

Justification: owner precedent is genuinely mixed — anicli-go picked BurntSushi ("reference impl, zero deps"), but the two NEWEST owner Go CLIs (hhauto, teleparse) both shipped go-toml/v2, which is also the faster parser and has the more active release cadence. Validation ergonomics are OURS, not the library's, either way: required-field fail-fast lives in a `config.Validate()` returning typed sentinel errors (hhauto T48/T74 pattern: both-empty key → fail-fast with Russian message), and Russian error text is produced by the config layer's own checks — TOML parse errors come back with position info from go-toml/v2 and get wrapped with a Russian prefix. Credentials inline in `~/.config/wauploader/wauploader.toml`, zero env lookups, per the fixed owner decision (hhauto T49/T52 proves this shape works end-to-end: file perms 0600 guidance, no env fallback, fail-loud).

## 7. Identity, UUID, size formatting, small utilities

- **Job IDs — ADOPT github.com/google/uuid**: v1.6.0 (2024-01-23), BSD-3-Clause. Repo dormant since 2024-11, which is correct for a finished utility (UUID formatting doesn't rot; open issues are feature requests). A hand-rolled `crypto/rand` 16-byte hex is 10 lines, but uuid.NewString() removes format-bug surface for zero cost. Note: the *authoritative* job id for SPN2 is the server-returned job-id (gospn/spn.sh contract) — local uuids are just correlation keys in the journal.
- **Size formatting — SKIP dustin/go-humanize (v1.1.0, MIT, active — verified) despite it being fine**: the RU console wants its own unit vocabulary (`ГБ`/`МБ`, dot/comma decision, fixed precision) and `humanize.Bytes`/`IBytes` emit English SI/IEC strings either way — a 15-line `formatBytesRU` in `internal/console` matches the owner's console-surface pattern (hhauto T69 template overrides) better than a dep. No dep for 15 lines.
- **NOT worth taking** (explicit anti-picks): viper (env lookups are banned; single TOML is the source of truth), logrus/zap/zerolog (stdlib `log/slog` to file + the typed console surface covers both modes; hhauto precedent), cobra+viper integration (violates no-env rule), yaml libraries (config is TOML), any TLS-fingerprint client (bogdanfinn/tls-client — hhauto needed it because hh.ru fingerprints TLS; archive.org does not — stdlib crypto/tls is correct here).

## 8. Testing stack — **ADOPT testify + golangci-lint strict preset + stdlib fuzz/bench**

- **stretchr/testify** v1.12.1 (2026-08-17), MIT, active — owner gate precedent assumes it (teleparse style rules include testifylint; hhauto suites built on it). Use `require` for fatal preconditions, `assert` otherwise; `testifylint` enforced in golangci.
- **golangci-lint** — latest release verified today: **v2.15.0** (2026-10-16), GPL-3.0. GPL is fine: it is a build TOOL, never linked into the binary — no license implications for the MIT project. Machine landmine from memory: the golangci-lint binary must be rebuilt/bumped to match the go toolchain (system 2.13.1 failed on go1.27 export-data v5; T96 rebuilt 2.14.0; v2.15.0 is current) — pin the binary install in the Makefile/`docs/lint.md` and keep it out of PATH assumptions. Preset recommendation (mirroring the owner's hhauto gate, adapted to golangci v2 config schema):
  - enable: `errcheck`, `govet`, `staticcheck`, `unused`, `gocritic`, `revive`, `ineffassign`, `noctx`, `testifylint`, `wsl`, `varnamelen`, `errchkjson`, `perfsprint`;
  - complexity caps: `gocyclo: 20`, `gocognit: 30`, `funlen: 200`, `nestif: 5` (hhauto values);
  - tests excluded from the gate (hhauto convention);
  - v2 landmines from ert memory: zero out `issues.max-issues-per-linter` and `issues.max-same-issues` (defaults silently cap output), and `//nolint` matches the linter NAME only (not rule IDs).
- **govulncheck** as a separate blocking step (traffic_mimicry precedent), not a golangci linter.
- **Fuzz/bench**: stdlib — `go test -fuzz` (hhauto precedent: 2 fuzz targets; natural candidates here: journal record round-trip, config parser, byte formatter), `go test -bench` + benchstat (hhauto: 7 bench targets). Gate: `gofmt -l`, `go vet ./...`, `CGO_ENABLED=0 go build ./...`, golangci 0 findings, `go test -race -p 2 ./...` (`-p 2` — full parallelism flakes on this machine, anicli-go lesson), GOTMPDIR off /tmp (tmpfs quota landmine).

## 9. Behavior references map (NOT dependencies — verified today, all AGPL/behavior-only where licensed)

| Client area | Single best source | Verification (this session) | License |
|---|---|---|---|
| SPN2 submission, retry classes, rate/lock discipline | `overcast07/wayback-machine-spn-scripts` → `spn.sh` | raw fetch HTTP 200; markers confirmed: `capture_all=on by default`, `parallel='20'`, `resume`, `status_rate$f.txt` / `capture_job_rate$f.txt` / `daily_limit$f.txt` / `lock$f.txt` control files, curl-driven submission + status polling, `no_errors` handling | MIT (repo: 150★, pushed 2025-04-03, 15 open issues) |
| SPN2 job lifecycle typing | `internetarchive/gospn` → `status.go` (+ `spn.go` for submission shape) | repo listing fetched: LICENSE, spn.go, status.go, capture.go, options.go; `status.go` content confirmed: `UserStatus` struct for `/save/status/user` (`DailyCaptures int`), status poll = `GET https://web.archive.org/save/status/{jobID}`, response fields `Timestamp/DurationSec/OriginalURL/Status` | **AGPL-3.0 — behavior reference ONLY** (prior-art report consistent) |
| IA-S3 PUT headers, checksum, skip-if-exists | `jjjake/internetarchive` → `internetarchive/item.py` + `internetarchive/utils.py` | raw fetches HTTP 200; `item.py` confirmed: `upload_file(...)` with `checksum`/`checksum_archive` kwargs, skip-if-already-uploaded via checksum cache, `Content-MD5` header used on verify/delete path, `x-archive-size-hint` header set before IA-S3 URL build; `utils.py` confirmed: `md5(file_object)` chunked hashing ("Reads the file in chunks to handle large files efficiently") + `chunk_size` generator | **AGPL-3.0 — behavior reference ONLY** (repo: 1921★, pushed 2026-10-15) |
| xauthn bootstrap (S3 keys + login cookies) | `jjjake/internetarchive` → `internetarchive/config.py` | raw fetch HTTP 200; confirmed: `get_auth_config(email, password, host='archive.org')` → POST to `.../xauthn/` with `params={'op':'login'}`, `data={email,password}`, `timeout=10`, returns config dict (S3 keys + cookies) | AGPL-3.0 — behavior reference ONLY |

Implementation note from this map: IA-S3 integrity checksums are **MD5** (`Content-MD5`, server-side dedup) — the journal's md5 column is the one IA compares; sha256 stays as a local-integrity extra, not an IA contract field. The 50+ GB hashing path must be streaming (jjjake's chunked `md5()` behavior), single-pass, and computed before upload for skip-if-exists decisions.

---

# Final dependency table

| # | Component | Pick | Version (fresh 2026-10-19) | License | Why |
|---|---|---|---|---|---|
| 1 | CLI framework | `github.com/spf13/cobra` | v1.10.2 (2025-12-03) | Apache-2.0 | Owner precedent ×2; built-in completion; flag-over-config proven |
| 2 | TTY detect | `golang.org/x/term` | v0.47.0 (2026-10-09) | BSD-3-Clause | IsTerminal + width; replaces go-isatty |
| 3 | Console colors | build-own ANSI (~100 lines) | — | — | hhauto T53 gate contract; byte-parity plain fallback; no global mutable state |
| 4 | Progress UI | `github.com/vbauerster/mpb/v8` | v8.16.2 (2026-09-25) | Unlicense | Multi-bar per worker, byte counters, ETA, cancellation; clean io.Writer degradation |
| 5 | HTTP client | stdlib `net/http` | go 1.24+ `Transport.Protocols` | — | `SetHTTP1(true)/SetHTTP2(false)` for IA-S3; cookiejar for session cookies |
| 6 | Retry | build-own ladder (~80 lines) | — | — | retryablehttp's context-deadline (#272/#284/#292) and Retry-After (#295/#283/#298) classes verified OPEN today |
| 7 | Rate limit | `golang.org/x/time/rate` | v0.16.0 (2026-08-19) | BSD-3-Clause | per-host token bucket |
| 8 | Concurrency | `golang.org/x/sync` (semaphore + errgroup) | v0.24.0 (2026-09-23) | BSD-3-Clause | worker cap + fan-out (anicli-go precedent) |
| 9 | Journal store | `go.etcd.io/bbolt` | v1.5.0 (2026-06-03) | MIT | crash-safe COW B+tree, single-writer flock, O(log n) resume lookup, zero migrations; sqlite = proven upgrade path |
| 10 | Config | `github.com/pelletier/go-toml/v2` | v2.4.3 (2026-07-05) | MIT | newest owner CLIs' choice; active; fail-fast via own Validate() |
| 11 | Job UUIDs | `github.com/google/uuid` | v1.6.0 (2024-01-23) | BSD-3-Clause | finished utility, zero risk; server job-id remains authoritative |
| 12 | Testing asserts | `github.com/stretchr/testify` | v1.12.1 (2026-08-17) | MIT | owner gate precedent (+testifylint) |
| 13 | Lint tool | golangci-lint (binary, NOT a dep) | v2.15.0 (2026-10-16) | GPL-3.0 (tool) | strict preset per §8; binary/toolchain pairing landmine noted |
| 14 | RU byte formatting | build-own `formatBytesRU` (~15 lines) | — | — | go-humanize emits English units; console owns its vocabulary |

Total runtime dependencies: **8** (cobra, x/term, mpb/v8, x/time, x/sync, bbolt, go-toml/v2, google/uuid) + testify (test-only). All licenses MIT-compatible (Apache-2.0 and BSD-3-Clause are one-way compatible with an MIT project with notice retention; Unlicense is public domain).

# Sketch go.mod

```go
module github.com/4q4r/wauploader

go 1.27 // owner machines run go 1.27.x; Transport.Protocols needs ≥1.24

require (
    github.com/google/uuid v1.6.0
    github.com/pelletier/go-toml/v2 v2.4.3
    github.com/spf13/cobra v1.10.2
    github.com/stretchr/testify v1.12.1 // test-only
    github.com/vbauerster/mpb/v8 v8.16.2
    go.etcd.io/bbolt v1.5.0
    golang.org/x/sync v0.24.0
    golang.org/x/term v0.47.0
    golang.org/x/time v0.16.0
)
```
(Per owner's no-pinning-in-manifests preference for application builds, exact versions resolve at build time; listed versions are today's verified latest and the compatibility floor.)

# UNVERIFIED / open questions for the controller

1. **bbolt v1.5.0 API drift**: the module moved to a v1.5.0 line (memory precedents used 1.3-era APIs). Whether 1.5 changed `bolt.Open`/`Tx` semantics was NOT checked (release notes not fetched). Implementation task #1: read the v1.4/v1.5 changelog before writing the store.
2. **go-toml/v2 strict mode**: I did not verify whether go-toml/v2 offers a BurntSushi-style undecoded-keys report. The recommended validation pattern (own `Validate()` + fail-fast) doesn't depend on it, but "typo'd config key is silently ignored" is a UX hole worth one check at implementation; if absent, accept it (hhauto lives with it) or add a reflection pass later.
3. **SPN2 official API parameter surface**: spn.sh was verified as the behavior source (capture_all, parallel, resume, rate-lock files), but the exact SPN2 API field/enum names beyond those markers were taken from the prior-art report, not re-derived here. The client implementation must mirror spn.sh/gospn field-for-field as planned.
4. **Exa tool result shape in this runtime**: `web_search_exa` returns a markdown string; my three judgment-search calls technically returned data (mpb profile confirmed in the decoded slice) but the golangci-preset and cobra-vs-urfave searches were not decoded before I switched to primary-source fetches. Verdicts for those rest on versions/activity/issues + owner precedent, which I consider stronger evidence; noting for honesty.
5. **Go toolchain floor**: `go 1.27` in go.mod assumes owner machines (1.27.1/1.27.2 per memory). If go-installability on older toolchains matters, drop the directive to `go 1.24` (Transport.Protocols floor) — implementation should confirm with the owner which floor they want.
