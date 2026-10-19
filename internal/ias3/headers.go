// Package ias3 implements the IA-S3 upload wire layer: identifier
// validation, the full x-archive-* header surface, the check_limit=1
// overload preflight, and the single-PUT upload with the dedicated
// 503 SlowDown loop (docs/research/r2-ia-s3-report.md §1–§4).
//
// Wire contract highlights (r2, all HIGH confidence):
//   - PUT https://s3.us.archive.org/<identifier>/<key> with
//     Authorization: LOW <access>:<secret> (SigV4 does not work);
//   - Transfer-Encoding: chunked must NEVER be set (known Content-Length);
//   - a 503 whose body contains "appears to be spam" is terminal and is
//     NEVER retried; a plain 503 SlowDown is retried by re-PUTting the
//     whole file (there is no resume) after check_limit clears;
//   - non-ASCII metadata values are wrapped as uri(<percent-encoded-utf8>).
package ias3

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// S3Base is the IA-S3 endpoint root. Item identifiers map to S3 buckets,
// files to keys: PUT <S3Base>/<identifier>/<key>.
const S3Base = "https://s3.us.archive.org"

// Meta carries metadata fields with their ordered values. A field with
// several values becomes numbered x-archive-meta00-<name>,
// x-archive-meta01-<name>, ... headers; a single value goes out
// unnumbered.
type Meta map[string][]string

// HeaderOpts is the complete control surface of one PUT. The collection
// and mediatype are required by config (fail-fast upstream in the upload
// runner); this package emits exactly what it is given.
type HeaderOpts struct {
	Collection  string // item collection(s); emitted as x-archive-meta01-collection
	Mediatype   string // item mediatype; immutable after creation — always set explicitly
	Title       string // optional convenience metadata
	Description string // optional convenience metadata

	// Extra holds arbitrary x-archive-meta-* fields. Names must match the
	// reference validator ^[A-Za-z][.\-0-9A-Za-z_]+ minus the numeric
	// bracket suffix: bracketed names are legal metadata keys in the JSON
	// Item Metadata API but cannot ride S3 header names, so Upload rejects
	// them (and any other invalid name) fail-fast in Russian — see
	// validateExtraMeta.
	Extra Meta

	AutoMakeBucket          bool // x-archive-auto-make-bucket: 1 — create the item on this PUT
	QueueDerive             bool // x-archive-queue-derive — always emitted (1/0): 0 skips derive
	KeepOldVersion          bool // x-archive-keep-old-version: 1 — keep clobbered files in history/files/
	InteractivePriority     bool // x-archive-interactive-priority: 1
	IgnorePreexistingBucket bool // x-archive-ignore-preexisting-bucket: 1

	// SizeHint is the whole-item total in bytes, set on the first PUT of a
	// batch; omitted when <= 0.
	SizeHint int64
}

// headers materializes the option set into wire headers with the exact
// IA-S3 names (r2 §2). Boolean switches are emitted only when true,
// except x-archive-queue-derive which is always emitted because its 0
// value carries meaning (it suppresses derive, which is otherwise the
// server default). Header names are canonicalized by net/http on the
// wire; IA matches them case-insensitively as usual.
func (h HeaderOpts) headers() http.Header {
	hdr := http.Header{}
	if h.AutoMakeBucket {
		hdr.Set("x-archive-auto-make-bucket", "1")
	}
	// 0 must be expressible (skip derive), so emit unconditionally.
	hdr.Set("x-archive-queue-derive", boolToHeader(h.QueueDerive))
	if h.SizeHint > 0 {
		hdr.Set("x-archive-size-hint", strconv.FormatInt(h.SizeHint, 10))
	}
	if h.Mediatype != "" {
		hdr.Set("x-archive-meta-mediatype", encodeMetaValue(h.Mediatype))
	}
	if h.Collection != "" {
		hdr.Set("x-archive-meta01-collection", encodeMetaValue(h.Collection))
	}
	if h.Title != "" {
		hdr.Set("x-archive-meta-title", encodeMetaValue(h.Title))
	}
	if h.Description != "" {
		hdr.Set("x-archive-meta-description", encodeMetaValue(h.Description))
	}
	if h.KeepOldVersion {
		hdr.Set("x-archive-keep-old-version", "1")
	}
	if h.InteractivePriority {
		hdr.Set("x-archive-interactive-priority", "1")
	}
	if h.IgnorePreexistingBucket {
		hdr.Set("x-archive-ignore-preexisting-bucket", "1")
	}
	for name, values := range h.Extra {
		if len(values) == 0 {
			continue
		}
		wire := strings.ReplaceAll(name, "--", "_")
		if len(values) == 1 {
			hdr.Set("x-archive-meta-"+wire, encodeMetaValue(values[0]))
			continue
		}
		for i, v := range values {
			hdr.Set(fmt.Sprintf("x-archive-meta%02d-%s", i, wire), encodeMetaValue(v))
		}
	}
	return hdr
}

func boolToHeader(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// metaNameRe is the reference metadata-key validator
// (internetarchive utils.py is_valid_metadata_key), pinned by the plan:
// a letter first, then letters/digits/dot/hyphen/underscore, with an
// optional numeric bracket suffix.
var metaNameRe = regexp.MustCompile(`^[A-Za-z][.\-0-9A-Za-z_]+(?:\[[0-9]+\])?$`)

// validateExtraMeta checks every Extra key and returns one Russian error
// naming every invalid key. Two rejection classes: bracketed names —
// legal metadata keys in the JSON Item Metadata API, but S3 header names
// cannot contain "[" or "]" — and names failing the reference validator
// ^[A-Za-z][.\-0-9A-Za-z_]+(?:\[[0-9]+\])?$. Names spelled with "--" are
// accepted: they are translated to "_" for the wire because RFC 822
// header names cannot carry underscores (IA translates them back).
func validateExtraMeta(m Meta) error {
	if len(m) == 0 {
		return nil
	}
	var bad, bracketed []string
	for name := range m {
		switch {
		case strings.ContainsAny(name, "[]"):
			bracketed = append(bracketed, "«"+name+"»")
		case !metaNameRe.MatchString(name):
			bad = append(bad, "«"+name+"»")
		}
	}
	if len(bracketed) > 0 {
		return fmt.Errorf("недопустимые имена метаданных: %s — имена метаданных со скобками не поддерживаются S3-заголовками (передавайте такие поля через Item Metadata API)",
			strings.Join(bracketed, ", "))
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("недопустимые имена метаданных: %s — имя должно начинаться с буквы и содержать только буквы, цифры, точку, дефис или подчёркивание (пара дефисов «--» допустима и означает подчёркивание)",
		strings.Join(bad, ", "))
}

// encodeMetaValue passes ASCII values through verbatim and wraps values
// with non-ASCII bytes as uri(<percent-encoded-utf8>), the IA-S3
// workaround for non-ASCII metadata (r2 §2.2; example:
// uri(This%20is%20a%20snowman%20%E2%98%83)).
func encodeMetaValue(v string) string {
	if isASCII(v) {
		return v
	}
	return "uri(" + percentEncodeUTF8(v) + ")"
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// percentEncodeUTF8 escapes every byte outside the RFC 3986 unreserved set
// (plus "/") as %XX, matching the reference client's Python
// urllib.parse.quote(value.encode("utf-8")) semantics.
func percentEncodeUTF8(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// ValidateIdentifier enforces the archive.org identifier rules (r2 §3):
// charset [A-Za-z0-9._-], alphanumeric first character, length 5..80, and
// no collision with the IA-managed <id>_meta.xml / <id>_files.xml file
// names. The Russian error lists every violated rule, not just the first.
func ValidateIdentifier(id string) error {
	var rules []string
	if n := len(id); n < 5 || n > 80 {
		rules = append(rules, fmt.Sprintf("длина %d, а требуется от 5 до 80 символов", n))
	}
	if id != "" {
		first, _ := utf8.DecodeRuneInString(id)
		if !isAlnumASCII(first) {
			rules = append(rules, "первый символ должен быть буквой или цифрой")
		}
	}
	// Charset: the allowed set is pure ASCII, so a single byte scan is exact.
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		rules = append(rules, "недопустимые символы: разрешены только латинские буквы, цифры, точка, подчёркивание и дефис (A-Z, a-z, 0-9, ., _, -)")
		break
	}
	if strings.HasSuffix(id, "_meta.xml") {
		rules = append(rules, "идентификатор не может оканчиваться на «_meta.xml»: это служебное имя файла метаданных внутри элемента")
	}
	if strings.HasSuffix(id, "_files.xml") {
		rules = append(rules, "идентификатор не может оканчиваться на «_files.xml»: это служебное имя файла описания файлов внутри элемента")
	}
	if len(rules) == 0 {
		return nil
	}
	return fmt.Errorf("идентификатор «%s» не соответствует правилам archive.org: %s",
		id, strings.Join(rules, "; "))
}

func isAlnumASCII(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}
