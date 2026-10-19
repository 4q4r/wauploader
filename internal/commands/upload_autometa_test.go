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
	require.Contains(t, r.out, "Автор: test, PDF Author", "creator must be on the review surface")
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
	assert.Equal(t, "test, PDF Author", hdr.Get("x-archive-meta-creator"))

	// The derived identifier and title surface in the RU console output.
	require.Contains(t, r.out, "athens-1-test-author-topic")
	require.Contains(t, r.out, "Derived Title From PDF")
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
