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
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/4q4r/wauploader/internal/ias3"
)

// PDFInfoBin and PdftotextBin are the external poppler-utils binaries;
// package variables so tests can substitute fake scripts.
var (
	PDFInfoBin   = "pdfinfo"
	PdftotextBin = "pdftotext"
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
	Title       string   // PDF Title, else a filename-derived human string
	Creator     string   // filename author segment (+ PDF Author when present and distinct)
	Year        string   // CreationDate year, empty when undetectable
	Description string   // first ~500 chars of first-page text, whitespace-collapsed
	Subjects    []string // parent dir name + filename topic words
	Language    string   // "rus"/"eng" heuristic over first-page text, empty when mixed
}

// yearRe extracts the first 19xx/20xx year from a pdfinfo date line
// (covers both the ISO form of -isodates and the legacy locale form).
var yearRe = regexp.MustCompile(`(19|20)\d{2}`)

// numberPrefixRe matches the mandatory leading file number of the
// NN_author_topic_words.pdf convention.
var numberPrefixRe = regexp.MustCompile(`^(\d+)`)

// Extract reads the document info of one PDF through `pdfinfo -isodates`.
func Extract(path string) (Info, error) {
	out, err := runBin(PDFInfoBin, []string{"-isodates", path})
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
	out, err := runBin(PdftotextBin, []string{"-f", "1", "-l", "1", path, "-"})
	if err != nil {
		return "", fmt.Errorf("не удалось извлечь текст первой страницы %s через pdftotext: %w", path, err)
	}

	return truncateRunes(sanitizeText(string(out)), maxChars), nil
}

// runBin executes name with args and returns stdout; a non-zero exit or a
// missing binary is an error carrying the binary name and its stderr.
func runBin(name string, args []string) ([]byte, error) {
	// The exported surface is synchronous by design (the CLI has no
	// cancellation shorter than the whole batch), so the background
	// context is the honest lifetime here.
	cmd := exec.CommandContext(context.Background(), name, args...)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
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
// words). dirName is the parent directory name feeding the subjects.
func DeriveMeta(path string, dirName string) (Meta, error) {
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

	text, err := FirstPageText(path, descriptionMaxChars)
	if err != nil {
		return Meta{}, err
	}

	title := info.Title
	if title == "" {
		title = humanFromFilename(author, topicWords)
	}

	m := Meta{
		Title:       title,
		Creator:     joinCreators(author, info.Author),
		Year:        extractYear(info.CreationDate),
		Description: text,
		Subjects:    append([]string{dirName}, strings.Join(topicWords, " ")),
		Language:    DetectLanguage(text),
	}

	if m.Subjects[1] == "" {
		m.Subjects = m.Subjects[:1]
	}

	return m, nil
}

// humanFromFilename renders the post-number filename as a human title:
// segments joined with spaces, every word's first letter uppercased.
func humanFromFilename(author string, topicWords []string) string {
	words := topicWords
	if author != "" {
		words = append([]string{author}, topicWords...)
	}

	var b strings.Builder

	for idx, word := range words {
		if idx > 0 {
			b.WriteByte(' ')
		}

		first, size := utf8.DecodeRuneInString(word)
		if first == utf8.RuneError && size <= 1 {
			b.WriteString(word)

			continue
		}

		b.WriteString(string(unicode.ToUpper(first)))
		b.WriteString(word[size:])
	}

	return b.String()
}

// joinCreators merges the filename author segment with the PDF Author,
// skipping duplicates (case-insensitive) and empty parts.
func joinCreators(parts ...string) string {
	var seen []string

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		dup := false

		for _, s := range seen {
			if strings.EqualFold(s, p) {
				dup = true

				break
			}
		}

		if !dup {
			seen = append(seen, p)
		}
	}

	return strings.Join(seen, ", ")
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

	slug := slugify(strings.TrimPrefix(stem, nn[1]))

	id := slugify(dirName) + "-" + strconv.Itoa(num)
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
