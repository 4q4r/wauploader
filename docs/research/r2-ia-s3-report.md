R2 REPORT — Uploading files to archive.org items via IA-S3 + Metadata/Tasks APIs + auth model
================================================================================
DONE

Skills invoked (5): brainstorming, grilling, build-with-exa, research, sharp-edges
- brainstorming / grilling: loaded per gate mandate; as a research-only subagent with no user interaction and a fully-specified brief, no interview/design loop was applicable — the brief itself is the spec.
- build-with-exa: applied — Exa MCP (web_search_exa + web_fetch_exa) was the primary research path for every external fact.
- research: applied — primary sources only (archive.org/developers docs, internetarchive source code at master, IA help center), per-claim citations.
- sharp-edges: applied as the analysis lens for §4/§8 (API footguns: retry semantics, silent failure modes, error taxonomy, idempotency).

Memory files read (coverage verifiable): MEMORY.md (full) + all 43 files under ~/.config/opencode/memory/ (17 topics/ + 26 projects/). Every file was opened in the sweep (cat); 19 small files read in full, 24 large files head+tail+full-text keyword sweep (archive\.org|wayback|internetarchive|ia-s3|s3\.us|wauploader|spn2|savepagenow). Keyword hits: only 2, both worth passing to the controller:
- topics/environment-infra.md:16 — `archive.org/wayback/available` API "returns false empties"; use CDX (`web.archive.org/cdx/search/cdx?url=...&filter=statuscode:200&reverse=true`) instead. (Relevant to the SPN/wayback slice, not this one.)
- projects/bos-pro-bugs.md:33 — a Wayback recon module in the panic tool (irrelevant).
No memory file contains prior wauploader/IA-S3 knowledge. No memory files were written (facts returned here per contract).

Confidence legend: HIGH = official IA doc or reference-implementation source read directly; MED = secondary source / real-world issue / inference from reference implementation; LOW-flagged = community lore not confirmed this session.

--------------------------------------------------------------------------------
1. IA-S3 ENDPOINT & SEMANTICS
--------------------------------------------------------------------------------

1.1 Endpoint and resource model — HIGH
- PUT https://s3.us.archive.org/<identifier>/<filename> — "Items (things with details pages) get mapped to S3 Buckets"; "Files within items are also available as S3 keys"; "Doing a `PUT` on the S3 endpoint will result in a new internet archive Item"; DNS-bucket style `http://<identifier>.s3.us.archive.org/` also works.
  Source: https://archive.org/developers/ias3.html
- "When a file is added to an Item, it is staged in temporary storage and ingested via the Archive's content management system. This can take some time." (ias3.html) — uploads are async-ingested; the file is not necessarily visible immediately (rclone docs corroborate: "all uploads/deletes will not show up immediately", https://rclone.org/internetarchive/).

1.2 Auth header — HIGH
- `Authorization: LOW <access>:<secret>` — "There is an optional simple way to authorize a request; Authorization header can be of form `Authorization: LOW $accesskey:$secret` Be sure to use https when using the simple mode." (ias3.html)
- Reference impl `S3Auth` (https://raw.githubusercontent.com/jjjake/internetarchive/master/internetarchive/auth.py) sets exactly `LOW {access}:{secret}`.
- AWS SigV4 is NOT supported: real-world 403 with XML body `<Error><Code>InvalidAccessKeyId</Code><Message>The AWS Access Key Id you provided does not exist in our records.</Message><Resource /><RequestId>3cb1b592-...</RequestId></Error>` when a Dart client sent SigV4; switching to LOW made the upload succeed. (https://github.com/idrats/aws_s3_client/issues/9) — MED-HIGH.

1.3 Where keys come from + programmatic bootstrap — the critical answer
- Manual: "Go to https://archive.org/account/s3.php and log in. Select the checkbox and click Generate New Keys. Make a note of the two keys." Source: https://archive.org/developers/tutorial-get-ia-credentials.html (official). The s3.php page itself, unauthenticated, renders: "You must be logged in to get API keys. Click here to login" (fetch of the page). HIGH.
- Programmatic (documented in reference implementation): `POST https://archive.org/services/xauthn/?op=login` with form data `email` + `password`. JSON response: `j['values']['s3']['access']`, `j['values']['s3']['secret']`, `j['values']['cookies']['logged-in-user']`, `j['values']['cookies']['logged-in-sig']`, `j['values']['screenname']`; failure → `{"success": false, ...}` with `values.reason` ∈ {`account_not_found`, `account_bad_password`, ...} or top-level `error`. The reference impl sleeps 2s after the call. Source: https://raw.githubusercontent.com/jjjake/internetarchive/master/internetarchive/config.py (`get_auth_config`). Corroborated by IAKit issue: "Keys can be fetched programmatically: POST `email` + `password` to `https://archive.org/services/xauthn/?op=login`. The JSON response returns `values.s3.access`, `values.s3.secret`, the `logged-in-user`/`logged-in-sig` cookies, and `screenname`. No HTML scraping needed." + "IA has no delegated-auth flow to avoid [the primary password]" (https://github.com/jbuckner/InternetArchiveKit/issues/36, 2026-06). HIGH.
- Cookies→keys bootstrap (no password): NO documented or known endpoint. s3.php key generation is an interactive logged-in web form ("checkbox + Generate New Keys"); no public JSON endpoint that converts `logged-in-user`/`logged-in-sig` cookies into S3 keys was found in official docs, the reference CLI (which only supports email+password via xauthn, or netrc), or third-party tooling. Design consequence for wauploader: bootstrap = user pastes S3 keys (primary), or optional email+password → xauthn (returns keys AND cookies in one call); do NOT scrape s3.php. MED-HIGH (negative finding — absence of evidence after targeted search).
- Env vars `IA_ACCESS_KEY_ID` / `IA_SECRET_ACCESS_KEY` are honored by the reference impl and must be set together (config.py `get_config`). HIGH.
- Key validation without side effects: `GET https://archive.org/services/user.php?op=whoami` with LOW auth returns account JSON; `ia configure --check` treats `success: true` as valid keys (session.py `whoami()` — deliberately uses S3 keys, not cookies). HIGH.

1.4 Single-PUT vs multipart — HIGH (existence) / MED (details)
- Official: "ias3 has support for multipart uploads." (ias3.html). Also: "HTTP 1.1 Range headers are ignored (also copy range headers for multipart)" and "For multipart, the `x-archive-keep-old-version` header must be specified at the time the multipart upload is completed" (ias3.html).
- But no official doc describes the multipart flow (no initiate/part/complete endpoints documented anywhere on the developer portal). The reference CLI/lib does NOT use multipart — every file is a single PUT, even multi-GB.
- Community evidence (jjjake/internetarchive#386, comment by "maxz", 2021): IA multipart differs from AWS S3 — no copy-range support; parts as small as 1KB accepted (5MB AWS minimum not enforced); "some parts of the multipart upload related API are rather unstable or unreliable. The endpoint responsible for listing already uploaded parts ... just keeps on processing for hours" at 1000 parts; ~100 parts per listing query "somewhat reliable". (https://github.com/jjjake/internetarchive/issues/386) — MED.
- Practical verdict for wauploader: single PUT per file is the reference path and the safe path; multipart exists but is undocumented and fragile.

1.5 Other transport semantics — HIGH
- "Only the HTTP 1.1 REST interface is supported." "DELETE bucket is not allowed." "ACLs are fake. permissions are: World readable, Item uploader writable." "Archive is much more likely to issue 307 Location redirects than Amazon is." Clients need good 100-Continue support; curl ≥7.58 needs `--location-trusted` when an Authorization header is present (ias3.html).
- `transfer-encoding: chunked` is NOT supported by IA-S3 — "It should NEVER be set" (item.py strips it from the prepared request). Source: https://raw.githubusercontent.com/jjjake/internetarchive/master/internetarchive/item.py. HIGH.
- Fast downloads should go through `https://archive.org/download/<id>/<file>` (redirecting web infra), not s3 GET (ias3.html).
- Item task/activity page: `https://archive.org/history/<bucket>` (ias3.html).

--------------------------------------------------------------------------------
2. x-archive-* / metadata HEADERS (all verbatim names)
--------------------------------------------------------------------------------

2.1 Item creation & lifecycle
- `x-archive-auto-make-bucket: 1` — "combined upload and make item feature" on a PUT (alias `x-amz-auto-make-bucket:1` appears in all official curl examples). "This header is only valid in IAS3 PUT commands"; default 0; "If you do not specify this value you must create an item before you attempt to upload to it." Sources: ias3.html; https://github.com/vmbrasseur/IAS3API/blob/master/headers.md (reference doc by an IA staff member). HIGH.
- `x-archive-queue-derive: 0` — skips derive; DEFAULT behavior is that "a PUT (aka file upload) to IA will cause the derive process to be queued for that bucket/item" (ias3.html). Reference impl sends the header only on the LAST file of a multi-file upload batch (item.py `upload()` sets `_queue_derive=True` only when `file_index >= total_files`); `ia upload --no-derive` disables for all files. HIGH.
- `x-archive-size-hint: <bytes>` — "For large items a size hint can be given to the IA content management system at make bucket time. Units are in bytes" (ias3.html). Reference impl auto-computes it as the total byte size of the whole upload batch if not supplied (item.py `upload()` → `recursive_file_count_and_size`), and per-file otherwise (`upload_file` falls back to the single file's size). HIGH.
- `x-archive-interactive-priority: 1` — "express queue ... interactive user's upload for example, one can request interactive queue priority. Do not perform multiple transactions on items with mixed settings for `x-archive-interactive-priority`. Doing so can result in reordering of the application of requests to the item state." (ias3.html). HIGH.
- `x-archive-keep-old-version: 1` — "Normally PUT and DELETE do not keep old versions of files around... Saved versions will be placed in `history/files/$key.~N~`" (ias3.html). CLI docs: "ia upload makes a backup of any files that are clobbered. They are saved to a directory in the item named `history/files/`. The files are named in the format `$key.~N~`"; disable with `-H x-archive-keep-old-version:0`. IMPORTANT DEFAULT: `ia upload` sends `x-archive-keep-old-version: 1` unless `--no-backup` is passed (ia_upload.py: `if not args.header.get("x-archive-keep-old-version") and not args.no_backup: args.header["x-archive-keep-old-version"] = "1"`). headers.md (older doc) describes it as renaming the old file "prepending the filename with `.~~`" and marks it "experimental" — discrepancy with the current official `history/files/$key.~N~` behavior; trust ias3.html + cli.html. Sources: https://archive.org/developers/ias3.html, https://archive.org/developers/internetarchive/cli.html, headers.md. HIGH (with noted discrepancy).
- `x-archive-cascade-delete: 1` — on DELETE, "all the derivatives and originals related to a file can be automatically deleted" (ias3.html). HIGH.
- `x-archive-ignore-preexisting-bucket: 1` — EXACT NAME (the brief's "ignore-preexisting-bucket-error" is not a real header). Default 0; "This header only works when PUTting to IAS3." headers.md semantics: "A normal PUT operation including `x-archive-meta-*` headers will overwrite an existing `IDENTIFIER_meta.xml` file. The `x-archive-ignore-preexisting-bucket` header will instead overwrite the existing `IDENTIFIER_meta.xml` file with the `x-archive-meta-*` header values passed in the same PUT command." (headers.md). The reference impl does NOT send this header anywhere. HIGH (name+doc), MED (exact 403-vs-overwrite behavior on existing buckets — see UNVERIFIED #9).
- `x-archive-simulate-error: <name>` — error simulation for testing: `curl s3.us.archive.org -v -H x-archive-simulate-error:SlowDown`; `x-archive-simulate-error:help` lists all simulatable errors (ias3.html). GOLD for wauploader's CI/test suite. HIGH.

2.2 Metadata headers — HIGH
- Form: `x-archive-meta-$meta_name: $meta_value` (alias `x-amz-meta-$meta_name`). Multiple values: numbered prefixes `x-archive-meta01-<name>`, `x-archive-meta02-<name>`, ... ("Meta headers are sorted prior to tag generation"). Values "are interpreted as having utf-8 character encoding". Because rfc822 headers disallow `_`, "in $meta_name two hyphens in a row (`--`) will be translated to an underscore (`_`)". Unicode workaround: encode the entire value as `uri($payload_as_uri_encoded_utf8)` — e.g. `x-archive-meta-title:uri(This%20is%20a%20snowman%20%E2%98%83)`. "to update metadata use the Item Metadata API" (headers are make-bucket-time only in spirit). All from ias3.html. HIGH.
- `x-archive-meta-mediatype` — accepted values (official schema): `texts, etree, audio, movies, software, image, data, web, collection, account`. If uploading via CLI without specifying, items default to `data` mediatype "This cannot be changed afterwards" (cli.html warning) — a classic footgun; wauploader should always set mediatype explicitly (for "everything" uploads, `data` is the sensible explicit choice). Sources: https://archive.org/developers/metadata-schema/ , cli.html. HIGH.
- `x-archive-meta-collection` (repeatable meta00/01/…) — "Required for all items except 'fav-username' collections... Uploaders can only choose from collections that they have privileges for. General uploaders with no special privs can only upload to selected 'Community' collections or the test_collection. Items in the test_collection are removed from the site after 30 days." (metadata-schema, collection field). Community collections (items.html): `opensource` (Community Texts), `opensource_movies` (Community Video), `opensource_audio` (Community Audio), `opensource_image` (Community Images), `opensource_media` (Community Data), `open_source_software` (Community Software). `test_collection` = sandbox, auto-removed after ~30 days, "use it to validate uploads end-to-end (upload, derive, viewing)". Collections can only be created by IA staff (items.html). CLI default when unspecified = `opensource` (cli.html). Parent collections are auto-appended after the next task (schema). Sources: https://archive.org/developers/items.html , metadata-schema, cli.html. HIGH.
- Metadata keys must be valid XML tag names; the reference impl's validator: `^[A-Za-z][.\-0-9A-Za-z_]+(?:\[[0-9]+\])?$` (utils.py `is_valid_metadata_key`). HIGH.

2.3 Checksum mechanism — exact
- At upload: reference impl sets the standard `Content-MD5` header — but with the HEX digest (`get_md5()` returns `hashlib.md5(...).hexdigest()`), and only when `verify` or `delete` is requested: `_headers['Content-MD5'] = md5_sum` (item.py). Docstring for `verify`: "Verify local MD5 checksum matches the MD5 checksum of the file received by IAS3" — i.e. the server is relied upon to reject a mismatched PUT. Official docs do not document Content-MD5 at all; server-side enforcement is an INFERENCE from the reference implementation (MED). Note the hex-vs-base64 oddity (AWS uses base64; IA evidently accepts hex).
- `--checksum` (skip-if-exists) exact mechanics (item.py `upload_file`): compute local `md5 = get_md5(body)`; `ia_file = self.get_file(key)` (from metadata API cache); skip the PUT iff `(not self.tasks) and (ia_file) and (ia_file.md5 == md5_sum)` — i.e. the file already exists in the item AND its stored md5 equals the local md5 AND the item has no queued/running tasks (race guard against ingest/derive). Returns an empty `Response()` object for skipped files. So: NO server checksum header is involved; it's a client-side compare against `files[].md5` from the Metadata API. HIGH.
- Post-upload verification: `GET https://archive.org/metadata/<id>` → `files[]` entries carry `md5`, `crc32`, `sha1`, `size` (md-read.html example). Compare locally. HIGH.
- IA-S3 ETag semantics (whether ETag == md5): NOT verified — do not rely on it.

2.4 Size limits & concurrency — HIGH (numbers are official but mutually inconsistent)
- Help center (upload troubleshooting): "A single file in an item should not be larger than ~500 to ~700 GB (capacity depends on activity levels...; we recommend using the lower limit of 500GB). We recommend creating items that have less than 10,000 files, and where the files do not exceed 1TB. While 10,000 files is not a hard limit (the API will allow up to 250,000 files), exceeding that number is likely to disrupt your experience." Source: https://help.archive.org/help/uploading-troubleshooting/
- Developer portal (items.html): "As a rule of thumb, items should: not be over 100GB; not contain more than 10,000 files." Source: https://archive.org/developers/items.html — quote BOTH to users; treat ~100GB/10k files as the comfortable envelope, 1TB/~500GB-file as the ceiling.
- Concurrency: no official numeric limits anywhere. Known constraints: 503 SlowDown when the PUT/DELETE task queue is overloaded (per-key, dynamic — §4); one task at a time per item (tasks.html — derives/metadata tasks serialize per item); "Do not perform multiple transactions on items with mixed settings for x-archive-interactive-priority" (ias3.html); rclone's official docs: the per-item write queue feeds an "Item Deriver Queue [which] has a limit, and it may block you from uploading, or even deleting. You should avoid uploading a lot of small files for better behavior." (https://rclone.org/internetarchive/). IA staff (jjjake, issue #312): rate limiting "is dynamic and changes often"; "We do not ban anyone ... based on them exceeding our rate-limits"; "if you're getting a 503 the solution is to simply retry until you don't". https://github.com/jjjake/internetarchive/issues/312 — HIGH.
- Reference client etiquette (session.py): default User-Agent embeds the access key: `internetarchive/<ver> (<os> <release>; N; <lang>; <access_key>) Python/<ver>`; default header `Connection: close`. MED (convention, not documented requirement).

--------------------------------------------------------------------------------
3. IDENTIFIER RULES
--------------------------------------------------------------------------------

3.1 Official rules — HIGH (https://archive.org/developers/metadata-schema/)
- Charset: "a unique combination of alphanumeric characters (limited to ASCII), underscores (`_`), dashes (`-`), or periods (`.`)". First character must be alphanumeric (cannot start with `_`, `-`, `.`). Max length 100; "we generally recommend that identifiers be between 5 and 80 characters". Schema field `identifier` "accepted values": "minimum length is 5 characters, maximum length is 100 characters ... first character must be alphanumeric ... mediatype:account items begin with @ symbol."
- "Identifiers must be unique across the entirety of Internet Archive, not simply unique within a single collection." "Once defined an identifier can not be changed."

3.2 Reference-implementation validator — HIGH (utils.py `validate_s3_identifier`)
- Legal chars set: `A-Za-z0-9._-`; reject if string starts with `.`, `_`, or `-`; reject if `len < 3` or `len > 100`; a leading `@` is allowed and stripped before the charset check (user/account items). Note internal inconsistency in the lib: the length check is 3..100 but the error message says "between 3 and 80". The CLI's `ia upload <identifier>` validates with this function (ia_upload.py `type=validate_identifier`).
- Practical guidance for wauploader: generate identifiers in `[a-z0-9]` + `-`, length ≥5 ≤80, not starting with `-`/`.`/`_`; treat the 3-char minimum as theoretical.

3.3 Bucket mapping & existing items — HIGH/MED
- identifier == bucket name; filename == key. PUT to an existing (self-owned) bucket adds a new file or overwrites a same-named key ("Normal PUT operation will overwrite a file when it is used to upload a file of the same name" — headers.md); other files in the item are untouched. Sending `x-archive-meta-*` headers on such a PUT rewrites `IDENTIFIER_meta.xml` (headers.md; exact conflict behavior — see UNVERIFIED #9). Buckets cannot be deleted (ias3.html); items can be removed via the `delete.php` task (tasks.html, irreversible) or made dark.
- Files whose names collide with managed metadata (`<identifier>_meta.xml`, `<identifier>_files.xml`) are IA-managed; no explicit doc captured this session — avoid generating those names (MED, standard practice).

--------------------------------------------------------------------------------
4. ERROR TAXONOMY & RETRY (reference defaults included)
--------------------------------------------------------------------------------

4.1 Response body format — HIGH
- IA-S3 error bodies are AWS-style XML: `<Error><Code>...</Code><Message>...</Message><Resource>...</Resource><RequestId>...</RequestId></Error>`. The reference impl parses `Message` + `Resource` tags for display (`get_s3_xml_text`, utils.py). On some 500s the body is invalid XML — the lib handles that explicitly ("probably HTTP 500 error and response is invalid XML ... either temporary, or requires the intervention of IA admins", item.py). HIGH.

4.2 Confirmed errors — verbatim where captured
- 403 + `InvalidAccessKeyId` (wrong/foreign key or SigV4): full XML captured in §1.2. HIGH.
- 503 SlowDown: "Sometimes the task queue system which processes PUTs and DELETEs becomes overloaded, and the endpoint returns a 503 SlowDown error instead of processing an upload or delete" (ias3.html). Sub-case: SPAM BLOCK — a 503 whose body contains `appears to be spam` is a hard, non-retryable fail in the reference impl (`if b'appears to be spam' in response.content: log.info('detected as spam, upload failed'); break`, item.py); help center calls this "503-slowdown-spam" and asks users to send the Details text to info@archive.org. HIGH.
- Overload pre-check (documented, cheap): `GET https://s3.us.archive.org/?check_limit=1&accesskey=$accesskey&bucket=$bucket` → JSON `{bucket, accesskey, over_limit, detail}`; `over_limit: 0` = queue ready, `1` = "uploads or deletes are likely to get a 503 SlowDown error"; `detail` "contains internal information about the current rate limiting scheme, it may change at any time" (ias3.html). The lib's `s3_is_overloaded()` treats any non-200/JSON-parse failure as overloaded=True (fail-safe). HIGH.
- 429 Too Many Requests + optional `Retry-After`: documented for Tasks API and Metadata Write rate limits (see §5/§6). rclone also retries 429 on IA-S3 (rclone source comment: `429, // Too Many Requests` in retryErrorCodes). HIGH for tasks/mdapi; MED that IA-S3 PUTs can emit 429.
- 409 Conflict: rename.php task to an existing `new_identifier` (tasks.html).
- 403 `bad request` (malformed LOW auth), 403 "bucket already owned by you": NOT captured from any primary source this session — see UNVERIFIED #1. LOW-confidence lore.
- 404: task log fetch for a task that doesn't exist / hasn't run (tasks.html). Also GET of nonexistent file/bucket (S3-style; HIGH by S3 convention, LOW for IA-specific body).
- 503 with generic "503-slowdown-spam" vs plain SlowDown: distinguish in code by sniffing the body for `appears to be spam`. HIGH.

4.3 Retry defaults of the reference implementation — HIGH
- CLI: `--retries/-R` ("Number of times to retry request if S3 returns a 503 SlowDown error") DEFAULT 0 (no retries); `--sleep/-s` (seconds between retries) DEFAULT 30s; `--status-check` = run `s3_is_overloaded()` and exit 1 with "Expect 503 SlowDown errors" if over limit (ia_upload.py, session.py, item.py: `retries = retries or 0; retries_sleep = retries_sleep or 30`; request timeout default 120s).
- Upload retry loop (item.py `upload_file`): on 503 with retries left: if body has `appears to be spam` → fail hard; else sleep(retries_sleep), decrement, and BEFORE re-PUT poll `s3_is_overloaded(access_key)` (check_limit=1) — while overloaded, keep sleeping/retrying. Note: a fresh attempt re-sends the whole file (no resume).
- Session-level HTTP retries (requests/urllib3) are mounted ONLY for `archive.org`, NEVER for `s3.us.archive.org` — comment: "Don't mount on s3.us.archive.org, only archive.org! IA-S3 requires a more complicated retry workflow." Config: `Retry(total=3, connect=3, read=3, redirect=False, allowed_methods=["POST","HEAD","GET","OPTIONS"], status_forcelist=[429,500,501,502,503,504], backoff_factor=1, respect_retry_after_header=True)`. Default `max_retries=3` (session.py `mount_http_adapter`). HIGH — i.e.: metadata/tasks GETs/POSTs get transport-level retries with exponential backoff (factor 1 → sleeps 0,2,4...s scaled by Retry-After), S3 PUTs get only the explicit 503 loop.
- rclone IA backend for comparison: retries on `{429, 500, 503}` with a pacer; supports `wait_archive` to poll until the server-side queue drains before comparing files (https://rclone.org/internetarchive/). MED.

4.4 Interrupted PUT semantics
- There is NO resume: a killed/cut PUT must be restarted from byte 0 (single-PUT model; multipart exists but is undocumented/fragile, §1.4). Feature request for resume has been open since 2021 with no implementation: "ability to resume the uploading of a single file ... each upload can require 12 hours or more" (jjjake/internetarchive#386, OPEN as of 2026-06 update). HIGH.
- Re-PUT of the same key+content is the retry strategy and is effectively idempotent (same key overwritten, old version preserved if keep-old-version:1). Partial-transfer artifacts inside the item are not documented either way — no primary source says whether a partial object can appear; the staged-then-ingested model (§1.1) suggests it does not, but treat "is a re-PUT after mid-transfer failure always sufficient?" as needing one sandbox validation (UNVERIFIED #5). MED.

--------------------------------------------------------------------------------
5. METADATA API (read + write)
--------------------------------------------------------------------------------

5.1 Read — HIGH (https://archive.org/developers/md-read.html)
- `GET https://archive.org/metadata/<identifier>` → JSON with top-level `metadata`, `files[]` (each: `name, source, format, mtime, size, md5, crc32, sha1`, ...), `item_size`, `files_count`, `server`, `d1/d2`, `dir`, `workable_servers`. Public for public items; auth (S3 keys OR cookies) needed for restricted items and some User JSON fields.
- Partial reads by path: `/metadata/<id>/metadata`, `/metadata/<id>/files/12` (files by INDEX for reads).

5.2 Write — HIGH (https://archive.org/developers/md-write.html)
- `POST https://archive.org/metadata/<identifier>`, `Content-Type: application/x-www-form-urlencoded`. Single-target fields: `-target` ∈ {`metadata`, `files/<filename>`, `<identifier>`/`<name>` (user JSON)}, `-patch` = a JSON Patch (JSON-encoded, then percent-encoded — "double-encoded"). Patch `path` is a JSON Pointer that EXCLUDES the target (e.g. `/creator`, not `/metadata/creator`). Optional `priority` (int, −10..+10, default 0). Multi-target (atomic): `-changes` = percent-encoded JSON array of `{target, patch}` objects; same target may repeat.
- Auth for writes: `Authorization: LOW` header, OR S3 keys as form fields `access` + `secret` appended to the payload (reference impl `S3PostAuth` appends `&access=...&secret=...` and forces form content-type; md-write.html: "Metadata Write also permits the user's S3 access/secret keys to be included as URL-encoded form data in the POST payload"), OR cookies (metadata.html: MDAPI accepts `logged-in-user`/`logged-in-sig` via `Cookie:` header — "in general, cookies should only be used by browsers and S3 keys for programmatic access"). HIGH.
- Patch standard: "The Metadata Write API does not yet comply with the latest Json-Patch standard. It currently complies with version 02" (item.py docstring). MED-HIGH.
- Success: `200 {"success":true,"task_id":<int>,"log":"https://catalogd.archive.org/log/<task_id>"}`. Error: `400 {"success":false,"error":"No changes made to _meta.xml"}` — patches that change nothing are rejected ("no changes made"); "Other errors have different messages" (md-write.html; no exhaustive error-code list exists). HIGH.
- Rate limits: clients "should be prepared to receive a 429 Too Many Requests ... The server may return a Retry-After header". `X-Accept-Reduced-Priority: 1` request header may convert a would-be 429 into a queued task returned with `200` + `X-Priority-Reduced: <n>` (e.g. `-7`); a 429 can still happen even with the header. HIGH.
- Lookahead: "changes submitted to an item will be reflected in MD Read's results before the patch is written to disk. Lookahead takes effect the moment a change is submitted." Tasks are serialized per item, executed in submission order regardless of priority. Always read state via MD Read (never the raw data node) before diffing. HIGH.
- Deletion semantics: JSON Patch `{"op":"remove","path":"/<field>"}` (md-write.html example). Note: the older internetarchive idiom of sending `{"<field>": "REMOVE_TAG"}` as a plain dict exists in older lib versions; the current lib builds real JSON patches client-side (MetadataRequest with append/append_list/insert/expect sugar). MED.
- User JSON targets create `{identifier}.json` / `{identifier}_{name}.json` files in the item — do NOT upload or delete those via S3 ("This interferes with MDAPI"). HIGH.

--------------------------------------------------------------------------------
6. TASKS API (derive/task polling)
--------------------------------------------------------------------------------
All HIGH unless noted. Source: https://archive.org/developers/tasks.html (official) + catalog.py.

6.1 Endpoint & auth
- `https://archive.org/services/tasks.php` — GET (listing + logs), POST (submit), PUT (rerun; the overview prose says "only two HTTP methods: GET and POST" but the Rerun section documents PUT — doc inconsistency). "The API requires IA-S3 keys for user authentication. It currently does not accept Internet Archive cookies." (verbatim — this is the hard cookies-vs-keys boundary).
- Response envelope: `{"success": bool, "error": ..., "value": ...}`; JSON Lines (`Content-Type: application/json-l`) only for limit=0 streaming.

6.2 Listing (polling)
- Categories: `summary` (counts by run status: queued/running/error/paused; default, always included unless `summary=0`), `catalog` (active tasks: queued/running/error/paused), `history` (completed). Combine: `?identifier=<id>&catalog=1&history=1`.
- Criteria (AND-combined; one value each; wildcard `*` or `%` allowed except in history): `identifier`, `task_id`, `server`, `cmd` (e.g. `archive.php`, `modify_xml.php`, `derive.php`), `args`, `submitter`, `priority` (10..-10), `wait_admin` (int) / `status` / `color`, and `submittime>` `<` `>=` `<=` (free-text datetimes). History category REQUIRES `identifier` or `task_id`, and identifier may NOT be wildcarded there.
- Paging: default 50, max 500 (clamped); response may include `cursor` (opaque, e.g. `c:123456`, URL-encode the colon) — repeat the identical request + `cursor` until no cursor. `limit=0` → single round-trip JSON-L stream, each line a task object with an extra `category` field (`summary`/`catalog`/`history`).
- Run states: `wait_admin` 0=Queued(green), 1=Running(blue), 2=Error(red), 9=Paused(brown). These are THE terminal signals for polling: a derive is done when it leaves `catalog` and appears in `history`; failed = wait_admin 2.
- Poll rate-limit visibility: `GET ?rate_limits=1&cmd=derive.php` → `{"success":true,"value":{"cmd":"derive.php","task_limits":<n>,"tasks_inflight":<n>,"tasks_blocked_by_offline":<n>}}` ("IA may adjust rate limiting thresholds and policies at any time"). Example shows task_limits:500 for modify_xml.php — do NOT hardcode; MED (single example).
- Task log: `GET ?task_log=<task_id>` → `text/plain`, with `Last-Modified` for caching; 404 if not-yet-run/nonexistent; partial logs while running; owner-only; now served from `archive.org` (catalogd.archive.org legacy routing noted in docs; the lib still targets catalogd for logs — catalog.py — MED on which host to prefer).

6.3 Submission
- `POST` JSON `{"identifier": "...", "cmd": "derive.php", "args": {...}, "priority": 0}`. Supported cmds: `book_op.php`, `bup.php`, `delete.php` (IRREVERSIBLE item deletion), `derive.php` (optional arg `remove_derived=<filespec>` e.g. `*.jpg`; originals never deleted), `fixer.php`, `make_dark.php` / `make_undark.php` (arg `comment`), `rename.php` (arg `new_identifier`; 409 Conflict if taken). Success: `{"success":true,"value":{"task_id":<int>,"log":"https://archive.org/log/<id>"}}`.
- Only one task executes per item at a time; tasks run in submission order regardless of priority (tasks.html + md-write.html).
- Rerun a failed (wait_admin 2) task: `PUT` JSON `{"op":"rerun","task_id":<int>}` → `{"success":true,"value":{"<task_id>":"<identifier>"}}`.
- Reduced priority opt-in: `X-Accept-Reduced-Priority: 1` (same 429-avoidance contract as MD Write; response header `X-Priority-Reduced`).
- Human-viewable catalog page per item: `https://archive.org/history/<bucket>` (ias3.html).
- Wauploader recommendation (my synthesis, flagged): after PUTs, don't submit derive manually unless `x-archive-queue-derive:0` was used — PUT auto-queues derive; poll `?identifier=<id>&catalog=1` until the derive leaves catalog, then optionally verify in history / via MD Read files[].

--------------------------------------------------------------------------------
7. COOKIES vs S3 KEYS — full surface map
--------------------------------------------------------------------------------

| Surface | S3 keys (LOW) | Cookies (logged-in-user + logged-in-sig) | Source/conf |
|---|---|---|---|
| IA-S3 PUT/GET/DELETE (s3.us.archive.org) | REQUIRED (`Authorization: LOW`) | no role | ias3.html; HIGH |
| Metadata Read (archive.org/metadata/<id>) | accepted (needed for restricted) | accepted | metadata.html: "the caller may supply either the user's S3 access/secret keys or their Internet Archive HTTP cookies"; HIGH |
| Metadata Write (POST /metadata/<id>) | accepted (header OR access/secret form fields) | accepted | md-write.html + metadata.html; HIGH |
| Tasks API (services/tasks.php) | REQUIRED | REJECTED — "It currently does not accept Internet Archive cookies" | tasks.html; HIGH |
| whoami (services/user.php?op=whoami) | REQUIRED (lib deliberately forces S3 auth here) | not used | session.py; HIGH |
| account/s3.php key page | n/a | required (interactive) | tutorial-get-ia-credentials; HIGH |
| Downloading access-restricted content; "viewing your task history" | no | yes (cookies) | tutorial-get-ia-credentials ("archive.org logged-in cookies, for downloading access-restricted content and viewing your task history"); HIGH — note the "task history" phrasing refers to the website surface, consistent with tasks.html's API-level no-cookies rule; MED on the nuance |
| xauthn login (services/xauthn/?op=login) | n/a (produces BOTH keys and cookies from email+password) | n/a | config.py + IAKit #36; HIGH |

- Cookie format details (config.py/session.py): two cookies, `logged-in-user` (value starts with the email + `;`-joined data; lib takes `split(';')[0]` and `unquote`s it for `user_email`) and `logged-in-sig`; default domain `.archive.org`, path `/`.
- Sharp-edge verdict (sharp-edges lens): for wauploader, use S3 keys EVERYWHERE except (a) if SPN2/wayback slice needs logged-in cookies — note SPN2 API also accepts S3-style `access`/`secret` params (that's the other slice's territory), and (b) cookie-based download of restricted content. Keys are full-account credentials with no scoping — treat as secrets, never log them (the reference lib deliberately embeds the ACCESS key only in the UA, never the secret).

--------------------------------------------------------------------------------
8. PRACTICAL LARGE-FILE GUIDANCE (synthesis, each fact sourced above)
--------------------------------------------------------------------------------
- No resume exists anywhere in the API or reference CLI (open issue #386 since 2021; only the web browser uploader has a Resume button). Wauploader must: stage the file locally, compute md5 before upload, do whole-file PUTs, and on failure restart the file from 0 (idempotent because same-key PUT overwrites; enable `x-archive-keep-old-version:1` to protect prior versions — it's the CLI default).
- Pre-flight before each PUT (or each item): `GET s3.us.archive.org/?check_limit=1&accesskey=...&bucket=<id>`; while `over_limit==1`, sleep+poll (this is exactly what the reference impl does between 503 retries, with 30s default sleep).
- Treat 503-with-`appears to be spam` as terminal (rate-limit-evasion signal, do not hammer).
- Timeouts: reference impl uses a 120s requests timeout per PUT (that is a socket inactivity timeout, not a total-transfer cap) — MED (interpretation); set generous no-progress timeouts for multi-GB PUTs.
- Set `x-archive-size-hint` (whole-item byte total) on the first PUT; it exists precisely "for large items" to help CMS capacity planning (ias3.html).
- Sequential uploads within one item; parallelism only across items; avoid thousands of tiny files (Item Deriver Queue limits can block uploads/deletes; rclone guidance). Use `x-archive-queue-derive:0` on every file except the last if you want exactly one derive at the end (reference-impl pattern), or `:0` everywhere + manual `derive.php` submission if you want full control of when derive runs.
- Verify: send `Content-MD5` (hex form works — reference impl) for in-transfer integrity, then compare against `files[].md5` from the Metadata API post-ingest; `files[]` also carries `crc32` and `sha1` if stronger verification is wanted.
- `x-archive-simulate-error` (e.g. `:SlowDown`; `:help` lists the rest) gives deterministic failure injection for wauploader's own retry-logic tests — no real traffic abuse needed.
- Mediatype: set explicitly (default `data` is immutable); collection: set explicitly (`opensource` family or `test_collection` for E2E tests — test_collection items are purged after ~30 days).
- Expect ingest latency: file "staged in temporary storage and ingested ... can take some time"; not immediately visible via archive.org/download (rclone's `wait_archive` exists for this).

--------------------------------------------------------------------------------
UNVERIFIED / OPEN QUESTIONS
--------------------------------------------------------------------------------
1. Verbatim failure bodies not captured from primary sources this session: 403 body `bad request` (malformed LOW auth), `bucket already owned by you` (PUT to existing bucket — the brief expected this; headers.md instead documents meta.xml overwrite semantics and `x-archive-ignore-preexisting-bucket`), and the plain 503 SlowDown body text ("slow down buckaroo!" is NOT confirmed anywhere for IA-S3 — do not code against it). Controller can settle all three in one sandbox session with `x-archive-simulate-error:help` + one PUT to a self-owned existing bucket — I did not run any live calls per the no-mutation constraint.
2. Cookie→S3-key headless conversion: no documented or folklore-known endpoint; s3.php generation appears interactive-only. Negative result at MED-HIGH confidence after targeted searching (docs + IAKit #36 + reference source). If wauploader wants cookie-only bootstrap, it needs its own research or an IA contact; email+password→xauthn is the clean documented path.
3. Server-side enforcement of `Content-MD5` (hex form) on PUT: inferred from the reference impl's `--verify`/`--delete` design and docstring; no official doc states it. One sandbox test (upload with deliberately wrong Content-MD5) settles it.
4. Multipart upload flow on IA-S3: officially "supported" (ias3.html) but flow/endpoints undocumented; community report says listing-parts is unstable (issue #386). Do not build on it for v1.
5. Whether an interrupted (mid-transfer) PUT can leave a partial object visible in the item: undocumented; staged-then-ingested model suggests no, but verify once in `test_collection`.
6. ETag semantics of IA-S3 responses (md5? anything?): unverified; use `files[].md5` from the Metadata API instead.
7. Numeric rate limits: none published; "dynamic and changes often" (IA staff, issue #312). The only programmatic visibility is `check_limit=1` (S3) and `rate_limits=1&cmd=` (tasks).
8. `x-archive-keep-old-version` layout discrepancy: current official docs say `history/files/$key.~N~`; the older third-party headers.md says `.~~` prefix + "experimental". Trust the official doc; verify once if old-version recovery ever matters.
9. PUT-to-existing-bucket with metadata headers: does it 403 by default (requiring `x-archive-ignore-preexisting-bucket:1`) or silently overwrite `_meta.xml` as headers.md reads? Contradiction between the brief's expectation and the captured docs; sandbox test needed (same test as #1).
10. tasks.php method surface: overview says GET/POST only, Rerun section documents PUT — minor doc inconsistency; PUT rerun is the documented example, treat overview prose as stale.
11. Minimum identifier length: schema field says 5, items doc implies nothing, reference validator enforces 3. Generate ≥5-char identifiers to satisfy the strictest reading.
