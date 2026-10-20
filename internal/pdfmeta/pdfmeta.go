// Package pdfmeta derives archive.org item metadata from PDF files
// themselves (docs/plan.md Task 14): document info via pdfinfo, first-page
// text via pdftotext, and the dir-nn-slug identifier scheme over the
// NN_author_topic_words.pdf filename convention.
//
// External binaries (poppler-utils) are REQUIRED at runtime: pdfinfo for
// document info, pdftotext for first-page text. Both are resolved through
// injectable package variables (PDFInfoBin, PdftotextBin) so tests can
// substitute deterministic fakes. DEVIATION from the Task 14 brief (owner
// dispatch): pdfinfo is invoked in plain key-value mode (-isodates), not
// -json — poppler 26.07 removed the -json option ("pdfinfo: sanitize
// output", release notes; verified live on poppler 26.08.0 where -json
// exits with a usage error). The parsed surface (Title/Author/CreationDate/
// Pages) is exactly the brief's.
package pdfmeta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/4q4r/wauploader/internal/ias3"
)

// PDFInfoBin, PdftotextBin and BinTimeout are the external poppler-utils
// surfaces; package variables so tests can substitute fake scripts and a
// shrunk deadline.
var (
	PDFInfoBin   = "pdfinfo"
	PdftotextBin = "pdftotext"
	BinTimeout   = 60 * time.Second
)

// descriptionMaxChars is the first-page text budget for the item
// description (brief: first ~500 chars, whitespace-collapsed).
const descriptionMaxChars = 500

// Info is the document-info surface wauploader consumes from pdfinfo.
// Missing fields stay empty; Pages is 0 when pdfinfo could not report it.
type Info struct {
	Title        string
	Author       string
	CreationDate string
	Pages        int
}

// Meta is the derived archive.org item metadata for one PDF.
type Meta struct {
	Title       string   // PDF Title (junk titles fall back to the topic phrase), else filename-derived
	Creators    []string // creator values per the CreatorSource selection
	Year        string   // CreationDate year, empty when undetectable
	Description string   // first ~500 chars of first-page text, whitespace-collapsed; empty when degenerate
	Subjects    []string // parent dir name + filename topic words
	Language    string   // "rus"/"eng" heuristic over first-page text, empty when mixed
}

// CreatorSource selects which author surfaces feed the derived creator
// values (the --creator-source flag, default file).
type CreatorSource string

const (
	// CreatorSourceFile uses the filename author segment only (default).
	CreatorSourceFile CreatorSource = "file"
	// CreatorSourcePDF uses the PDF Author field only.
	CreatorSourcePDF CreatorSource = "pdf"
	// CreatorSourceBoth uses the filename author first, then the PDF
	// Author as a distinct value (case-insensitive duplicates collapse).
	CreatorSourceBoth CreatorSource = "both"
)

// ParseCreatorSource parses the --creator-source flag value; the empty
// string means the documented default (file).
func ParseCreatorSource(s string) (CreatorSource, error) {
	switch CreatorSource(s) {
	case "", CreatorSourceFile:
		return CreatorSourceFile, nil
	case CreatorSourcePDF:
		return CreatorSourcePDF, nil
	case CreatorSourceBoth:
		return CreatorSourceBoth, nil
	default:
		return "", fmt.Errorf("недопустимое значение --creator-source: %q — ожидается file, pdf или both", s)
	}
}

// yearRe extracts the first 19xx/20xx year from a pdfinfo date line
// (covers both the ISO form of -isodates and the legacy locale form).
var yearRe = regexp.MustCompile(`(19|20)\d{2}`)

// numberPrefixRe matches the mandatory leading file number of the
// NN_author_topic_words.pdf convention.
var numberPrefixRe = regexp.MustCompile(`^(\d+)`)

// Extract reads the document info of one PDF through `pdfinfo -isodates`.
func Extract(path string) (Info, error) {
	out, err := runBin(PDFInfoBin, path, []string{"-isodates", path})
	if err != nil {
		return Info{}, fmt.Errorf("не удалось прочитать метаданные PDF %s через pdfinfo: %w", path, err)
	}

	return parsePDFInfo(string(out)), nil
}

// parsePDFInfo parses the plain pdfinfo key-value output. Lines are matched
// by exact key prefix so keys containing colons ("Custom Metadata: no")
// and values containing colons stay intact; missing keys stay zero.
func parsePDFInfo(out string) Info {
	var info Info

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "Title:"):
			info.Title = strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
		case strings.HasPrefix(line, "Author:"):
			info.Author = strings.TrimSpace(strings.TrimPrefix(line, "Author:"))
		case strings.HasPrefix(line, "CreationDate:"):
			info.CreationDate = strings.TrimSpace(strings.TrimPrefix(line, "CreationDate:"))
		case strings.HasPrefix(line, "Pages:"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			if err == nil {
				info.Pages = n
			}
		}
	}

	return info
}

// FirstPageText extracts the first page's text through
// `pdftotext -f 1 -l 1 <path> -`, sanitized: control characters stripped,
// whitespace runs collapsed to single spaces, capped at maxChars runes.
func FirstPageText(path string, maxChars int) (string, error) {
	full, err := firstPageText(path)
	if err != nil {
		return "", err
	}

	return truncateRunes(full, maxChars), nil
}

// firstPageText returns the FULL sanitized first-page text. The language
// heuristic consumes all of it; only the description is truncated (a
// 500-char window would misread a Russian paper opening with a long
// English abstract).
func firstPageText(path string) (string, error) {
	out, err := runBin(PdftotextBin, path, []string{"-f", "1", "-l", "1", path, "-"})
	if err != nil {
		return "", fmt.Errorf("не удалось извлечь текст первой страницы %s через pdftotext: %w", path, err)
	}

	return sanitizeText(string(out)), nil
}

// runBin executes name with args against path and returns stdout. A
// non-zero exit or a missing binary is an error carrying the binary name
// and its stderr; a deadline breach (BinTimeout) kills the process and
// fails in Russian — a pathological PDF must never hang the batch. The
// C locale is forced so pdfinfo output stays parser-stable regardless of
// the machine's localization.
func runBin(name, path string, args []string) ([]byte, error) {
	// The exported surface is synchronous by design (the CLI has no
	// cancellation shorter than the whole batch), so the background
	// context is the honest lifetime; the deadline is the real guard.
	ctx, cancel := context.WithTimeout(context.Background(), BinTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)

	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return nil, fmt.Errorf("%s не ответил за %v — файл %s", name, BinTimeout, path)
		}

		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		return nil, fmt.Errorf("%s: %s", name, msg)
	}

	return out, nil
}

// sanitizeText strips control characters and collapses whitespace runs
// into single spaces (pdftotext page output is full of newlines, tabs and
// trailing form feeds).
func sanitizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	space := false

	for _, ch := range s {
		if unicode.IsControl(ch) || unicode.IsSpace(ch) {
			space = true

			continue
		}

		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}

		space = false

		b.WriteRune(ch)
	}

	return b.String()
}

// truncateRunes caps s at limit runes and trims a trailing space left by
// a cut between words.
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}

	r := []rune(s)
	if len(r) <= limit {
		return s
	}

	cut := string(r[:limit])

	return strings.TrimRight(cut, " ")
}

// DeriveMeta derives the full item metadata for one PDF: document info,
// first-page text and the filename convention NN_author_topic_words.pdf
// (NN = mandatory leading number, author = second segment, the rest = topic
// words). dirName is the parent directory name feeding the subjects;
// creatorSrc selects the creator values (--creator-source).
func DeriveMeta(path string, dirName string, creatorSrc CreatorSource) (Meta, error) {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	nn := numberPrefixRe.FindStringSubmatch(stem)
	if nn == nil {
		return Meta{}, fmt.Errorf("имя файла «%s» не начинается с номера — ожидается формат NN_автор_тема.pdf (например 01_ivanov_tema.pdf)", base)
	}

	segments := strings.Split(strings.TrimPrefix(stem, nn[1]), "_")
	if len(segments) < 2 || strings.TrimSpace(segments[1]) == "" {
		return Meta{}, fmt.Errorf("в имени файла «%s» нет сегмента автора после номера — ожидается формат NN_автор_тема.pdf", base)
	}

	author := strings.TrimSpace(segments[1])

	topicWords := make([]string, 0, len(segments)-2)
	for _, seg := range segments[2:] {
		if seg = strings.TrimSpace(seg); seg != "" {
			topicWords = append(topicWords, seg)
		}
	}

	info, err := Extract(path)
	if err != nil {
		return Meta{}, err
	}

	full, err := firstPageText(path)
	if err != nil {
		return Meta{}, err
	}

	m := Meta{
		Title:       deriveTitle(info.Title, author, topicWords),
		Creators:    creatorsFor(author, info.Author, creatorSrc),
		Year:        extractYear(info.CreationDate),
		Description: truncateRunes(full, descriptionMaxChars),
		Subjects:    append([]string{dirName}, strings.Join(topicWords, " ")),
		Language:    DetectLanguage(full),
	}

	if m.Subjects[1] == "" {
		m.Subjects = m.Subjects[:1]
	}

	return m, nil
}

// msWordTitlePrefix is the layout-tool prefix marking a Word-embedded
// title; matched case-insensitively (all-ASCII, so the byte length is
// case-stable).
const msWordTitlePrefix = "microsoft word - "

// hexBlobTitleRe matches hex placeholder titles (InDesign/Quark export
// leaks) like <E2E5F1F2EDE8EA5FE3F3EC31342E696E6464> — 16+ hex digits
// with an optional <> wrapper on each side.
var hexBlobTitleRe = regexp.MustCompile(`^[<>]?[0-9a-fA-F]{16,}[<>]?$`)

// junkTitleExts are layout/source-tool extensions that mark the PDF Title
// as a filename leak rather than a real title (matched case-insensitively
// on the title's end).
var junkTitleExts = []string{".indb", ".indd", ".qxd", ".docx", ".doc", ".pptx", ".wpd"}

// deriveTitle applies the owner's garbage-title rules: a PDF-embedded
// Title that is a hex placeholder, a layout/source filename leak, or
// empty/whitespace is junk and falls back to the humanized topic phrase.
// A "Microsoft Word - " prefix is stripped first (underscores→spaces,
// trimmed); a remainder of at least 4 chars becomes the title AS-IS
// (case preserved), anything shorter falls back.
func deriveTitle(pdfTitle, author string, topicWords []string) string {
	title := strings.TrimSpace(pdfTitle)

	if strings.HasPrefix(strings.ToLower(title), msWordTitlePrefix) {
		rest := strings.TrimSpace(strings.ReplaceAll(title[len(msWordTitlePrefix):], "_", " "))
		if utf8.RuneCountInString(rest) >= 4 {
			return rest
		}

		// A degenerate remainder is junk like any other.
		title = ""
	}

	if isJunkTitle(title) {
		return humanizedTopicPhrase(author, topicWords)
	}

	return title
}

// isJunkTitle reports whether the (trimmed) title is garbage: empty, a
// hex placeholder, or a layout/source filename leak.
func isJunkTitle(title string) bool {
	if title == "" {
		return true
	}

	if hexBlobTitleRe.MatchString(title) {
		return true
	}

	lower := strings.ToLower(title)
	for _, ext := range junkTitleExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}

	return false
}

// humanizedTopicPhrase renders the filename fallback title: the topic
// phrase with underscores (already split) and hyphens turned into single
// spaces and the phrase's first letter uppercased. A file without topic
// words degenerates to the author segment (the author is validated
// non-empty), so the fallback title is never empty.
func humanizedTopicPhrase(author string, topicWords []string) string {
	words := topicWords
	if len(words) == 0 {
		words = []string{author}
	}

	phrase := strings.Join(strings.Fields(strings.ReplaceAll(strings.Join(words, " "), "-", " ")), " ")
	if phrase == "" {
		return ""
	}

	first, size := utf8.DecodeRuneInString(phrase)
	if first == utf8.RuneError && size <= 1 {
		return phrase
	}

	return string(unicode.ToUpper(first)) + phrase[size:]
}

// creatorsFor builds the creator values per the --creator-source
// selection: file = filename author only (default), pdf = PDF Author
// only, both = file author first then the PDF Author as distinct values
// (case-insensitive duplicates collapse; empty parts never emit).
func creatorsFor(fileAuthor, pdfAuthor string, creatorSrc CreatorSource) []string {
	fileAuthor, pdfAuthor = strings.TrimSpace(fileAuthor), strings.TrimSpace(pdfAuthor)

	var out []string

	add := func(v string) {
		if v == "" {
			return
		}

		for _, seen := range out {
			if strings.EqualFold(seen, v) {
				return
			}
		}

		out = append(out, v)
	}

	switch creatorSrc {
	case CreatorSourcePDF:
		add(pdfAuthor)
	case CreatorSourceBoth:
		add(fileAuthor)
		add(pdfAuthor)
	default: // CreatorSourceFile
		add(fileAuthor)
	}

	return out
}

// extractYear pulls the first 19xx/20xx year out of a pdfinfo date.
func extractYear(date string) string {
	if y := yearRe.FindString(date); y != "" {
		return y
	}

	return ""
}

// DetectLanguage applies the owner's cyrillic heuristic over text:
// with cyr and ascii the counts of cyrillic and ASCII letters and total
// the count of ALL letters, "rus" fires when cyr/total ≥ 30%, "eng" when
// ascii/total ≥ 70% (ASCII-dominant); anything in between (mixed scripts)
// is omitted.
func DetectLanguage(text string) string {
	cyr, ascii, total := 0, 0, 0

	for _, ch := range text {
		if !unicode.IsLetter(ch) {
			continue
		}

		total++

		switch {
		case unicode.Is(unicode.Cyrillic, ch):
			cyr++
		case ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
			ascii++
		}
	}

	if total == 0 {
		return ""
	}

	if cyr*100 >= total*30 {
		return "rus"
	}

	if ascii*100 >= total*70 {
		return "eng"
	}

	return ""
}

// Identifier derives the dir-nn-slug identifier: <dir>-<NN>-<slug> where
// NN is the filename's leading number with leading zeros stripped (required
// — its absence is an error) and the slug is the remaining filename
// lowercased with every non-alphanumeric run collapsed to one hyphen.
// The result is truncated at a word boundary to archive.org's 80-char
// limit and validated with ias3.ValidateIdentifier.
func Identifier(dirName, fileName string) (string, error) {
	base := filepath.Base(fileName)
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	nn := numberPrefixRe.FindStringSubmatch(stem)
	if nn == nil {
		return "", fmt.Errorf("имя файла «%s» не начинается с номера — ожидается формат NN_автор_тема.pdf (например 01_ivanov_tema.pdf)", base)
	}

	num, err := strconv.Atoi(nn[1])
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать номер из имени файла «%s»: %w", base, err)
	}

	dir := slugify(dirName)
	if dir == "" {
		return "", fmt.Errorf("имя каталога %q не свёртывается в допустимые символы идентификатора — используйте латиницу", dirName)
	}

	slug := slugify(strings.TrimPrefix(stem, nn[1]))

	id := dir + "-" + strconv.Itoa(num)
	if slug != "" {
		id += "-" + slug
	}

	if over := len(id) - 80; over > 0 {
		id = truncateAtWordBoundary(id, over)
	}

	if err := ias3.ValidateIdentifier(id); err != nil {
		return "", fmt.Errorf("производный идентификатор «%s» недопустим: %w", id, err)
	}

	return id, nil
}

// slugify lowercases s, replaces every non-[a-z0-9] run with a single
// hyphen and trims hyphens at both ends.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	dash := false

	for _, ch := range strings.ToLower(s) {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}

			dash = false

			b.WriteRune(ch)

			continue
		}

		dash = true
	}

	return strings.Trim(b.String(), "-")
}

// truncateAtWordBoundary cuts id back to ≤80 chars at the last hyphen
// before the cut point (word boundary), trimming the boundary hyphen.
func truncateAtWordBoundary(id string, over int) string {
	limit := len(id) - over
	if limit >= len(id) {
		return id
	}

	cut := id[:limit]

	if i := strings.LastIndexByte(cut, '-'); i > 0 {
		cut = cut[:i]
	}

	return strings.TrimRight(cut, "-")
}
