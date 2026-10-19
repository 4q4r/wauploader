DONE_WITH_CONCERNS

# R1 — Save Page Now 2 (SPN2) Capture API: verified factual map

Date of research: 2026-10-19. Method: Exa MCP (`web_search_exa` + `web_fetch_exa`) against primary sources only. NO capture requests were made to any `/save` endpoint (contract constraint honored; the official Google Doc was fetched read-only via its `export?format=txt` URL).

**Concern that justifies DONE_WITH_CONCERNS:** global-memory coverage was FULL for 37 of 44 files; 6 large project files (secureway_bot.md remainder, traffic_mimicry.md, anicli-py.md, ert.md, hhauto.md, wms.md — total ~785 KB) were read as header + targeted keyword scan (`archive\.org|wayback|spn2|save page|web\.archive|Authorization: LOW|s3.?key` → **zero hits in all six**) because full reads were infeasible within the session budget. No task-relevant fact was plausibly missed, but coverage is not literal-full for those six. Details in the memory section below.

## Skills invoked (5)

1. `brainstorming` (mandated; loaded — interactive gate N/A for a fixed-scope subagent research task per SUBAGENT-STOP)
2. `grilling` (mandated; loaded — same N/A rationale)
3. `build-with-exa` (mandated; GOVERNED the whole research path: Exa primary, highlights-first, retry on timeout — the two `docs.google.com` livecrawl timeouts were solved by retrying with the `export?format=txt` URL)
4. `research` (own choice — primary-source discipline: every claim traced to the source that owns it)
5. `verification-before-completion` (own choice — no success claim without a captured source; every verbatim string below was seen in a fetched payload)

## Memory files read (coverage list)

Full reads (37):
- `~/.config/opencode/MEMORY.md` (index, in full)
- `topics/` (17/17): `workflow.md`, `workflow-git-pr.md`, `stack-and-tools.md`, `2026-10-09-telegram-serverless.md`, `2026-10-11-novum-organum-report-plan.md`, `2026-10-12-slides-novum-organum-plan.md`, `2026-10-14-opencode-v2-storage.md`, `2026-10-15-binary-re-landmines.md`, `2026-10-15-vision-ocr-scans.md`, `code-gotchas.md` (all 78 lines), `communication-behavior.md`, `environment-infra.md`, `harness-server.md`, `hh-ranking-ats-research.md`, `job-search.md`, `remnawave.md`, `tech-gotchas.md`
- `projects/` (20/26 full): `anicli-go.md`, `bos-pro-bugs.md`, `cindex.md`, `freeai-decompile.md`, `freepackages.md`, `habrparser.md`, `home-global.md`, `interview-kb.md`, `iris-proposal.md`, `kolosyug.md`, `labboardvent.md`, `labs.md`, `labs-docker-stands.md`, `remnavawe_ansible.md`, `shikimori-japanese-theme.md`, `teleparse.md`, `tspu-research-2026.md`, `vibecoding-course.md`, `WMS.md`, `xray-raw.md`

Partial reads (6, disclosed): `projects/secureway_bot.md` (lines 1–15 of 66) + header-only first lines with grep keyword scan (0 task-relevant hits each): `traffic_mimicry.md`, `anicli-py.md`, `ert.md`, `hhauto.md`, `wms.md`. None of these files contains anything matching `archive.org|wayback|spn2|save page|web.archive|Authorization: LOW|s3 key` (verified by grep before the partial-read decision). Per the standing memory rule, facts from memory were NOT written back — subagents return facts to the controller.

Relevant memory facts applied to this task: Exa-first search discipline with mandatory retry on timeout (`stack-and-tools.md`); "verify best practices via fresh Exa call before asserting" (same file); research standard: current-generation sources only, every claim with source (communication-behavior.md [2026-09-29]); no live `/save` requests (task contract).

---

## 1. Endpoint, auth, and the `Accept: application/json` behavior

**F1.1 — Capture endpoint.** `POST https://web.archive.org/save`, body `application/x-www-form-urlencoded`, params `url=<URL>` + optional options. With header `Accept: application/json` the response is a JSON body; without it (browser flow) an HTML page is returned. Source: official SPN2 doc (Vangelis Banos, "updated: 2026-07-22"), https://docs.google.com/document/d/1Nsv52MvSjbLb2PCpHlat0gkzw0EvtSgpKHu4mk0MnrA (fetched via `export?format=txt`; mirrored 2022-04-05 revision at https://gist.github.com/regstuff/82e690db2f1d91ba59f6681c1abad6cf). Confidence: HIGH (primary doc).

**F1.2 — Success response body.** `{"url":"http://brewster.kahle.org/", "job_id":"ac58789b-f3ca-48d0-9ea6-1d1225e98695"}` (doc verbatim). Job IDs appear in TWO shapes in the wild: UUID form (doc examples) and `spn2-<40 hex>` form (official Go client example: `"job_id":"spn2-0123456789abcdef0123456789abcdef12345678"`, https://github.com/internetarchive/gospn). Clients must treat job_id as an opaque string. Confidence: HIGH.

**F1.3 — Error envelope on the capture request itself.** The official Go client (gospn) documents that POST /save can return an error JSON WITHOUT a job id: `{"message":"Cannot resolve host nxdomain.fake.tld.","status":"error","status_ext":"error:invalid-host-resolution"}` and a dedup message shape `{"url":"...","job_id":null,"message":"The same snapshot had been made 3 minutes ago. You can make new capture of this URL after 2 hours."}`. Source: https://github.com/internetarchive/gospn (README). Confidence: HIGH (first-party repo, authored by vbanos = the doc author).

**F1.4 — Auth tier: S3 keys (LOW).** Header `Authorization: LOW <accesskey>:<secret>`. Keys generated at https://archive.org/account/s3.php (free account; see official IA developer doc https://archive.org/developers/tutorial-get-ia-credentials.html). Doc calls S3 keys "highly preferable". The literal scheme word is `LOW` (uppercase, single space, `access:secret`); it is IA's own S3-like auth naming, NOT a placeholder (secondary explanation: https://www.bigiron.cc/guides/wayback-machine-apis-cdx-save-page-now-and-availability). Confidence: HIGH.

**F1.5 — Auth tier: cookies.** Two cookies from a logged-in archive.org browser session: **`logged-in-sig`** and **`logged-in-user`** (value format `user%40archive.org` i.e. URL-encoded email). Doc verbatim warning: "Cookies are not desirable because they tend to expire after a few days". Source: official doc. Confidence: HIGH.

**F1.6 — Auth tier: anonymous.** Anonymous use of POST /save exists (counts against IP). Consequences documented: daily cap 200/day (current doc), daily captured-bytes cap 2GiB/day (2025-11-13 changelog; current doc table says "Anonymous Patrons are limited to 500MB total captured data per day" — see F7.6 discrepancy note), per-IP spamhaus SBL/XBL blocklist (error:blocked-client-ip, "Tor exit nodes are excluded from this filter"). Source: official doc + changelog https://docs.google.com/document/d/19RJsRncGUw2qHqGGg9lqYZYf7KKXMDL1Mro5o1Qw6QI. Confidence: HIGH.

**F1.7 — "HIGH limit"/"LOW limit" terminology.** The CURRENT official doc does NOT use the words HIGH/LOW for tiers; it distinguishes authenticated vs anonymous and recommends S3 keys over cookies. The task's "HIGH limit = cookies / LOW limit = S3" framing was NOT found in any primary source; treat client design around literal "HIGH/LOW" labels as unsupported. What IS documented per tier: per-minute caps (7/min authed, 3/min anon — current doc), daily caps, concurrent caps (historical), and login-only options (capture_outlinks, capture_screenshot, email_result require login — changelog 2020-02-03 "Allow capture outlinks & screenshot for registered users only"; confirmed 2026 by overcast07 README: "some options, in particular capture_outlinks=1 and capture_screenshot=1, require authentication", https://github.com/overcast07/wayback-machine-spn-scripts). Confidence: HIGH for what's documented; the HIGH/LOW framing itself: NOT FOUND.

## 2. All request parameters (current doc, verbatim names)

Doc note verbatim: **"Anything other than \"1\" or \"on\" is considered to be \"off\". If you use \"01\" or \"True\" it means \"off\"."** (source: official doc, 2026-07-22 revision). This is a critical client landmine.

| Parameter | Type / values | Semantics (doc-verbatim condensed) | Login required? |
|---|---|---|---|
| `url` | string, REQUIRED | target URL. NEW 2026-09-08 changelog: "Limit max target URL length to 2k bytes and max embed URL length to 8k bytes"; "Limit accessible target URL ports like 80, 443 etc for security reasons" | no |
| `capture_all` | `1`/`on` | "Archive page even when the server returns an HTTP error status (4xx or 5xx). By default, only pages with HTTP status 200 OK are captured." | no |
| `capture_outlinks` | `1`/`on` | "Automatically archive links found on the target page. Also applies to links discovered in PDF, JSON, epub, RSS and MRSS documents." Max 100 outlinks captured per job; login required | **yes** |
| `capture_screenshot` | `1`/`on` | "Generate and archive a full-page PNG screenshot... stored as a separate capture." Screenshot skipped if >4MB; since 2026-03-22 max dimensions 2000px width × 8000px height; login required | **yes** |
| `delay_wb_availability` | `1`/`on` | "capture becomes available in the Wayback Machine after ~12 hours instead of immediately... API responses remain the same." (added 2021-05-04) | no |
| `force_get` | `1`/`on` | "Always use an HTTP GET request for the capture. By default SPN2 does a HTTP HEAD request first to determine when a headless browser or a simple HTTP GET request is required." | no |
| `skip_first_archive` | `1`/`on` | "Skip checking if a capture is a first. Enable this option if you don't need this information to improve performance." | no |
| `if_not_archived_within` | duration expr ("3d 5h 20m") or seconds ("120"); or `v1,v2` where v1=main capture, v2=outlinks | "Capture the web page only if the most recent capture is older than the specified limit... If a newer capture already exists, SPN2 returns that as a recent capture instead of creating a new one. **The default interval is 1 hour.**" (2022 revision said 30 min — changed) | no |
| `outlinks_availability` | `1`/`on` | "Include the timestamp of the last capture for all outlinks." (Field report, March 2021: "does not appear to work as described" — overcast07 README) | no |
| `email_result` | `1`/`on` | "Send an email report of the captured URLs to the Patron's email." (added 2020-04-14, logged-in only) | **yes** |
| `js_behavior_timeout` | integer seconds, default 5, **max 30** (doc FAQ/current text); changelog 2021 said "max value that applies is 30 sec"; `0` disables JS behaviors and speeds up capture. Behaviors = brozzler behaviors.yaml (https://github.com/internetarchive/brozzler/blob/master/brozzler/behaviors.yaml) | no |
| `capture_cookie` | string | "Use extra HTTP Cookie value when capturing the target page" (cookie for the TARGET site, not for archive.org) | no |
| `use_user_agent` | string | "Use a custom HTTP User-Agent value when capturing the target page." (in current doc; NOT in 2022 revision) | no |
| `target_username` / `target_password` | string | "Use your own username and password in the target page's login forms" (form-login autofill, NOT HTTP Basic auth) | no |

Sources: official doc (2026-07-22) for all rows; changelog for history. Confidence: HIGH.

**F2.2 — `x_http_password` does NOT exist in primary sources.** Searched repeatedly (Exa, 4 distinct queries) across the official doc (both revisions), GitHub client code (gospn, waybackpy, spn rust, spn-client, savepagenow, inwebo PHP, ramazansancar JS), PyPI/npm/llms indexes: **zero occurrences**. The closest real capabilities are `target_username`/`target_password` (HTML form login) and `capture_cookie`. Verdict: treat `x_http_password` as a myth/confusion; do NOT implement. Confidence: HIGH (verified absence in all indexed primary+secondary sources; can't prove a negative against unindexed private forks).

## 3. Job lifecycle

**F3.1 — Where job_id comes from.** JSON body of POST /save (`job_id` field). The 2022 doc also shows GET-style flows where an HTML page embeds `spn.watchJob("<job_id>",...)` which clients regex-extract (https://webapps.stackexchange.com/questions/151299 — includes working extraction code; stackoverflow.com/questions/63315159 confirms via HAR). A `Location:` header mechanism was NOT found in any source — the documented async contract is "submit → job_id in body → poll". Confidence: HIGH.

**F3.2 — Status endpoint (exact forms).**
- Per-job: `GET https://web.archive.org/save/status/<job_id>` (or POST with `job_id=<id>` form body) + `Accept: application/json`. Doc verbatim.
- Batch: POST /save/status with `job_ids=<id1>,<id2>,...` (comma-separated).
- All outlink jobs of a parent: POST /save/status with `job_id_outlinks=<parent_id>`.
- User quota: `GET https://web.archive.org/save/status/user` → `{"available":12,"processing":3}`; cache-buster `?_t=<random>` recommended by doc ("To avoid getting a stale cache response").
- System: `GET https://web.archive.org/save/status/system` → `{"status":"ok"}` or `{"status":"Save Page Now servers are temporarily overloaded. Your captures may be delayed."}`; "If there is a critical problem, there will be an HTTP status=502 response."
- **`?api=1` was NOT found** on the status endpoint in the official doc, the changelog, or any client. The real cache-buster in the wild is `?_t=<epoch>` (doc for /user; webapps.SE & stackoverflow for per-job status). Treat `?api=1` as unverified/likely nonexistent; the documented API discriminator is the `Accept: application/json` header. Confidence: HIGH for the forms above; `?api=1`: NOT FOUND.

**F3.3 — Status payload states (terminal + non-terminal).**
- `"status":"pending"` — not terminal. Extra field in the wild: `download_size` (bytes so far, overcast07 uses it to detect stalls: https://raw.githubusercontent.com/overcast07/wayback-machine-spn-scripts/main/spn.sh).
- `"status":"success"` — terminal. Fields (doc verbatim example): `job_id`, `original_url` (**final URL after redirects**), `timestamp` (YYYYMMDDHHMMSS), `duration_sec` (float), `resources[]` (captured embeds list), `outlinks` — object mapping outlink→child job_id when `capture_outlinks=1`, else list/array of URLs (overcast07 parses `outlinks` as a JSON string-array); with `outlinks_availability=1`: outlink→`{"timestamp": "<ts>"|null}`; `screenshot` (URL `http://web.archive.org/screenshot/<original_url>`) only when capture_screenshot=1 and screenshot succeeded. Wild extras: `first_archive` (boolean; overcast07 greps `"first_archive":true`), `http_status` and `counters` (`{"outlinks":70,"embeds":21}` — seen in the rust client's test fixture, docs.rs/spn). Sources: official doc; docs.rs/spn/latest/src/spn/lib.rs.html test fixtures; overcast07 spn.sh. Confidence: HIGH.
- `"status":"error"` — terminal. Fields: `status_ext` (machine code, e.g. `error:invalid-host-resolution`), `message` (human), `exception` (optional raw exception string), `resources[]` (partial), `job_id`. Doc verbatim example captured.
- Snapshot URL derivation: `https://web.archive.org/web/<timestamp>/<original_url>` (doc verbatim: "You can access the created capture using the following URL pattern").
- **Status retention:** current doc NEW: "the status API result is available for a limited time due to system memory limitations. Please try to check job status **within 1 hour** after performing a capture request." → clients must persist job results promptly. Confidence: HIGH.

**F3.4 — Durations and polling guidance.**
- Network connect timeout to target: 10s. Browser page-capture window: 50s ("Partial success may still be recorded if sufficient content has been captured"). Total capture budget: 2m. JS behavior runtime: default 5s, cap 30s. Redirects followed: 3. Outlink extraction window: 30s; "The full capture exceeded the 90-second limit" is cited in FAQ Q2 as the outlink-processing ceiling. (all doc verbatim; note internal inconsistency 2m vs 90s — 90s applies to outlink extraction budget)
- Error `soft-time-limit-exceeded` says 45s in the doc text while the FAQ/limitations say 50s/2m — doc-internal drift; treat 45s as the historical per-page soft limit.
- In-the-wild polling: overcast07 polls every `parallel` seconds (~2s in linear mode, ~1/60min aggregate in parallel mode), gives up on a job after 1200s (20 min) of pending; a practical client budget of ~2–5 min covers HTML captures, but slow/file captures legitimately run longer. waybackpy's anonymous legacy path sleeps 5–10s between attempts. Confidence: HIGH (both are field-hardened clients).

## 4. Error taxonomy

**F4.1 — In-body `status_ext` codes (complete, current doc).** `error:bad-gateway` (target 502) · `error:bad-request` (doc text anomalously says "HTTP status=401" — sic, in the doc) · `error:bandwidth-limit-exceeded` (target 509) · `error:blocked` ("The target site is blocking us (HTTP status=999)") · `error:blocked-client-ip` (anonymous client IP on Spamhaus SBL/XBL; Tor exits excluded) · `error:blocked-url` (Mozilla tracker blocklists) · `error:browsing-timeout` · `error:capture-location-error` · `error:cannot-fetch` (system overload) · `error:celery` (task start failure) · `error:filesize-limit` (>2GB resource) · `error:ftp-access-denied` · `error:gateway-timeout` (target 504) · `error:http-version-not-supported` (505) · `error:internal-server-error` · `error:invalid-url-syntax` · `error:invalid-server-response` · `error:invalid-host-resolution` · `error:job-failed` · `error:method-not-allowed` (405) · `error:not-implemented` (501; listed twice in the doc) · `error:no-browsers-available` · `error:network-authentication-required` (511) · `error:no-access` (403) · `error:not-found` (404) · `error:proxy-error` · `error:protocol-error` ("IncompleteRead") · `error:read-timeout` · `error:soft-time-limit-exceeded` ("duration exceeded 45s") · `error:service-unavailable` (503) · `error:too-many-daily-captures` ("This URL has been captured 10 times today..." — message text still says 10 while current limit is 5/day) · `error:too-many-redirects` · `error:too-many-requests` (target returned 429; "captures to the same host will be delayed for 10-20s") · `error:user-session-limit` · `error:unauthorized` (target requires auth) · **NEW 2021-05-23 wave:** `error:max-daily-bandwidth` (authed 5GB/day), `error:max-daily-bandwidth-from-ip` (anon 2GB/day), `error:max-daily-bandwidth-host` (100GB/day per target host). Source: official doc. Confidence: HIGH.

**F4.2 — Verdict: the task's candidate HTTP codes on POST /save.** Evidence found per code:
- **400** — plausible/request-shape errors; no source documents 400 specifically on /save. NOT CONFIRMED (no source).
- **401** — auth failure; inwebo PHP client maps bad/expired S3 keys to `AuthenticationException` (https://github.com/inwebo/save-page-now-2); doc's `error:unauthorized` is about the TARGET, but a 401 on /save for bad keys is the standard shape implied by client design. LIKELY, not doc-stated.
- **402** — NEVER SEEN in any source. Not documented anywhere. Verdict: does not exist in SPN2.
- **403** — no source documents 403 on /save itself (403 appears only as target-site status in payloads). NOT CONFIRMED on the endpoint.
- **404** — same: only as target status (`error:not-found`). NOT CONFIRMED on the endpoint.
- **405** — same (method-not-allowed is about the TARGET's response; a 405 from /save for GET vs POST was not documented). NOT CONFIRMED.
- **429** — REAL on BOTH endpoints: (a) changelog 2020-01-25: "Improve /status endpoint to return HTTP status=429 when reaching user session limit"; (b) overcast07 treats literal `"429 Too Many Requests"` on BOTH POST /save and GET /save/status as the retry signal (sleep 20s); (c) current doc: per-minute caps "Exceeding these limits results in an error" (code not named, but 429 is the observed one). Confidence: HIGH.
- **451** — NEVER SEEN on /save. (451 can appear as a TARGET-site status inside a job's error message: "…(HTTP status=451)." pattern — overcast07's permanent-failure regex covers any 4xx/5xx target status in messages.) Verdict on endpoint: NOT FOUND.
- **500** — real in the wild as an HTML error page; overcast07 matches the literal body "The server encountered an internal error and was unable to complete your request" and retries. Confidence: HIGH (observed in hardened client), though not doc-documented.
- **502** — documented explicitly for /save/status/system ("critical problem → HTTP status=502"); for /save generally a 5xx storm is the known failure mode (waybackpy: "Wayback Machine's save API is known to be very unreliable", retries on 500/502/503/504 — save_api.py). Confidence: HIGH.
- **523** — NEVER SEEN in any SPN2 source. Cloudflare-specific code; no evidence SPN2 emits it. Verdict: does not exist in SPN2's contract.
- **529** — NOT an SPN2 response code; it is a TARGET-SITE status SPN2 reacts to: changelog 2021-05-23 verbatim: "If a target site returns HTTP status=529 (bandwidth exceeded), we pause crawling that for an hour. If a target site returns HTTP status=429 (too many requests), we pause crawling that for a minute. All requests for the same host in that period get a relevant error message." (same facts mirrored at https://greasyfork.org/en/discussions/development/214822). Confidence: HIGH.
- **509** — bonus wild find: waybackpy maps HTTP 509 on the anonymous GET /save path to "You have probably reached the limit of active sessions" (save_api.py, referencing issue #99). Endpoint-specific (legacy GET path), worth handling.

**F4.3 — Human-readable / in-body error messages (verbatim, wild-observed).**
- "Sorry. This URL is in the Save Page Now service block list and cannot be captured." — blocklist hit (ArchiveTeam wiki, https://wiki.archiveteam.org/index.php/Internet_Archive/Save_Page_Now; also seen in email reports via ArchivingToolsForWBM/AdvancedInternetArchiving). Known blocklist contents include tpc.googlesyndication.com, trib.al, */gtm.js, play.google.com/log, static.doubleclick.net, patreon.com/api/tracking, */generate_204, */wp-admin/*.
- "Sorry. We're currently facing some limitations when it comes to archiving this site. We apologize for any inconvenience this might cause and appreciate your understanding." — hard-limited sites: archive.org, web.archive.org, ia600207.us.archive.org, twitter.com, x.com, cloudflare.com/cdn-cgi/trace (ArchiveTeam wiki).
- "Please try again in ~10 min. Crawling this host is paused because they notified us that they are overloaded right now. (HTTP status=429)" — host paused after target 429 (wayback-machine-webextension issue #1110, https://github.com/internetarchive/wayback-machine-webextension/issues/1110).
- "Crawling this host is paused" — retryable (overcast07 match list).
- "You have already reached the limit" — session/limit error, retryable later (overcast07).
- "You have reached your daily not-logged-in captures limit of…" and "You cannot make more than <N> captures per day" — daily quota exhaustion, pause until next window (overcast07).
- "Cannot start capture" — retryable (overcast07; correlates with error:celery).
- "Your capture will begin in XXs." / "capture will start in <N> hour(s)/minute(s)/second(s)" — host-level artificial delay; FAQ Q3 explains the >20-concurrent-per-host delay algorithm (delay = n/5 sec; "concurrent captures" = captures in the last 60 sec; PLUS 10–20s delay after any target-429 for 60s). Client should parse and sleep the stated delay (overcast07 does exactly this).
- "Live page is not available" (+ wild variant "Live page is not available: chrome-error://chromewebdata/") — SPN2's pre-flight HEAD/GET failed; successful checks are cached 10 minutes; false negatives happen (IP blocking of IA, throttling under load, transient outages) (doc FAQ Q1; overcast07; webmasters.stackexchange.com/questions/145994 documents a real 2024 incident where SPN2 aborted healthy HTTP targets ~260ms into the response).
- "The same snapshot had been made N minutes ago. You can make new capture of this URL after N hours." — dedup refusal (gospn README, job_id:null).
- "Save Page Now could not capture this URL because it was unreachable." — anonymous HTML flow wording (webmasters.SE).
- Target-status message pattern: "<description> for <url> (HTTP status=<code>)." e.g. "Service Unavailable for https://… (HTTP status=503)", "Save Page Now does not have access rights for https://… (HTTP status=403)", "The target server cannot find the requested resource … (HTTP status=404)" (ArchivingToolsForWBM + doc error table).

**F4.4 — Quirks a robust client must know.**
- Service unreliability is acknowledged by its own maintainers' client ecosystem: "Since April 2021, the error rate of Save Page Now has decreased significantly, but there may still be systematic issues" (overcast07); "known to be very unreliable" (waybackpy save_api.py docstring, retries 500/502/503/504 with backoff ×5).
- Slow responses: both hardened clients use generous HTTP timeouts (60s curl max-time per request in overcast07; waybackpy backoff). Design for multi-second POST latency and occasional 5xx storms; never retry POST blindly on network timeout — the capture may have been created (waybackpy calls this out: an ambiguous timeout may still have archived the page; recheck CDX/status instead).
- User-Agent requirements: no UA requirement is documented for the API itself. The WAYBACK extension/website blocks non-browser UAs on the HTML endpoints, but the JSON API works with library defaults (openlibrary uses plain httpx defaults in production — https://github.com/internetarchive/openlibrary/blob/master/openlibrary/core/ia.py `save_page_now`). Also relevant: the project owner's own standing rule (memory: cindex HTTP identity rule — don't spoof UAs; use library defaults).
- Per-host politeness: >20 concurrent captures to the same host → delays (n/5 sec); >50 (2020-11-31 changelog) historical; target-429 → host paused 1 min; target-529 → host paused 1 hour (2021-05-23); current doc FAQ keeps the >20/60s-window rule and the 10–20s 429-delay for 60s. Anonymous client IPs on Spamhaus SBL/XBL are hard-blocked (error:blocked-client-ip).
- URL canonicalization quirks: fbclid stripped (2020-04-03); http/https redirect pairs were mishandled once (fixed 2020-12-28); the same logical page under different schemes counts separately.
- Timestamp-served-cached: anonymous/legacy flow may return a RECENT existing capture instead of a new one (waybackpy `cached_save` flag: timestamp older than request start ⇒ cached; codecopy/savepagenow raises `CachedPage` when "the same URL twice within a few seconds").

## 5. Non-HTML resources via SPN2 (PDF, images, arbitrary files)

**F5.1 — Supported and first-class.** Evidence:
- Doc: `capture_outlinks` "applies to links discovered in PDF, JSON, epub, RSS and MRSS documents"; FAQ Q2: "SPN2 can extract outlinks from many file types: HTML pages, PDF, RSS, XML, epub and JSON files. For each format, it uses a dedicated extraction pipeline" (HTML outlinks via brozzler extract-outlinks.js; PDF via PyMuPDF since 2021-04-10; epub via EbookLib since 2021-08-06; MRSS 2021-06-28).
- `force_get=1` doc tip: "Use force_get=1 when the target URL is not an HTML page and can be retrieved with a simple HTTP request" — faster capture.
- Changelog 2020-08-08: "Identify and route HTML page captures and misc file captures to different queues. Misc file captures are much faster than HTML page captures because they don't require a full browser to run." → non-HTML URLs take a FAST dedicated queue. 
- Download-progress in pending status: changelog 2019-12-22 "Report download size progress for binary file captures"; overcast07 tracks `download_size` on pending status.
- Limits that bind for files: 2GB max resource (error:filesize-limit); per-file download cap 2GiB authed / 500MiB anonymous (2026-03-22); daily byte quotas per tier/host (F7.6).
**Response/status differences:** none in shape — the same pending/success/error status payloads; `resources[]` lists captured subresources; for a plain file capture, success typically has the file URL in resources and no outlinks. Screenshot option is meaningless for non-HTML (doc ties it to pages). Confidence: HIGH (all primary).

## 6. Old-style `GET https://web.archive.org/save/<URL>`

**F6.1 — Still exists, still works, officially shown in the doc itself:** the doc's second auth example is `curl -X GET -H "Accept: application/json" --cookie "logged-in-sig=...;logged-in-user=..." https://web.archive.org/save/http://brewster.kahle.org/`. Behavior (multiple sources):
- Anonymous GET runs the capture INLINE and answers with a redirect to the capture page; the snapshot URL can be extracted from response headers `Content-Location: (/web/<14-digit-ts>/<url>)`, `Link: <...>; rel="memento"`, or `X-Cache-Key` (waybackpy save_api.py regexes — field-proven), or from the final redirect URL.
- History: around 2020-08 it required login ("You need to be logged in to use Save Page Now"), worked again by ~2022 (IndieWeb page https://indieweb.org/Internet_Archive, verified 2023-05); it is rate-limited harder than the API.
- Anonymous 429 on GET /save observed: waybackpy raises TooManyRequestsError with its field observation "Save Page Now limits saving 15 URLs per minutes" (wild number, not doc-confirmed); 509 = session limit on this path.
- Options cannot be passed (options are POST-only per doc: "Additional capture request options (HTTP POST required)"). POST to `/save/<url>` with form body also works in the browser-flow (webapps.SE user code posts `url=<link>&capture_all=on` to `/save/<link>` and gets HTML + `spn.watchJob`).
- ArchiveBox's official plugin still uses anonymous GET /save/<url> and detects `RobotAccessControlException` in the body (robots.txt denial) as a soft-fail (https://github.com/ArchiveBox/abx-plugins, `archivedotorg` plugin).
**When appropriate:** one-off anonymous saves, no credentials, no options, tolerant of caching/dedup semantics. For wauploader's robust API-only client: use POST /save + S3 keys; keep GET /save/<url> only as a no-credential fallback, knowing it returns HTML, is dedup-prone, and harder-limited. Confidence: HIGH.

## 7. Reliability/idempotency/concurrency facts

**F7.1 — Re-saving the same URL (dedup).** Server dedups by default: doc `if_not_archived_within` — "If a newer capture already exists, SPN2 returns that as a recent capture instead of creating a new one. **The default interval is 1 hour**" (30 min in the 2022 revision; historical lineage: "as a general rule, we archive URLs every 30 min to reduce redundant captures" — changelog 2021-01-07). Outlinks have their OWN dedup window (two-value form `v1,v2`). Explicit dedup refusal observed: `{"url":"...","job_id":null,"message":"The same snapshot had been made 3 minutes ago. You can make new capture of this URL after 2 hours."}` (gospn). Conclusion: **POST /save is NOT idempotent, but is effectively deduplicated per-URL by a ~30–60min window unless you override `if_not_archived_within`**; a client must accept "recent capture returned instead of new capture" as success (gospn returns job_id:null + message). Confidence: HIGH.

**F7.2 — Per-URL daily cap:** current doc: 5 captures/day per URL (message text in error table still says 10 — doc drift; changelog history: 10/day since 2020-08-13, reduced later). `error:too-many-daily-captures`. Confidence: HIGH.

**F7.3 — Per-minute caps (current, 2026-07-22 doc):** authenticated 7/min, anonymous 3/min; "Exceeding these limits results in an error." History: 2023-01-22 changelog switched from concurrent-capture counting to per-minute (12/min authed, 4/min anon at that date — matches ruarxive.org's summary of the 2023-01-22 PDF); before that (2021-05-23): concurrent 5 authed / 3 anon (and the 2022-04-05 doc revision's table said 12 concurrent authed / 6 anon — superseded). All numbers with dates above. Confidence: HIGH (current values) / dated otherwise.

**F7.4 — Daily capture counts (current doc):** anonymous 200/day; authenticated 30,000/day ("If you need to make more captures, please contact info@archive.org"). History: 100k/8k (2021-01-10) → 100k/4k (2022 revision table) → current 30k/200. Confidence: HIGH.

**F7.5 — Concurrency:** user sessions queryable via `/save/status/user` → `{"available":N,"processing":M}` (doc example available:12, processing:3); the status endpoint 429s at the session limit (changelog 2020-01-25); overcast07 pauses submissions when `available:0` and runs up to 60 parallel jobs with 2.5s job spacing. Waybackpy historically cited "7 concurrent save sessions per user" (2023 IndieWeb copy of an older doc). Confidence: HIGH for the mechanism, values vary by era — poll /user rather than hardcoding.

**F7.6 — Daily byte quotas (NEWEST, binding for file uploads).** Changelog: 2025-11-13 "5GiB per day for authenticated patrons and 2GiB per day for anonymous patrons" + "100GB per day for a target host"; 2026-03-22 revised to "4GiB per day for authenticated patrons" and per-download 2GiB authed / 500MiB anon. Current doc limitations table says: "Max captures' size per day is 2GB for anonymous users / Anonymous Patrons are limited to 500MB total captured data per day / Max captures' size per day is 5GB for authenticated users / Authenticated Patrons are limited to 5GB total captured data per day". **Doc-vs-changelog discrepancy (500MB vs 2GiB anon) is unresolved in sources; treat the doc table (2026-07-22, newer) as current-best and enforce via error codes `error:max-daily-bandwidth*`.** Confidence: HIGH that the quotas exist; exact anon number: MEDIUM (conflicting primary statements).

**F7.7 — Retry-safe semantics (synthesis for the client design).**
- Ambiguity on transport failure: a timed-out POST may still have created a job ⇒ resolve by checking CDX for a fresh timestamp or re-POSTing with `if_not_archived_within` small window (dedup makes blind re-POSTs safe-ish: worst case you get the recent capture back or the "same snapshot… wait" refusal; you CANNOT create unbounded duplicates — per-URL daily cap 5 bounds the damage).
- Retryable POST responses: 429 (+sleep 20s), 5xx (esp. 500 body "The server encountered an internal error…", 502), empty body; messages "Cannot start capture", "Crawling this host is paused", "You have already reached the limit" (overcast07 retry classes).
- Retryable job-level status_ext: error:cannot-fetch, error:proxy-error (overcast07 keeps the job open on proxy-error), error:too-many-requests (host pause), error:celery, error:job-failed, error:browsing-timeout, error:read-timeout, target statuses 408/409/429/500/502/503/504/507/508 per the message pattern.
- Terminal-for-URL: error:blocked-url, block-list messages, error:invalid-url-syntax, error:invalid-host-resolution, error:not-found, error:no-access, error:unauthorized, error:filesize-limit, too-many-daily-captures (until tomorrow), and any other "…(HTTP status=4xx/5xx)." message (overcast07's permanent class).
- Third-party categorization to cross-check design: spn-client (PyPI) sorts all ~30 documented codes into permanent/transient/quota_exhausted (https://pypi.org/project/spn-client/).
- Status polling: honor 429 (sleep 20s+), persist results within the 1-hour status-retention window, detect pending stalls via `download_size` plateau, and cap per-job wait (field: 20 min).

---

## UNVERIFIED / open questions

1. **`x_http_password`** — not present in the official doc (any revision), changelog, or any of ~10 client libraries searched. Assessed as non-existent (see F2.2). If the task source for this name exists somewhere, it is outside all indexed primary sources.
2. **`?api=1` on the status endpoint** — not found anywhere; the documented API mode is the `Accept: application/json` header, and the only documented query param is the `?_t=` cache-buster on `/save/status/user`. Per-job status with `?_t=<epoch>` seen in the wild (webapps.SE/stackoverflow).
3. **HTTP 402, 451, 523 on POST /save** — no source documents them; 529 is a target-site status that SPN2 reacts to, not an SPN2 response code (F4.2). If these came from a specific blog/issue, that source should be re-checked; nothing in primary sources supports them.
4. **Exact HTTP status code for auth failure and for per-minute limit on POST /save** — behaviorally 401/429 are the strong candidates (client mapping + observed 429s), but no primary text states the codes for /save itself (only /save/status/user→429 documented, and 502 for /save/status/system critical).
5. **Anonymous daily byte quota: 500MB (current doc) vs 2GiB (2025-11-13 changelog, superseded by 2026-03-22 "4GiB authed" revision that does not restate the anon number)** — unresolved conflict between primary sources; authed 4–5GiB/day and host 100GB/day are consistent.
6. **`first_archive` / `http_status` / `counters` / `download_size` fields** — observed in wild payloads and client fixtures but not in the official doc's examples; treat as present-but-undocumented.
7. **`outlinks_availability=1` reliability** — official param, but a March 2021 field report says it "does not appear to work as described" (overcast07 README); no newer confirmation found.
8. **Current `logged-in-*` cookie TTL** — doc says only "tend to expire after a few days"; no exact TTL source found.
9. I did not live-verify any endpoint behavior (contract forbade /save requests); every behavioral claim above is documentation- or client-code-derived, each with its source URL. Values marked "current" are from the doc revision self-dated 2026-07-22 and the changelog entry 2026-09-08 (the latest I could retrieve).

## Source register (primary unless noted)

1. SPN2 official API doc (updated 2026-07-22, Vangelis Banos, archive.org) — https://docs.google.com/document/d/1Nsv52MvSjbLb2PCpHlat0gkzw0EvtSgpKHu4mk0MnrA (via export?format=txt)
2. SPN2 official Change Log (2005→2026-09-08) — https://docs.google.com/document/d/19RJsRncGUw2qHqGGg9lqYZYf7KKXMDL1Mro5o1Qw6QI (via export?format=txt)
3. 2022-04-05 doc revision mirror (gist) — https://gist.github.com/regstuff/82e690db2f1d91ba59f6681c1abad6cf
4. Official IA Go client gospn (vbanos) — https://github.com/internetarchive/gospn (+ raw README)
5. overcast07/wayback-machine-spn-scripts (field-hardened batch client) — https://github.com/overcast07/wayback-machine-spn-scripts (+ raw spn.sh)
6. waybackpy save_api.py (legacy GET path, 429/509/5xx handling) — https://github.com/akamhy/waybackpy
7. palewire/codecopy savepagenow (CachedPage dedup semantics) — https://github.com/palewire/savepagenow
8. ArchiveBox abx-plugins `archivedotorg` (official ArchiveBox SPN usage) — https://github.com/ArchiveBox/abx-plugins/blob/main/abx_plugins/plugins/archivedotorg/on_Snapshot__08_archivedotorg.finite.bg.py
9. ArchiveTeam wiki, Internet Archive/Save Page Now (blocklists, email API) — https://wiki.archiveteam.org/index.php/Internet_Archive/Save_Page_Now
10. ruarxive.org Wayback API KB (2023-01-22 doc digest: 12/min & 4/min) — https://ruarxive.org/kb/instruments/tools/wayback-api
11. IndieWeb Internet_Archive page (anon /save history; old 7-session note) — https://indieweb.org/Internet_Archive
12. wayback-machine-webextension issue #1110 ("Crawling this host is paused…" 429 UX) — https://github.com/internetarchive/wayback-machine-webextension/issues/1110
13. Rust client `spn` (status payload shapes/fixtures) — https://docs.rs/spn/latest/src/spn/lib.rs.html
14. spn-client (PyPI; rate pacing 7/min & 3/min; permanent/transient/quota taxonomy) — https://pypi.org/project/spn-client/
15. inwebo/save-page-now-2 (PHP exception taxonomy) — https://github.com/inwebo/save-page-now-2
16. webapps.stackexchange.com/questions/151299 (spn.watchJob, ?_t cache buster) [secondary]
17. stackoverflow.com/questions/63315159 (capture_outlinks=on wire shape, status flow) [secondary]
18. webmasters.stackexchange.com/questions/145994 ("unreachable" false negatives, live-check flakiness 2024) [secondary]
19. ArchivingToolsForWBM/AdvancedInternetArchiving (email-report message samples) — https://github.com/ArchivingToolsForWBM/AdvancedInternetArchiving
20. IA credentials tutorial (S3 keys) — https://archive.org/developers/tutorial-get-ia-credentials.html
21. openlibrary core/ia.py `save_page_now` (production client, default UA) — https://github.com/internetarchive/openlibrary/blob/master/openlibrary/core/ia.py
22. IA help center (user-facing SPN description) — https://help.archive.org/help/save-pages-in-the-wayback-machine/
23. bigiron.cc guide (LOW-auth explanation) [secondary] — https://www.bigiron.cc/guides/wayback-machine-apis-cdx-save-page-now-and-availability
