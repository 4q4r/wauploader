// --auto-meta command tests (Task 14): the flag contract, the dry-run
// derived plan, and a full run against the fake IA-S3 backend with the
// poppler binaries replaced by deterministic fakes.
package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/4q4r/wauploader/internal/pdfmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakePdfinfoOut = "Title:           Derived Title From PDF\n" +
	"Author:          PDF Author\n" +
	"CreationDate:    2021-09-15T11:27:10+03\n" +
	"Pages:          12\n"

const fakePdftotextOut = "The first page text with ascii dominance.\n"

// writeFakePoppler installs fake pdfinfo/pdftotext scripts printing canned
// output and repoints the pdfmeta injection seams for the test's lifetime.
func writeFakePoppler(t *testing.T, pdfinfoOut, pdftotextOut string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pdfinfo"),
		[]byte("#!/bin/sh\nprintf '%s' \""+pdfinfoOut+"\"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pdftotext"),
		[]byte("#!/bin/sh\nprintf '%s' \""+pdftotextOut+"\"\n"), 0o755))

	oldInfo, oldText := pdfmeta.PDFInfoBin, pdfmeta.PdftotextBin
	pdfmeta.PDFInfoBin = filepath.Join(dir, "pdfinfo")
	pdfmeta.PdftotextBin = filepath.Join(dir, "pdftotext")
	t.Cleanup(func() { pdfmeta.PDFInfoBin, pdfmeta.PdftotextBin = oldInfo, oldText })
}

// autoMetaFixture creates dir/01_test_author_topic.pdf and returns its path.
func autoMetaFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "athens")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	file := filepath.Join(dir, "01_test_author_topic.pdf")
	require.NoError(t, os.WriteFile(file, []byte("%PDF-stub"), 0o600))
	return file
}

// TestUploadAutoMetaFlagContract: --auto-meta requires --identifier-scheme
// dir-nn-slug, and the pair excludes --item/--identifier-from/--key. All
// violations are usage errors (exit 2) before any work.
func TestUploadAutoMetaFlagContract(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "01_author_topic.pdf")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	base := []string{"--config", cfg, "upload", "--collection", "opensource_texts", "--mediatype", "texts"}

	// --auto-meta without --identifier-scheme.
	r := runRoot(t, append(base, "--auto-meta", file)...)
	require.Equal(t, ExitUsage, r.code)
	require.Contains(t, r.errOut, "--identifier-scheme")

	// --identifier-scheme without --auto-meta.
	r = runRoot(t, append(base, "--identifier-scheme", "dir-nn-slug", "--item", "some-item-1", file)...)
	require.Equal(t, ExitUsage, r.code)
	require.Contains(t, r.errOut, "--auto-meta")

	// An unknown scheme value.
	r = runRoot(t, append(base, "--auto-meta", "--identifier-scheme", "uuid", file)...)
	require.Equal(t, ExitUsage, r.code)
	require.Contains(t, r.errOut, "dir-nn-slug")

	// Conflicts with the explicit identifier/key sources.
	for _, extra := range [][]string{
		{"--item", "some-item-1"},
		{"--identifier-from", "x-{{.Base}}"},
		{"--key", "k.pdf"},
	} {
		args := append(append([]string{}, base...), "--auto-meta", "--identifier-scheme", "dir-nn-slug")
		args = append(args, extra...)
		r = runRoot(t, append(args, file)...)
		require.Equal(t, ExitUsage, r.code, "args=%v", args)
		require.Contains(t, r.errOut, extra[0], "the error must name the conflicting flag")
	}
}

// TestUploadAutoMetaBadFilename: a file outside the NN_author_topic
// convention is a DATA error of the file set, not CLI misuse — exit 1 with
// the Russian convention error (review ruling, fix round 1).
func TestUploadAutoMetaBadFilename(t *testing.T) {
	newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	cfg := writeTestConfig(t, testConfigFull)
	file := filepath.Join(t.TempDir(), "chapter_one.pdf")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	r := runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", file)
	require.Equal(t, ExitSomeFailed, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "chapter_one.pdf")
	require.Contains(t, r.errOut, "NN_автор_тема")
}

// TestUploadAutoMetaDryRunPlan: dry-run prints the full derived plan
// (identifier, title, author, collection, language, subjects, description
// head) and touches no network — the owner's complete pre-ship surface.
func TestUploadAutoMetaDryRunPlan(t *testing.T) {
	fb := newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)

	r := runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", "--dry-run", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "athens-1-test-author-topic")
	require.Contains(t, r.out, "Derived Title From PDF")
	require.Contains(t, r.out, "opensource_texts")
	require.Contains(t, r.out, "eng")
	require.Contains(t, r.out, "Автор: test", "creator must be on the review surface (default file source)")
	require.Contains(t, r.out, "Темы: athens, author topic", "subjects must be on the review surface")
	require.Contains(t, r.out, "Описание: The first page text with ascii dominance.", "a description head must be shown")
	require.Contains(t, r.out, "Итог dry-run")
	require.Empty(t, fb.requests(), "dry-run must not issue any HTTP request")
}

// TestUploadAutoMetaDuplicateIdentifiers: two filenames mapping to one
// derived identifier (01_x.pdf and 1_x.pdf) would silently merge items —
// the batch fails with exit 2 naming the collision.
func TestUploadAutoMetaDuplicateIdentifiers(t *testing.T) {
	newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	cfg := writeTestConfig(t, testConfigFull)
	dir := filepath.Join(t.TempDir(), "athens")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	a := filepath.Join(dir, "01_x_topic.pdf")
	b := filepath.Join(dir, "1_x_topic.pdf")
	require.NoError(t, os.WriteFile(a, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("y"), 0o600))

	r := runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", "--dry-run", a, b)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "athens-1-x-topic", "both files derive the same identifier")
	require.Contains(t, r.errOut, "01_x_topic.pdf")
	require.Contains(t, r.errOut, "1_x_topic.pdf")
}

// TestUploadAutoMetaFullRun: the derived metadata rides the PUT —
// per-file title/description, subjects/language/year extras.
func TestUploadAutoMetaFullRun(t *testing.T) {
	fb := newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--auto-meta",
		"--identifier-scheme", "dir-nn-slug", "--collection", "opensource_texts", "--mediatype", "texts", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Equal(t, 1, fb.puts)

	fb.mu.Lock()
	path, hdr := fb.putPaths[0], fb.putHeaders[0]
	fb.mu.Unlock()

	require.Equal(t, "/athens-1-test-author-topic/01_test_author_topic.pdf", path)
	assert.Equal(t, "Derived Title From PDF", hdr.Get("x-archive-meta-title"))
	assert.Equal(t, "The first page text with ascii dominance.", hdr.Get("x-archive-meta-description"))
	assert.Equal(t, "opensource_texts", hdr.Get("x-archive-meta01-collection"))
	assert.Equal(t, "texts", hdr.Get("x-archive-meta-mediatype"))
	assert.Equal(t, "athens", hdr.Get("x-archive-meta00-subject"))
	assert.Equal(t, "author topic", hdr.Get("x-archive-meta01-subject"))
	assert.Equal(t, "eng", hdr.Get("x-archive-meta-language"))
	assert.Equal(t, "2021", hdr.Get("x-archive-meta-year"))
	assert.Equal(t, "test", hdr.Get("x-archive-meta-creator"), "default --creator-source file: filename author only")
	assert.Empty(t, hdr.Get("x-archive-meta00-creator"), "single creator value rides unnumbered")

	// The derived identifier and title surface in the RU console output.
	require.Contains(t, r.out, "athens-1-test-author-topic")
	require.Contains(t, r.out, "Derived Title From PDF")
}

// TestUploadAutoMetaBadMetaNameFailsBeforePoppler: a malformed --meta name
// is a usage error (exit 2) BEFORE any poppler invocation — a data error
// must not mask the usage error (round-4 minor).
func TestUploadAutoMetaBadMetaNameFailsBeforePoppler(t *testing.T) {
	newFakeBackend(t)
	dir := t.TempDir()
	counter := filepath.Join(dir, "poppler-count")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pdfinfo"),
		[]byte("#!/bin/sh\necho x >> "+counter+"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pdftotext"),
		[]byte("#!/bin/sh\necho x >> "+counter+"\n"), 0o755))
	oldInfo, oldText := pdfmeta.PDFInfoBin, pdfmeta.PdftotextBin
	pdfmeta.PDFInfoBin = filepath.Join(dir, "pdfinfo")
	pdfmeta.PdftotextBin = filepath.Join(dir, "pdftotext")
	t.Cleanup(func() { pdfmeta.PDFInfoBin, pdfmeta.PdftotextBin = oldInfo, oldText })

	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)

	r := runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", "--meta", "bad name=x", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "bad name")

	b, err := os.ReadFile(counter)
	require.True(t, os.IsNotExist(err) || len(b) == 0, "poppler must not run: %s (%v)", string(b), err)
}

// TestUploadAutoMetaLanguageRusPlan: the cyrillic heuristic flips to rus
// end-to-end from the pdftotext fake.
func TestUploadAutoMetaLanguageRusPlan(t *testing.T) {
	fb := newFakeBackend(t)
	writeFakePoppler(t, "Title:          Т\nPages:          1\n",
		"Аристофан и афинская читательская культура в древней комедии.")
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)

	r := runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", "--dry-run", file)
	require.Equal(t, ExitOK, r.code)
	require.Contains(t, r.out, "rus")
	require.Empty(t, fb.requests())
}

// TestUploadAutoMetaExplicitFlagsOverrideDerived (owner fix batch):
// explicitly passed flags override the auto-meta values per parameter —
// --title/--description replace the derived fields; a --meta name wins
// entirely for that name while derived names not mentioned stay. The
// dry-run review surface shows the overridden (final) values.
func TestUploadAutoMetaExplicitFlagsOverrideDerived(t *testing.T) {
	fb := newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--auto-meta",
		"--identifier-scheme", "dir-nn-slug", "--collection", "opensource_texts", "--mediatype", "texts",
		"--title", "Custom Title", "--description", "Custom description",
		"--meta", "subject=x", "--meta", "publisher=MIT", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Equal(t, 1, fb.puts)

	fb.mu.Lock()
	hdr := fb.putHeaders[0]
	fb.mu.Unlock()

	assert.Equal(t, "Custom Title", hdr.Get("x-archive-meta-title"), "--title overrides the derived title")
	assert.Equal(t, "Custom description", hdr.Get("x-archive-meta-description"), "--description overrides the derived description")
	assert.Equal(t, "x", hdr.Get("x-archive-meta-subject"), "explicit subject wins entirely for the name")
	assert.Empty(t, hdr.Get("x-archive-meta00-subject"), "no numbered subject: the derived values are dropped for the name")
	assert.Equal(t, "MIT", hdr.Get("x-archive-meta-publisher"), "names the derivation never emits ride along")
	assert.Equal(t, "eng", hdr.Get("x-archive-meta-language"), "derived names not mentioned stay")
	assert.Equal(t, "2021", hdr.Get("x-archive-meta-year"))
	assert.Equal(t, "test", hdr.Get("x-archive-meta-creator"))

	// The dry-run surface shows the final (overridden) values — against a
	// fresh backend and journal: the full run above legitimately issued
	// requests.
	fbDry := newFakeBackend(t)
	r = runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", "--dry-run",
		"--title", "Custom Title", "--meta", "subject=x", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "Название: «Custom Title»")
	require.Contains(t, r.out, "Темы: x")
	require.NotContains(t, r.out, "Derived Title From PDF")
	require.Empty(t, fbDry.requests(), "dry-run must not issue any HTTP request")
}

// TestUploadAutoMetaCreatorSource (owner fix batch): --creator-source
// selects the creator values on the wire — default file (one unnumbered
// header), pdf (the PDF Author), both (numbered multi-value headers,
// file author first).
func TestUploadAutoMetaCreatorSource(t *testing.T) {
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)
	// Each sub-run needs its own journal: with a shared one the second
	// run would skip the file as already done and never PUT.
	jp := func() string { return filepath.Join(t.TempDir(), "journal.bbolt") }

	fb := newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	args := append([]string{"--config", cfg, "--journal", jp(), "upload", "--auto-meta",
		"--identifier-scheme", "dir-nn-slug", "--collection", "opensource_texts", "--mediatype", "texts"}, file)
	r := runRoot(t, args...) // no flag: default file
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	fb.mu.Lock()
	hdr := fb.putHeaders[0]
	fb.mu.Unlock()
	assert.Equal(t, "test", hdr.Get("x-archive-meta-creator"))
	assert.Empty(t, hdr.Get("x-archive-meta00-creator"))

	fb = newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	args = append([]string{"--config", cfg, "--journal", jp(), "upload", "--auto-meta",
		"--identifier-scheme", "dir-nn-slug", "--collection", "opensource_texts",
		"--mediatype", "texts", "--creator-source", "pdf"}, file)
	r = runRoot(t, args...)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	fb.mu.Lock()
	hdr = fb.putHeaders[0]
	fb.mu.Unlock()
	assert.Equal(t, "PDF Author", hdr.Get("x-archive-meta-creator"), "pdf source: the PDF Author only")

	fb = newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	args = append([]string{"--config", cfg, "--journal", jp(), "upload", "--auto-meta",
		"--identifier-scheme", "dir-nn-slug", "--collection", "opensource_texts",
		"--mediatype", "texts", "--creator-source", "both"}, file)
	r = runRoot(t, args...)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	fb.mu.Lock()
	hdr = fb.putHeaders[0]
	fb.mu.Unlock()
	assert.Equal(t, "test", hdr.Get("x-archive-meta00-creator"), "both: file author first")
	assert.Equal(t, "PDF Author", hdr.Get("x-archive-meta01-creator"), "both: PDF author as a distinct value")
	assert.Empty(t, hdr.Get("x-archive-meta-creator"), "multi-value creator rides numbered headers only")
}

// TestUploadAutoMetaCreatorSourceFlagContract: a bogus --creator-source
// value is a usage error (exit 2), and without --auto-meta the flag is a
// usage error too — both before any poppler work.
func TestUploadAutoMetaCreatorSourceFlagContract(t *testing.T) {
	newFakeBackend(t)
	writeFakePoppler(t, fakePdfinfoOut, fakePdftotextOut)
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)
	base := []string{"--config", cfg, "upload", "--collection", "opensource_texts", "--mediatype", "texts", "--dry-run"}

	r := runRoot(t, append(append([]string{}, base...), "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--creator-source", "dropbox", file)...)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "--creator-source")
	require.Contains(t, r.errOut, "file")

	r = runRoot(t, append(append([]string{}, base...), "--item", "some-item-1",
		"--creator-source", "file", file)...)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "--auto-meta")
}

// TestUploadAutoMetaDegenerateDescriptionOmitted (owner fix batch): a
// whitespace-only first page leaves the description empty — no
// «Описание» line in the dry-run, no description header on the PUT.
func TestUploadAutoMetaDegenerateDescriptionOmitted(t *testing.T) {
	fb := newFakeBackend(t)
	writeFakePoppler(t, "Title:          X\nPages:  1\n", "   \n\t \x0c ")
	cfg := writeTestConfig(t, testConfigFull)
	file := autoMetaFixture(t)

	r := runRoot(t, "--config", cfg, "upload", "--auto-meta", "--identifier-scheme", "dir-nn-slug",
		"--collection", "opensource_texts", "--mediatype", "texts", "--dry-run", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.NotContains(t, r.out, "Описание:")

	jp := filepath.Join(t.TempDir(), "journal.bbolt")
	r = runRoot(t, "--config", cfg, "--journal", jp, "upload", "--auto-meta",
		"--identifier-scheme", "dir-nn-slug", "--collection", "opensource_texts",
		"--mediatype", "texts", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Equal(t, 1, fb.puts)

	fb.mu.Lock()
	hdr := fb.putHeaders[0]
	fb.mu.Unlock()
	assert.Empty(t, hdr.Get("x-archive-meta-description"), "a degenerate description is omitted from the PUT")
}
