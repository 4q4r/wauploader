DONE

# r3 — Prior-art inventory, robustness patterns, Go ecosystem gap analysis (wauploader)

Research agent report, 2026-10-19. Scope: prior art + robustness patterns worth adopting + Go ecosystem gap analysis for a greenfield Go CLI that (a) saves pages via Wayback SPN2 and (b) uploads files to archive.org items via IA-S3. No live captures/uploads were performed; all facts are from primary sources fetched in this session.

## Skills invoked

1. `brainstorming` — request is fully specified by the dispatch brief (research-only, output contract fixed); no build gate applies, no user questions needed.
2. `build-with-exa` — Exa MCP (`web_search_exa` + `web_fetch_exa`) as the primary research path per memory rule exa-only-search.
3. `grilling` — design tree fully settled by the output contract; no open user decisions surfaced that the brief doesn't already answer.
4. `research` — primary-source discipline: every named library/repo verified by fetching the repo or official docs this session.
5. `planning-with-files` — LOADED BUT PARTIALLY DEVIATED: the skill normally writes `task_plan.md`/`findings.md`/`progress.md` into the project dir; the dispatch output contract forbids writing anywhere except the report file, so the phase plan was kept in-context (5 phases = the 5 deliverable sections). Disclosed here as the required deviation note.

## Memory files read (full coverage, verified list)

`~/.config/opencode/MEMORY.md` (in full, 60 lines).

Topics (`~/.config/opencode/memory/topics/`) — all 17:
1. stack-and-tools.md
2. workflow.md
3. workflow-git-pr.md
4. environment-infra.md
5. code-gotchas.md
6. communication-behavior.md
7. tech-gotchas.md
8. 2026-10-15-vision-ocr-scans.md
9. 2026-10-15-binary-re-landmines.md
10. 2026-10-14-opencode-v2-storage.md
11. 2026-10-12-slides-novum-organum-plan.md
12. 2026-10-11-novum-organum-report-plan.md
13. 2026-10-09-telegram-serverless.md
14. harness-server.md
15. hh-ranking-ats-research.md
16. job-search.md
17. remnawave.md

Projects (`~/.config/opencode/memory/projects/`) — all 26:
1. anicli-go.md
2. anicli-py.md
3. cindex.md
4. hhauto.md
5. ert.md
6. home-global.md
7. freepackages.md
8. interview-kb.md
9. traffic_mimicry.md
10. tspu-research-2026.md
11. kolosyug.md
12. labboardvent.md
13. habrparser.md
14. xray-raw.md
15. shikimori-japanese-theme.md
16. bos-pro-bugs.md
17. labs-docker-stands.md
18. labs.md
19. vibecoding-course.md
20. teleparse.md
21. remnavawe_ansible.md
22. wms.md
23. WMS.md
24. iris-proposal.md
25. secureway_bot.md
26. freeai-decompile.md

Memory facts that shaped this research: Exa-only search discipline with retry-on-timeout; every library verified via fresh upstream fetch before asserting; Go deps verified in past projects (tls-client v1.16.0 BSD-4, modernc sqlite, charm.land v2, BurntSushi/go-toml precedent); /tmp tmpfs quota caution (report is small, no risk).

---

## 1. Python `internetarchive` CLI (jjjake/internetarchive)

**F1.1 — Repo exists, actively maintained, current major v5.x.** README (fetched from raw master today) carries a live security notice "upgrade to v5.4.2+ immediately" (directory-traversal fix in `File.download()`), installs via pipx, docs at archive.org/services/docs/api/internetarchive.
Source: https://raw.githubusercontent.com/jjjake/internetarchive/master/README.rst — **confidence: HIGH**.

**F1.2 — SPN2 saving is NOT covered.** The official CLI page documents download / search / metadata / upload etc. — no save command. A feature request to add an SPN2 sub-command is still OPEN: issue #771 "Add a command to help use the SPN2 api", opened 2026-05-22. So "does it cover SPN2 (ia save / session.save)?" → **No, not as of 2026-10**; SPN2 in Python lives in the separate EDGI `wayback` library (see F1.7).
Sources: https://archive.org/developers/internetarchive/cli.html ; https://github.com/jjjake/internetarchive/issues/771 — **confidence: HIGH**.

**F1.3 — Uploads fully covered, with retry/checksum knobs.** Documented API `upload(...)`: `retries` ("Number of times to retry the given request if S3 returns a 503 SlowDown error"), `retries_sleep` ("Amount of time to sleep between retries"), `checksum` ("Skip uploading files based on checksum"), `verify` ("Verify local MD5 checksum matches the MD5 checksum of the file received by IAS3"), `queue_derive=False` to skip derive, `delete` after verified upload. CLI: `ia upload <id> file --retries 10`; clobbered files are backed up to `history/files/$key.~N~` unless `-H x-archive-keep-old-version:0`.
Sources: https://archive.org/developers/internetarchive/python-lib.html ; https://archive.org/developers/internetarchive/cli.html — **confidence: HIGH**.

**F1.4 — Retry/backoff defaults are NOT set in the config file.** The configuration page documents only `[s3]`, `[cookies]`, `[general]` (incl. `user_agent_suffix`), `[custom]`; there are no documented `[http] retries/backoff_factor` keys. Retry control exists at three code surfaces: `upload(retries=, retries_sleep=)`, `search_items(max_retries: int | Retry)` (accepts a raw `urllib3.Retry`), and `ArchiveSession.mount_http_adapter(protocol, max_retries, status_forcelist, ...)` ("If you need more control (e.g. status_forcelist), use an ArchiveSession object and mount your own adapter"). Exact numeric defaults inside `mount_http_adapter` were not captured in my fetches — flagged UNVERIFIED (see §7).
Sources: https://archive.org/developers/internetarchive/configuration.html ; https://archive.org/developers/internetarchive/python-lib.html ; https://archive.org/developers/_modules/internetarchive/session.html — **confidence: HIGH for the surfaces, UNVERIFIED for exact default numbers**.

**F1.5 — Session reuse pattern.** `ArchiveSession` subclasses `requests.Session` (connection pooling, cookie persistence, config persistence across operations); created via `get_session(config_file=...)` or env vars `IA_ACCESS_KEY_ID` + `IA_SECRET_ACCESS_KEY` (both required together; env takes precedence over the file). Noteworthy robustness detail: `rebuild_auth()` is overridden so the Authorization header is NOT stripped on redirects within archive.org hosts (IA-S3 loves 307 redirects) while cross-host redirects keep requests' default stripping.
Sources: https://archive.org/developers/internetarchive/python-lib.html ; https://archive.org/developers/internetarchive/configuration.html ; session source https://raw.githubusercontent.com/jjjake/internetarchive/master/internetarchive/session.py — **confidence: HIGH**.

**F1.6 — License caution.** `internetarchive/session.py` header: ":license: AGPL 3". The library itself is AGPL-3 — fine to *use* as a CLI, but its code must not be copied into a permissively-licensed Go tool (behavior reference only).
Source: https://raw.githubusercontent.com/jjjake/internetarchive/master/internetarchive/session.py — **confidence: HIGH**.

**F1.7 — The canonical Python SPN2 client is EDGI's `wayback` lib, and its defaults are documented.** `WaybackSession(retries=6, backoff=2, timeout=60, user_agent=None, search_calls_per_second=0.4, memento_calls_per_second=8)`; backoff formula "seconds = backoff * 2^(retry number - 1), first retry immediate". Its source carries a load-bearing comment: *"Global default rate limits for various endpoints. Internet Archive folks have asked us to set the defaults at 80% of the hard limits"* → CDX 0.8×30/60 = 0.4/s and Memento 0.8×600/60 = 8/s. This is the best available primary evidence of IA's hard read-API limits (30/min CDX, 600/min memento) and of the "set your client to 80% of the limit" practice. Repo per its default UA: github.com/edgi-govdata-archiving/wayback.
Sources: https://wayback.readthedocs.io/en/stable/usage.html ; https://wayback.readthedocs.io/en/latest/_modules/wayback/_client.html — **confidence: HIGH**.

**F1.8 — SPN2 error handling in internetarchive:** does not exist (follows from F1.2). IA's own `openlibrary` calls SPN2 with a small ad-hoc helper (`save_page_now()` returning job_id or error strings) — evidence that even IA-internal projects hand-roll SPN2 rather than going through `internetarchive`.
Source: https://github.com/internetarchive/openlibrary/blob/master/openlibrary/core/ia.py — **confidence: MEDIUM-HIGH** (helper seen in source tree search result).

---

## 2. Go ecosystem inventory

**F2.1 — `internetarchive/gospn` — the official IA Go SPN2 client (exists, real, quiet).** Repo github.com/internetarchive/gospn, created 2024-11-14, 8★, license **AGPL-3.0**, latest release **v1.0.4 published 2025-05-25** (pkg.go.dev default). API: `spn.Init(accessKey, secretKey)` → `client.Capture(url, spn.CaptureOptions{SkipFirstArchive: true, IfNotArchivedWithin: "3d", ...})` (returns immediately with job_id) + `GetCaptureStatus(job_id)` + `Close()`, `GOSPN_DEBUG=1` for debug. Contributors include vbanos (the SPN2 API doc author) and yzqzss — canonical provenance. Maintenance: alive but no release since 2025-05-25.
Sources: https://pkg.go.dev/github.com/internetarchive/gospn ; https://github.com/internetarchive/gospn — **confidence: HIGH**.
License consequence: AGPL-3 is viral — linking gospn forces the wauploader to be AGPL-3-compatible. For a personal MIT/BSD tool, rewrite (~small API surface) is cheaper than the license.

**F2.2 — `RaghavSood/spn2` — minimal MIT Go SPN2 client+CLI (exists, dormant).** Created 2023-12-18, 1★, 3 commits by one author, MIT. Features: save / status / system-status / user-status with S3-method auth (`SPN2_ACCESS_KEY`/`SPN2_SECRET_KEY`). Useful as an MIT-licensed *reference shape*, too thin to adopt.
Source: https://github.com/RaghavSood/spn2 — **confidence: HIGH**.

**F2.3 — `wabarc/wayback` — the big Go archiver (exists, healthy, wrong license).** 2241★, GPL-3.0, created 2020-06, active; multi-service: Internet Archive + archive.today + Ghostarchive + IPFS + Telegraph, batch wayback, IM-bot daemons (Telegram/Discord/Matrix/IRC/XMPP), Tor hidden service, prometheus metrics. IA leg delegates to the separate module `github.com/wabarc/archive.org` (GPL-3.0, 4★, created 2020-06) via `ia.Archiver{Client: ingress.Client()}` with errgroup fan-out per URL × slot.
Sources: https://github.com/wabarc/wayback ; https://github.com/wabarc/archive.org ; https://github.com/wabarc/wayback/blob/main/wayback.go — **confidence: HIGH**.
Coverage gaps vs wauploader: no IA-S3 upload at all (capture-only), no checksum dedup, no resume journal; whether its IA capture uses the SPN2 JSON API or scrapes `/save` was NOT confirmed from source in this session (README only shows output URLs on web.archive.org) — flagged UNVERIFIED. GPL-3 → code must not be copied into a non-GPL tool.

**F2.4 — `internetarchive/Zeno` — official IA crawler in Go (exists, active, AGPL).** 398★, AGPL-3.0, 47 releases, latest **v2.0.23 (2026-04-09)**, last push 2026-04-14; records to WARC via `internetarchive/gowarc`; note the CGO landmine: full speed needs GCC 12+ for ada-url/goada (WHATWG URL parser), CGO-free wasm fallback is ~1x on amd64/arm64 but **10x+ slower on other arches**. Relevant as a *design reference* (queueing, rate limiting, WARC pipeline), not as a capture-uploader dependency.
Source: https://github.com/internetarchive/zeno/ — **confidence: HIGH**.

**F2.5 — `internetarchive/wayback` — the Wayback Machine software itself** (Java; the thing SPN2 feeds). Its issue tracker is the living SPN2-doc errata: #305 (2026-09-28) and #304 (2026-09-13) document doc/behavior divergences — see §3. Not a dependency; useful to watch.
Source: https://github.com/internetarchive/wayback/issues — **confidence: HIGH**.

**F2.6 — Go IA-S3 upload libraries: none found.** The official IA-S3 docs name only the Python library ("The internet archive maintains a python library … see jjjake/internetarchive"). Targeted searches for Go IA-S3 upload packages surfaced no maintained library — only capture-only `wabarc/archive.org`. **This is the core gap wauploader must fill.**
Sources: https://archive.org/developers/ias3.html ; search round W2 — **confidence: HIGH (absence claim bounded by two search angles)**.

**F2.7 — ArchiveBox-related Go tooling: none of substance.** ArchiveBox is Python-only; no maintained Go port/variant found. Closest stray: `MathiasDPX/goarchiver` (created 2025-12-13, 0★, 1 contributor, WARC crawler + mitmproxy replay addon) — irrelevant to SPN2/IA-S3.
Sources: https://github.com/MathiasDPX/goarchiver ; search round W4 — **confidence: MEDIUM-HIGH**.

**F2.8 — License compatibility summary for reuse.** Official/canonical Go IA code is copyleft: gospn AGPL-3, Zeno AGPL-3, wabarc GPL-3. Permissive SPN2 references: RaghavSood/spn2 (MIT), the public API spec itself (free to implement). Verdict: an MIT wauploader should implement SPN2/IA-S3 clients itself (the wire surface is small and fully documented) and treat gospn/wabarc/Zeno as behavior references only.

---

## 3. Community knowledge on limits (SPN2 quotas, concurrency, timeouts, spam triggers; IA-S3 speed)

Primary source for all SPN2 limits: the official "Save Page Now 2 Public API Docs" (Vangelis Banos, updated 2022-04-05), Google Doc id `1Nsv52MvSjbLb2PCpHlat0gkzw0EvtSgpKHu4mk0MnrA`, captured here via the public mirror https://gist.github.com/regstuff/82e690db2f1d91ba59f6681c1abad6cf — **confidence: HIGH for everything quoted below (verbatim table content)**.

**F3.1 — Auth tiers.** Exactly two documented auth methods for `https://web.archive.org/save`: (1) **S3 API keys — "highly preferable"** via header `Authorization: LOW myaccesskey:mysecret` (keys at archive.org/account/s3.php); (2) **browser cookies** `logged-in-sig` + `logged-in-user` — "not desirable because they tend to expire after a few days". The dispatch's "LOW S3 / HIGH cookie" tier framing is NOT in the docs; only anonymous-vs-authenticated matters (numbers below). Marked as unverified community shorthand in §7.

**F3.2 — Concurrency caps.** "Max concurrent captures for authenticated users = 12 and for anonymous API users = 6" — pending-status jobs; exceeding returns an error.

**F3.3 — Capture durations (corrects the "~30 min" folklore).** Network connect timeout 10s; **max web-page capture time 50s** (browser + JS); **max total capture duration 2m**; outlink-extraction flow budget ~90s (FAQ Q2). The only "30 min" in the docs is the **default of `if_not_archived_within`** (dedup window), not a timeout. SPN2 captures are minutes-scale; the client's job is to *poll* `/save/status/{job_id}`, not to hold a 30-minute timeout. (js_behavior_timeout: default 5s; the Limitations table says "max 7s configurable" while the param section says "max value that applies is 30 sec" — internal doc inconsistency, report as-is.)

**F3.4 — Daily quotas.** Anonymous: **4,000 captures/day**; authenticated: **100,000/day** ("if you need more, contact info@archive.org"); **per-URL: 10 captures/day**.

**F3.5 — The per-URL daily cap is messy in practice (critical client fact).** `internetarchive/wayback` issue #305 (2026-09-28, verified by fetching the issue): the docs state the per-URL cap as **10** in the error-code table and **5** in the Limits section, and live observation (2026-09-27, authenticated POST for a PDF) returned *"This URL has been already captured 1 times today, which is a daily limit we have set for that Resource type"* — a per-resource-type cap that appears nowhere in the docs; other clients report the same message with "5 times today". **Second critical fact from the same issue: a refused capture request arrives as HTTP 200 with a JSON body `{"status":"error","status_ext":"error:too-many-daily-captures","message":"..."}` and NO `job_id`** — corroborated across three independent clients (jordibrouwer/nextdash, meedan/pender, MKRWW/wortlaut). A client checking only HTTP status + job_id will report "accepted, nothing started". wauploader MUST parse the response body of POST /save.
Sources: https://github.com/internetarchive/wayback/issues/305 — **confidence: HIGH**.

**F3.6 — Spam-detection triggers / soft blocks.**
- Target blocking: `error:blocked` (site blocks IA, HTTP 999); anonymous clients whose IPs are on **Spamhaus XBL/SBL** get `error:blocked-client-ip` (Tor exits exempt); URL blocklist based on **Mozilla web-tracker lists** → `error:blocked-url`.
- Same-host throttling (server-side artificial delay): >20 concurrent captures on the same host (in the last 60s) → delay new captures by `concurrent_count/5` seconds (e.g. 50 concurrent → 10s delay).
- Target-side 429 → SPN2 delays subsequent captures for that host by 10–20s for 60s.
- FAQ Q1: "Live page is not available" is frequently a false positive caused by IA's own concurrent load on the target — retry later is the correct response.
- Full `status_ext` error taxonomy exists (bad-gateway, browsing-timeout, filesize-limit 2GB, no-browsers-available, celery, …) — worth encoding as typed Go errors. Note: #304 documents the docs already miss at least one code (`error:no-captures`).

**F3.7 — Useful request parameters for a batch tool.** `capture_all=1` (capture 4xx/5xx), `capture_outlinks=1` (max 100 outlinks, ranked PDF/Epub first), `capture_screenshot=1` (≤4MB else skipped), `delay_wb_availability=1` (reduces IA load; result appears ~12h later), `skip_first_archive=1` (faster), `if_not_archived_within=<dur>` (dedup; supports "3d 5h 20m" or seconds; default 30 min), `js_behavior_timeout=0` (big speedup for static pages), `force_get=1` (skip HEAD+browser), `email_result=1`, `capture_cookie=` / `target_username=`/`target_password=` for gated targets. Official tips: do NOT use capture_outlinks casually; prefer capturing specific outlinks yourself.

**F3.8 — IA-S3 speed/concurrency guidance (official).**
- Endpoint: PUT to `https://s3.us.archive.org/<item>/<file>` (bucket-style `<item>.s3.us.archive.org`); PUT can create the item (`x-archive-auto-make-bucket:1`).
- **Multipart uploads supported.** Only HTTP/1.1 REST; **HTTP 1.1 Range headers ignored**; DELETE bucket not allowed; ACLs fake (world-readable, uploader-writable).
- **307 redirects are much more common than on Amazon S3 → clients with good 100-Continue support matter** (docs explicitly call this out; curl ≥7.19 praised, ≥7.58 needs `--location-trusted` to keep Authorization on redirect).
- Metadata via `x-archive-meta-<name>` headers (uri() encoding for non-ASCII; `--`→`_`), size hint `x-archive-size-hint:<bytes>` for large items, `x-archive-queue-derive:0` to skip the heavyweight derive process (massively faster ingest for data uploads), `x-archive-interactive-priority:1` for express queue ("Do not perform multiple transactions on items with mixed settings" — a consistency landmine), `x-archive-keep-old-version:0/1` clobber semantics, `x-archive-cascade-delete:1`.
- Staging+ingest "can take some time" — uploads complete before items are fully derived.
Source: https://archive.org/developers/ias3.html — **confidence: HIGH**.

**F3.9 — Official bot etiquette (2026 policy page — must-follow for wauploader).** archive.org/developers/bots.html: **all automated requests must carry a descriptive User-Agent (tool name + version; for AI agents the model)**; `ia` CLI ≥5.7.2 has `--user-agent-suffix`; honor `429` + `Retry-After`; add delays for bulk ops; use `--checksum` to avoid re-uploads; exponential backoff explicitly recommended; `test_collection` is the sandbox (items auto-removed ~30 days); permanent collections require emailing info@archive.org.
Source: https://archive.org/developers/bots.html — **confidence: HIGH**.

---

## 4. Robustness patterns worth adopting in Go

**F4.1 — Retry/backoff libraries (verified).**
- `hashicorp/go-retryablehttp`: exists, **MPL-2.0**, 2351★, thin net/http wrapper, retries on transport errors + 5xx (except 501), rewindable request bodies, latest tag ~Jun 2025 (RateLimitLinearJitterBackoff added) — maintained but slow-moving (open "Maintenance status?" issue #249). Known sharp edges to design around: default backoff ignores `RetryWaitMax` when `Retry-After` present (#247, #295) and backoff sleep doesn't check the context deadline (#272).
  Source: https://github.com/hashicorp/go-retryablehttp/ (+ tags/issues pages). **Confidence: HIGH.**
- `golang.org/x/time/rate`: exists, official x-repo, **BSD-3-Clause**, tagged & stable, token-bucket `Limiter` with `Wait(ctx)`/`Reserve` — the canonical per-host limiter primitive. Page dated 2026-02-11.
  Source: https://pkg.go.dev/golang.org/x/time/rate. **Confidence: HIGH.**
- Recommendation: for SPN2, retries are *job-level* (poll status; retry submission on 5xx/network) — a ~100-line custom ladder with full jitter + `x/time/rate` fits better than retryablehttp; for IA-S3 PUTs (503 SlowDown semantics identical to S3), retryablehttp or the same ladder both work. Either way: honor `Retry-After` explicitly (retryablehttp's default doesn't).

**F4.2 — Rate limiting shape for SPN2.** Compose: (a) global `rate.Limiter` tuned to ≤80% of daily quota (EDGI precedent: IA staff asked for 80%-of-limit defaults, F1.7); (b) semaphore capped at 12 concurrent pending jobs (authenticated cap, F3.2) — e.g. `golang.org/x/sync/semaphore` (same x-family; used widely, incl. wabarc/wayback's errgroup); (c) per-target-host limiter kept well below the server's >20-same-host trigger (F3.6) — a map[string]*rate.Limiter keyed by target host.

**F4.3 — Local journal / resume store under the CGO-free constraint (all verified current).**
- `modernc.org/sqlite` **v1.60.1** (2026-09-29), BSD-3-Clause, CGo-free port, canonical at gitlab.com/cznic/sqlite with an active GitHub mirror (115★); daily-active development; proven in this workspace (anicli-go/hhauto memory: v1.58, single-writer, DSN pragmas).
  Source: https://pkg.go.dev/modernc.org/sqlite. **Confidence: HIGH.**
- `go.etcd.io/bbolt` **v1.5.0**, MIT, etcd-maintained Bolt fork, stable/frozen API, single-file KV, "used in high-load production environments serving databases as large as 1TB".
  Source: https://pkg.go.dev/go.etcd.io/bbolt. **Confidence: HIGH.**
- JSONL journal: `encoding/json` + atomic-rename append; zero deps, trivially inspectable, no crash-recovery semantics beyond fsync+rename.
- Recommendation: **bbolt** for the job journal (url → state machine: queued/submitted/job_id/pending/done/failed + timestamps + result snapshot) — single-writer fits the CLI, no SQL surface to maintain; choose modernc sqlite only if the tool grows queryable reporting (then reuse the anicli-go DSN-pragma conventions from memory). JSONL acceptable for an MVP but loses cheap random lookups on resume.

**F4.4 — Checksum/dedup.** Follow internetarchive's two-layer model: pre-submit dedup via `if_not_archived_within` + a local "already captured at T" index; upload dedup via local MD5 vs remote (`checksum=True` semantics) and `x-archive-keep-old-version:0` clobber semantics so re-upload is safe. Store content hashes (MD5 for IA-compat; SHA-256 for local identity) in the journal.

**F4.5 — Idempotency strategies.**
- SPN2: submission is idempotent-ish only via `if_not_archived_within`; treat job_id as the durable handle (persist immediately after POST, before polling); handle HTTP-200 refusals (F3.5) so a refusal is journaled as a terminal per-day state, not retried in a loop; `error:too-many-daily-captures` → park until local midnight+buffer.
- IA-S3: PUT-of-file is naturally idempotent (clobber) — keep it that way with `x-archive-keep-old-version:0`; item creation via `x-archive-auto-make-bucket:1`; never mix `x-archive-interactive-priority` settings within one item (F3.8); treat ingest/derive lag as normal (verify later, not synchronously).

**F4.6 — TOML config (both verified current).**
- `BurntSushi/toml`: MIT, 4974★, reference implementation, TOML v1.1.0-compatible, stable API.
- `pelletier/go-toml/v2`: MIT, **v2.4.3** (pkg.go.dev 2026-07-05), actively developed, ~5–7× faster in its published benchmarks; go-toml v1 is in maintenance mode (common comparison trap).
  Sources: https://github.com/BurntSushi/toml/ ; https://pkg.go.dev/github.com/pelletier/go-toml/v2 ; independent 2026-10 analysis https://fudengji.cn/article/5902/ (config-scale parse time is negligible either way). **Confidence: HIGH.**
- Recommendation: `pelletier/go-toml/v2` (active dev, better errors); workspace precedent exists for both (anicli-go: BurntSushi; hhauto: go-toml/v2 per memory). Secrets-in-TOML matches the owner's standing convention (memory: constraints-are-fixed).

**F4.7 — Progress UI for batch jobs (verified).**
- `vbauerster/mpb`: 2514★, **The Unlicense** (public domain), multi-bar with dynamic add/remove, EWMA ETA, byte counters, width-synced decorators — the natural fit for N concurrent captures/uploads.
  Source: https://github.com/vbauerster/mpb. **Confidence: HIGH.**
- `schollz/progressbar/v3`: MIT, 4678★, deliberately single-line (no multi-line outputs), io.Writer integration — simpler, good for a single long upload.
  Source: https://github.com/schollz/progressbar/. **Confidence: HIGH.**
- `charm.land/bubbletea/v2` for a full TUI is available and v2-current (memory-verified 2026-09-12, v2.0.9) — but per owner UX precedent (xrayctl pivot to plain CLI), a non-TUI colored console + mpb is the lighter default.
- Recommendation: mpb for the batch runner; keep a plain non-TTY output mode (pipe/systemd-safe), mirroring the hhauto console-surface pattern in memory.

**F4.8 — Redirect/auth pattern from internetarchive worth copying.** IA-S3's frequent 307s + the `--location-trusted` curl caveat mean a Go client must decide explicitly whether `Authorization` survives a redirect. Go's net/http strips Authorization on any cross-host redirect; internetarchive's Python lib keeps it for archive.org hosts only (`rebuild_auth` override). Go equivalent: `CheckRedirect` that re-attaches the LOW header iff the redirect target host ends in `.archive.org`.

---

## 5. GAP ANALYSIS — build vs adapt vs reuse

| Area | Findings | Recommendation |
|---|---|---|
| SPN2 client | Official `gospn` exists (AGPL-3, quiet since 2025-05); MIT `RaghavSood/spn2` minimal; the wire API is small and fully documented (F3.x) | **Build own thin client** (~400–600 lines: POST /save with LOW auth, GET /save/status/{id}, typed `status_ext` errors, HTTP-200-refusal handling per #305). MIT-safe; use gospn's `CaptureOptions` names only as a familiarity anchor. Encodes ALL doc quirks (#304/#305) from day one — no existing Go client does. |
| IA-S3 uploader | **No Go library exists** (F2.6); official docs describe a plain header-driven REST surface | **Build own**: net/http PUT + `x-archive-*` headers, multipart for large files, `ExpectContinueTimeout` enabled (100-Continue), `CheckRedirect` auth-preservation for `.archive.org`, `queue_derive=0` default for data files, size-hint, MD5 checksum dedup (internetarchive semantics). |
| HTTP transport | archive.org requires a descriptive UA and 429/Retry-After etiquette; **no evidence of TLS fingerprinting on archive.org** (the Bots policy page and ArchiveTeam's Obstacles wiki describe fingerprinting as a *target-site* technique, not an IA-side one) | **Plain `net/http`** with a mandatory descriptive User-Agent (config field). `bogdanfinn/tls-client` (BSD-4, v1.16.0, memory-verified) is **not needed** — it would add fhttp complexity for zero benefit here. |
| Retry/backoff | retryablehttp (MPL-2, maintained-ish, Retry-After caveats) vs custom ladder | Custom small ladder with full jitter + explicit `Retry-After` honoring for both APIs; adopt retryablehttp only if IA-S3 PUT retry semantics grow complex. |
| Rate limiting | x/time/rate verified; server caps: 12 concurrent / 100k·day / 10·day·URL / same-host >20 | `x/time/rate` global limiter at ≤80% of quota (EDGI pattern) + semaphore(12) + per-target-host limiter map. |
| Journal/resume | modernc sqlite v1.60.1 vs bbolt v1.5.0 vs JSONL (all CGO-free, verified) | **bbolt** for the job-state journal; sqlite if reporting/querying is wanted later. Persist job_id before first status poll. |
| Config | go-toml/v2 v2.4.3 vs BurntSushi (both MIT, verified) | **go-toml/v2**; keys/credentials inline in TOML per owner convention; descriptive UA as a required config value with a loud validation error if empty. |
| Progress | mpb (Unlicense) vs progressbar/v3 (MIT) vs bubbletea v2 | **mpb** for batch bars + plain non-TTY fallback (owner's xrayctl/hhauto console precedents). |
| Checksum dedup | internetarchive: MD5 skip-if-match + verify | MD5 for IA interop + SHA-256 local identity column in the journal. |
| What NOT to build | WARC writing (gowarc/Zeno territory), browser emulation (SPN2 does the capturing server-side), archive.today/Ghostarchive/Telegraph slots (wabarc territory) | Out of scope for wauploader v1 per the API-only brief. |

---

## 6. Sources index (all fetched this session via Exa MCP)

- SPN2 official API doc mirror: https://gist.github.com/regstuff/82e690db2f1d91ba59f6681c1abad6cf (mirror of docs.google.com/document/d/1Nsv52MvSjbLb2PCpHlat0gkzw0EvtSgpKHu4mk0MnrA, by Vangelis Banos, upd. 2022-04-05)
- https://github.com/internetarchive/wayback/issues/305 (per-URL cap discrepancies; HTTP-200 refusals)
- https://github.com/internetarchive/wayback/issues (issue list incl. #304)
- https://github.com/jjjake/internetarchive/issues/771 (SPN2 feature request, open)
- https://archive.org/developers/internetarchive/cli.html, /python-lib.html, /configuration.html, /_modules/internetarchive/session.html
- https://raw.githubusercontent.com/jjjake/internetarchive/master/README.rst and /internetarchive/session.py
- https://archive.org/developers/ias3.html (IA-S3)
- https://archive.org/developers/bots.html (UA/429/backoff policy)
- https://pkg.go.dev/github.com/internetarchive/gospn ; https://github.com/internetarchive/gospn
- https://github.com/RaghavSood/spn2
- https://github.com/wabarc/wayback ; https://github.com/wabarc/archive.org ; https://github.com/wabarc/wayback/blob/main/wayback.go
- https://github.com/internetarchive/zeno/
- https://github.com/MathiasDPX/goarchiver
- https://wayback.readthedocs.io/en/stable/usage.html ; https://wayback.readthedocs.io/en/latest/_modules/wayback/_client.html (EDGI wayback)
- https://github.com/internetarchive/openlibrary/blob/master/openlibrary/core/ia.py
- https://github.com/hashicorp/go-retryablehttp/ (+tags, +issues)
- https://pkg.go.dev/golang.org/x/time/rate
- https://pkg.go.dev/modernc.org/sqlite ; https://pkg.go.dev/go.etcd.io/bbolt
- https://github.com/BurntSushi/toml/ ; https://pkg.go.dev/github.com/pelletier/go-toml/v2 ; https://fudengji.cn/article/5902/
- https://github.com/vbauerster/mpb ; https://github.com/schollz/progressbar/
- https://wiki.archiveteam.org/index.php/Obstacles (fingerprinting = target-site technique)

## 7. UNVERIFIED / open questions

1. **`internetarchive` exact `mount_http_adapter` retry defaults** (numeric `max_retries`/`backoff_factor`/`status_forcelist` inside the library) — my source fetches captured the method signature but not the body defaults. Only the *surfaces* (upload retries/503-SlowDown, search max_retries, custom adapter) are verified. Impact on wauploader: none (we build our own ladder).
2. **wabarc/wayback's IA mechanism** (SPN2 JSON API vs scraping `/save`): repo/module verified to exist and to route IA captures through `wabarc/archive.org`, but the exact wire mechanism inside that module was not source-verified this session. Only matters if we copy behavior; license blocks code reuse anyway (GPL-3).
3. **"LOW S3 / HIGH cookie" tier lore**: the official docs describe exactly two auth methods (LOW keys, cookies) with anonymous(6)/authenticated(12) concurrency and 4k/100k daily caps. No "HIGH" cookie tier is documented anywhere I fetched; treat that framing as unverified community shorthand.
4. **Doc-internal inconsistencies to resolve at implementation time**: js_behavior_timeout max (7s in Limitations vs 30s in param table); per-URL daily cap (10 vs 5 vs per-resource-type — F3.5). Both should be handled defensively (parse messages; don't hard-code a single magic number).
5. **gospn release cadence**: latest release v1.0.4 (2025-05-25); no commits/release data past that was fetched. "Alive but quiet" is an inference from release history + 0 open issues, not a commit-graph verification.
6. **IA-S3 hard concurrency/speed numbers**: the official docs give no explicit requests-per-second cap for uploads; the only quantitative guidance is qualitative (307s, 100-Continue, derive skipping, SlowDown retries). Practical upload pacing should be calibrated against live 503 SlowDown behavior (not done here — no live requests per contract).
7. **Exa cache truncation**: the SPN2 gist was fetched twice with different maxCharacters to capture head+tail; the middle error-code table was captured in the first pass (R1/G1 results), the Limitations/tips/FAQ in the second (T1). Coverage of the doc is believed complete (all sections listed in its TOC were seen), but a final implementation-time read of the canonical Google Doc is cheap insurance.
