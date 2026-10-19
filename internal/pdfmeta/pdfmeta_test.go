// pdfmeta tests: fake poppler binaries (deterministic shell scripts),
// the DeriveMeta derivation over the NN_author_topic_words.pdf filename
// convention, and the dir-nn-slug identifier scheme (docs/plan.md Task 14).
package pdfmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4q4r/wauploader/internal/ias3"
	"github.com/stretchr/testify/require"
)

// writeScript drops an executable shell script and returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755))
	return p
}

// overrideBins repoints the injected binary paths for the test's lifetime.
func overrideBins(t *testing.T, pdfinfo, pdftotext string) {
	t.Helper()
	oldInfo, oldText := PDFInfoBin, PdftotextBin
	PDFInfoBin, PdftotextBin = pdfinfo, pdftotext
	t.Cleanup(func() { PDFInfoBin, PdftotextBin = oldInfo, oldText })
}

// fakePair installs a pdfinfo and a pdftotext fake printing the given
// stdout, and returns the directory holding them.
func fakePair(t *testing.T, pdfinfoOut, pdftotextOut string) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, dir, "pdfinfo", "printf '%s' \""+pdfinfoOut+"\"\n")
	writeScript(t, dir, "pdftotext", "printf '%s' \""+pdftotextOut+"\"\n")
	overrideBins(t, filepath.Join(dir, "pdfinfo"), filepath.Join(dir, "pdftotext"))
}

// touch creates an empty stand-in file (the fakes never read it).
func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("stub"), 0o600))
	return p
}

// --- Extract -----------------------------------------------------------------

func TestExtractParsesInfoFields(t *testing.T) {
	fakePair(t,
		"Title:           Aristophanes Frogs and reading culture\n"+
			"Keywords:        None\n"+
			"Author:          Bernd Seidensticker\n"+
			"Creator:         Arbortext\n"+
			"CreationDate:    2021-09-15T11:27:10+03\n"+
			"Custom Metadata: no\n"+
			"Pages:          12\n"+
			"Encrypted:      no\n",
		"")

	info, err := Extract("/any/file.pdf")
	require.NoError(t, err)
	require.Equal(t, Info{
		Title:        "Aristophanes Frogs and reading culture",
		Author:       "Bernd Seidensticker",
		CreationDate: "2021-09-15T11:27:10+03",
		Pages:        12,
	}, info)
}

func TestExtractRecordsCommandLine(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args.txt")
	writeScript(t, dir, "pdfinfo", "printf '%s\\n' \"$@\" > "+record+"\n")
	writeScript(t, dir, "pdftotext", "printf '%s\\n' \"$@\" >> "+record+"\n")
	overrideBins(t, filepath.Join(dir, "pdfinfo"), filepath.Join(dir, "pdftotext"))

	_, err := Extract("/docs/paper.pdf")
	require.NoError(t, err)
	text, err := FirstPageText("/docs/paper.pdf", 10)
	require.NoError(t, err)
	_ = text

	b, err := os.ReadFile(record)
	require.NoError(t, err)
	require.Equal(t, "-isodates\n/docs/paper.pdf\n-f\n1\n-l\n1\n/docs/paper.pdf\n-\n", string(b),
		"both poppler arg contracts are pinned")
}

// A pathological PDF must not hang the batch: the poppler binaries run
// under BinTimeout and a deadline breach fails the file in Russian.
func TestPopplerBinaryDeadlineFailsInRussian(t *testing.T) {
	dir := t.TempDir()
	// `exec` makes sleep the DIRECT child so CommandContext's kill lands
	// on the sleeper itself, not on a shell wrapper holding the pipe.
	writeScript(t, dir, "pdfinfo", "exec sleep 30\n")
	writeScript(t, dir, "pdftotext", "")
	overrideBins(t, filepath.Join(dir, "pdfinfo"), filepath.Join(dir, "pdftotext"))

	old := BinTimeout
	BinTimeout = 150 * time.Millisecond
	t.Cleanup(func() { BinTimeout = old })

	start := time.Now()
	_, err := Extract("/docs/huge.pdf")
	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second, "the deadline must kill the hung binary")
	require.Contains(t, err.Error(), "не ответил за")
	require.Contains(t, err.Error(), "/docs/huge.pdf")
}

func TestExtractBinaryFailureMentionsPath(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "pdfinfo", "echo 'Syntax Error: broken' >&2; exit 99\n")
	overrideBins(t, filepath.Join(dir, "pdfinfo"), filepath.Join(dir, "pdftotext"))

	_, err := Extract("/docs/broken.pdf")
	require.Error(t, err)
	require.Contains(t, err.Error(), "/docs/broken.pdf")
	require.Contains(t, err.Error(), "pdfinfo")
}

func TestExtractToleratesMissingFields(t *testing.T) {
	fakePair(t, "Pages:           3\n", "")
	info, err := Extract("/any/file.pdf")
	require.NoError(t, err)
	require.Equal(t, Info{Pages: 3}, info)
}

// --- FirstPageText -----------------------------------------------------------

func TestFirstPageTextCollapsesWhitespaceAndControlChars(t *testing.T) {
	fakePair(t, "", "  The Journal  of Hellenic\n\nStudies \x0c (2023)...\n\t")
	text, err := FirstPageText("/any/file.pdf", 500)
	require.NoError(t, err)
	require.Equal(t, "The Journal of Hellenic Studies (2023)...", text)
}

func TestFirstPageTextTruncatesToMaxChars(t *testing.T) {
	long := strings.Repeat("word ", 300) // 1500 chars
	fakePair(t, "", long)
	text, err := FirstPageText("/any/file.pdf", 500)
	require.NoError(t, err)
	require.LessOrEqual(t, len([]rune(text)), 500)
	require.Greater(t, len([]rune(text)), 450)
	require.False(t, strings.HasSuffix(text, " "), "must not end with a truncated-space artifact")
	require.True(t, strings.HasPrefix(text, "word word "))
}

func TestFirstPageTextEmptyOutputIsValid(t *testing.T) {
	fakePair(t, "", "")
	text, err := FirstPageText("/any/file.pdf", 500)
	require.NoError(t, err)
	require.Empty(t, text)
}

func TestFirstPageTextBinaryFailure(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "pdfinfo", "")
	writeScript(t, dir, "pdftotext", "echo 'pdftotext: corrupt' >&2; exit 3\n")
	overrideBins(t, filepath.Join(dir, "pdfinfo"), filepath.Join(dir, "pdftotext"))

	_, err := FirstPageText("/any/file.pdf", 500)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pdftotext")
}

// --- DeriveMeta --------------------------------------------------------------

const fullPdfinfoOut = "Title:           Aristophanes Frogs and reading culture\n" +
	"Author:          Bernd Seidensticker\n" +
	"CreationDate:    2021-09-15T11:27:10+03\n" +
	"Pages:          12\n"

func TestDeriveMetaFullFromPdfMetadata(t *testing.T) {
	fakePair(t, fullPdfinfoOut,
		"The Journal of Hellenic Studies (2023), 143, 147-166\n\nRESEARCH ARTICLE\n\nAristophanes' Frogs and reading culture in Athens.\n")
	dir := t.TempDir()
	path := touch(t, dir, "01_schmitz_aristophanes_frogs.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.Equal(t, Meta{
		Title:       "Aristophanes Frogs and reading culture",
		Creator:     "schmitz, Bernd Seidensticker",
		Year:        "2021",
		Description: "The Journal of Hellenic Studies (2023), 143, 147-166 RESEARCH ARTICLE Aristophanes' Frogs and reading culture in Athens.",
		Subjects:    []string{"athens", "aristophanes frogs"},
		Language:    "eng",
	}, m)
}

func TestDeriveMetaTitleFallsBackToFilename(t *testing.T) {
	fakePair(t, "Author:          Someone\nCreationDate:    2007-12-08T00:00:00Z\nPages:          4\n",
		"plain ascii text enough for the language gate to fire clearly.")
	dir := t.TempDir()
	path := touch(t, dir, "02_prokopyeva_kalokagathia.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.Equal(t, "Prokopyeva Kalokagathia", m.Title)
	require.Equal(t, "prokopyeva, Someone", m.Creator)
	require.Equal(t, "2007", m.Year)
}

func TestDeriveMetaLanguageRus(t *testing.T) {
	fakePair(t, "Title:          Тест\nCreationDate:    2015-01-30T14:09:07+06\nPages:          3\n",
		"Аристофан и афинская читательская культура в комедии.")
	dir := t.TempDir()
	path := touch(t, dir, "01_ivanov_komedia.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.Equal(t, "rus", m.Language)
	require.Equal(t, "2015", m.Year)
}

// The language window is the FULL first page, not the 500-char description
// budget: a Russian paper opening with a long English abstract must not be
// tagged eng off its title page alone.
func TestDeriveMetaLanguageRusAfterLongEnglishAbstract(t *testing.T) {
	abstract := strings.Repeat("This is an english abstract sentence. ", 16) // ~620 chars, ascii
	body := strings.Repeat("Аристофан и афинская культура в древней комедии города. ", 6)
	require.Greater(t, len([]rune(abstract)), 500, "precondition: the abstract alone exceeds the old 500-char window")
	fakePair(t, "Title:          Тест\nPages:          3\n", abstract+body)
	dir := t.TempDir()
	path := touch(t, dir, "01_ivanov_komedia.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.Equal(t, "rus", m.Language, "the Russian body must win over the English front matter")
	// The description budget is unchanged: capped at 500 runes of the head.
	require.LessOrEqual(t, len([]rune(m.Description)), 500)
}

func TestDeriveMetaLanguageOmittedWhenMixed(t *testing.T) {
	// Greek dominates; neither the 30% cyrillic nor the 70% ascii gate
	// fires (cyr 3/22 = 14%, ascii 4/22 = 18%).
	fakePair(t, "Pages:          1\n", "αβγδε αβγδε αβγδε три here.")
	dir := t.TempDir()
	path := touch(t, dir, "01_x_topic.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.Empty(t, m.Language)
}

func TestDeriveMetaNoPdfAuthorOnlyFilenameSegment(t *testing.T) {
	fakePair(t, "Title:          X\nPages:          1\n", "ascii text.")
	dir := t.TempDir()
	path := touch(t, dir, "05_pritchard_sport_war.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.Equal(t, "pritchard", m.Creator)
}

func TestDeriveMetaFilenameWithoutLeadingNumber(t *testing.T) {
	fakePair(t, fullPdfinfoOut, "text")
	dir := t.TempDir()
	path := touch(t, dir, "chapter_one.pdf")

	_, err := DeriveMeta(path, "athens")
	require.Error(t, err)
	require.Contains(t, err.Error(), "chapter_one.pdf")
	require.Contains(t, err.Error(), "NN_автор_тема")
}

func TestDeriveMetaDescriptionCappedAtMaxChars(t *testing.T) {
	fakePair(t, "Pages:          1\n", strings.Repeat("слово ", 200))
	dir := t.TempDir()
	path := touch(t, dir, "01_a_topic.pdf")

	m, err := DeriveMeta(path, "athens")
	require.NoError(t, err)
	require.LessOrEqual(t, len([]rune(m.Description)), 500)
}

// --- Identifier --------------------------------------------------------------

func TestIdentifierCanonical(t *testing.T) {
	id, err := Identifier("athens", "01_schmitz_aristophanes_frogs.pdf")
	require.NoError(t, err)
	require.Equal(t, "athens-1-schmitz-aristophanes-frogs", id)
	require.NoError(t, ias3.ValidateIdentifier(id))
}

func TestIdentifierStripsLeadingZeros(t *testing.T) {
	id, err := Identifier("sparta", "007_topic_x.pdf")
	require.NoError(t, err)
	require.Equal(t, "sparta-7-topic-x", id)

	id, err = Identifier("sparta", "00_z.pdf")
	require.NoError(t, err)
	require.Equal(t, "sparta-0-z", id)
}

func TestIdentifierNormalizesSlug(t *testing.T) {
	id, err := Identifier("athens", "03_Östliche Weisheit--in!!Athens.pdf")
	require.NoError(t, err)
	require.Equal(t, "athens-3-stliche-weisheit-in-athens", id)
	require.NoError(t, ias3.ValidateIdentifier(id))
}

func TestIdentifierEmptySlugKeepsDirNN(t *testing.T) {
	id, err := Identifier("athens", "05_.pdf")
	require.NoError(t, err)
	require.Equal(t, "athens-5", id)

	id, err = Identifier("athens", "06__.pdf")
	require.NoError(t, err)
	require.Equal(t, "athens-6", id)
}

func TestIdentifierNormalizesDirToo(t *testing.T) {
	id, err := Identifier("Athens-2023!", "01_x.pdf")
	require.NoError(t, err)
	require.Equal(t, "athens-2023-1-x", id)
}

func TestIdentifierMissingNumberErrors(t *testing.T) {
	_, err := Identifier("athens", "no_number_here.pdf")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no_number_here.pdf")
}

func TestIdentifierTruncatesSlugAtWordBoundary(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 8)+"-", 15) + "tail" // 144 chars
	id, err := Identifier("athens", "01_"+long+".pdf")
	require.NoError(t, err)
	require.LessOrEqual(t, len(id), 80, "identifier must fit the archive.org limit")
	require.NoError(t, ias3.ValidateIdentifier(id))
	require.True(t, strings.HasPrefix(id, "athens-1-"))
	require.False(t, strings.HasSuffix(id, "-"))
	// The cut must land on a word boundary: the tail after the last '-'
	// of the id equals a whole word of the original slug.
	full := "athens-1-" + long
	require.True(t, strings.HasPrefix(full, id), "id must be a prefix of the untruncated form")
	require.Equal(t, byte('-'), full[len(id)], "the next source char must be the word separator")
}

func TestIdentifierExtremesStillValid(t *testing.T) {
	// Minimal dir: the scheme still clears the 5-char archive.org floor.
	id, err := Identifier("a", "01_x.pdf")
	require.NoError(t, err)
	require.Equal(t, "a-1-x", id)
	require.NoError(t, ias3.ValidateIdentifier(id))
}

// A directory name that cannot fold into identifier characters (cyrillic)
// fails with the explicit rename guidance, not the generic validation
// error (fix round 2).
func TestIdentifierCyrillicDirExplicitError(t *testing.T) {
	_, err := Identifier("Афины", "01_x.pdf")
	require.Error(t, err)
	require.Contains(t, err.Error(), `имя каталога "Афины" не свёртывается в допустимые символы идентификатора — используйте латиницу`)
}
