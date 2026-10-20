// Tests for the --key-from contract: per-file keys rendered from a
// template, the --base-dir root of the relative paths, the recursive
// expansion of a directory argument, and the usage errors of the flag
// combinations.
package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyFromTree builds a small tree and returns its root:
//
//	<root>/001.json
//	<root>/sub/002.json
//	<root>/sub/deep/003.txt
func keyFromTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "top-taganrog")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))

	for _, file := range []struct {
		rel  string
		body string
	}{
		{"001.json", "{}"},
		{"sub/002.json", "{}"},
		{"sub/deep/003.txt", "x"},
	} {
		p := filepath.Join(root, filepath.FromSlash(file.rel))
		require.NoError(t, os.WriteFile(p, []byte(file.body), 0o600))
	}

	return root
}

// keyFromUploadLines returns the "Загрузить:" lines of a dry-run in order.
func keyFromUploadLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Загрузить: ") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}

	return lines
}

// TestKeyFromRendersPerFileKeys: the template renders once per file — the
// key of every file of the tree carries its own relative path.
func TestKeyFromRendersPerFileKeys(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	root := keyFromTree(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "tree-item-1", "--key-from", "{Path}",
		"--collection", "opensource", "--mediatype", "data", root)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)

	lines := keyFromUploadLines(r.out)
	require.Len(t, lines, 3, "one line per file: %v", lines)
	require.Contains(t, lines[0], "tree-item-1/top-taganrog/001.json")
	require.Contains(t, lines[1], "tree-item-1/top-taganrog/sub/002.json")
	require.Contains(t, lines[2], "tree-item-1/top-taganrog/sub/deep/003.txt")
}

// TestKeyFromTemplateFields: every documented template field is
// available — Path, Name, Base, Ext, Dir and Abs.
func TestKeyFromTemplateFields(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "data.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "fields-item-1", "--key-from", "{Name}|{Base}|{Ext}|{Dir}|{Path}",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "fields-item-1/data.json|data|json||data.json")
}

// TestKeyFromRequiresItem: the key template needs an explicit shared
// identifier — the whole tree goes into ONE item.
func TestKeyFromRequiresItem(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))

	r := runRoot(t, "--config", cfg, "upload", "--dry-run",
		"--key-from", "{Path}", "--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "--key-from")
	require.Contains(t, r.errOut, "--item")
}

// TestKeyFromConflictsWithKey: --key-from computes the key per file, so a
// single --key is contradictory.
func TestKeyFromConflictsWithKey(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "conflict-item-1", "--key", "k.json", "--key-from", "{Path}",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "--key-from")
	require.Contains(t, r.errOut, "--key")

	// The identifier template is contradictory too: the identifier of the
	// whole tree is --item.
	r = runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--identifier-from", "x-{Base}", "--key-from", "{Path}",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "--identifier-from")
}

// TestKeyFromTemplateError: a template that does not compile is a usage
// error (exit 2) with the template error named.
func TestKeyFromTemplateError(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "tmpl-item-1", "--key-from", "{Path",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "шаблон")

	// A field that does not exist in the data is a render error.
	r = runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "tmpl-item-1", "--key-from", "{Nope}",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
}

// TestKeyFromRejectsTraversal: a key with a ".." segment would escape the
// item's directory — a usage error (exit 2).
func TestKeyFromRejectsTraversal(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "trav-item-1", "--key-from", "../{Name}",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "..")
}

// TestKeyFromEmptyKeyRejected: a template that renders nothing is a usage
// error — an empty key has no meaning inside the item.
func TestKeyFromEmptyKeyRejected(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "empty-item-1", "--key-from", "{Dir}",
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "пустой ключ")
}

// TestKeyFromDuplicateKeyRejected: two files rendering the same key would
// overwrite each other inside the item — a usage error (exit 2), the same
// rule the auto-meta mode applies to duplicate identifiers.
func TestKeyFromDuplicateKeyRejected(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "b"), 0o755))

	first := filepath.Join(dir, "a", "same.json")
	second := filepath.Join(dir, "b", "same.json")
	require.NoError(t, os.WriteFile(first, []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("{}"), 0o600))

	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "dup-item-1", "--key-from", "{Base}.json",
		"--collection", "opensource", "--mediatype", "data", dir)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "same.json")
}

// TestDirectoryArgExpandsRecursively: a directory argument expands into
// the files of its tree, sorted by the relative path — a deterministic
// order (the same order on every run).
func TestDirectoryArgExpandsRecursively(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	root := keyFromTree(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	args := []string{"--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "dir-item-1", "--key-from", "{Path}",
		"--collection", "opensource", "--mediatype", "data", root}

	r := runRoot(t, args...)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)

	lines := keyFromUploadLines(r.out)
	require.Len(t, lines, 3, "the whole tree must be listed: %v", lines)
	require.Contains(t, lines[0], "top-taganrog/001.json")
	require.Contains(t, lines[1], "top-taganrog/sub/002.json")
	require.Contains(t, lines[2], "top-taganrog/sub/deep/003.txt")

	// The same command twice gives the same order.
	r2 := runRoot(t, args...)
	require.Equal(t, ExitOK, r2.code, "out=%s errOut=%s", r2.out, r2.errOut)
	require.Equal(t, lines, keyFromUploadLines(r2.out), "the expansion order must be deterministic")
}

// TestDirectoryRequiresKeyFrom: without --key-from a directory would lose
// its structure — a usage error (exit 2).
func TestDirectoryRequiresKeyFrom(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	root := keyFromTree(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "dir-item-1", "--collection", "opensource", "--mediatype", "data", root)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "каталог")
	require.Contains(t, r.errOut, "--key-from")
}

// TestDryRunShowsPerFileKeys: the dry-run shows the per-file
// identifier/key mapping (one line per file) and names the template and
// the base dir — the owner's review surface before a live run.
func TestDryRunShowsPerFileKeys(t *testing.T) {
	fb := newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	root := keyFromTree(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "dry-item-1", "--key-from", "{Path}",
		"--collection", "opensource", "--mediatype", "data", root)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "--key-from")
	require.Contains(t, r.out, "top-taganrog")
	require.Contains(t, r.out, "dry-item-1/top-taganrog/001.json")
	require.Contains(t, r.out, "dry-item-1/top-taganrog/sub/deep/003.txt")
	require.Empty(t, fb.requests(), "dry-run must not issue any HTTP request")
	journalFileAbsent(t, jp)
}

// TestKeyFromBaseDirFlag: --base-dir sets the root of the relative paths.
func TestKeyFromBaseDirFlag(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	root := keyFromTree(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--item", "base-item-1", "--key-from", "{Path}", "--base-dir", root,
		"--collection", "opensource", "--mediatype", "data", root)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "base-item-1/001.json", "the base dir is the root of the relative paths")
	require.Contains(t, r.out, "base-item-1/sub/deep/003.txt")
}

// TestKeyFromBaseDirWithoutKeyFromRejected: --base-dir only makes sense
// with --key-from.
func TestKeyFromBaseDirWithoutKeyFromRejected(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	require.NoError(t, os.WriteFile(file, []byte("{}"), 0o600))

	r := runRoot(t, "--config", cfg, "upload", "--dry-run",
		"--item", "base-item-2", "--base-dir", dir,
		"--collection", "opensource", "--mediatype", "data", file)
	require.Equal(t, ExitUsage, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.errOut, "--base-dir")
}

// TestKeyFromFullRunOneItem: the whole tree goes into ONE item — the
// first PUT creates it and carries the size hint of every file, the last
// PUT queues the derive, and the nested keys keep the tree.
func TestKeyFromFullRunOneItem(t *testing.T) {
	fb := newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	root := keyFromTree(t)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload",
		"--item", "run-item-1", "--key-from", "{Path}",
		"--collection", "opensource", "--mediatype", "data", root)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Equal(t, 3, fb.puts, "every file of the tree is uploaded")

	fb.mu.Lock()
	paths := append([]string(nil), fb.putPaths...)
	headers := append([]http.Header(nil), fb.putHeaders...)
	fb.mu.Unlock()

	require.Equal(t, []string{
		"/run-item-1/top-taganrog/001.json",
		"/run-item-1/top-taganrog/sub/002.json",
		"/run-item-1/top-taganrog/sub/deep/003.txt",
	}, paths, "the keys keep the tree structure")

	// The first PUT creates the item and carries the whole-item size hint
	// (2 + 2 + 1 bytes).
	assert.Equal(t, "1", headers[0].Get("x-archive-auto-make-bucket"))
	assert.Equal(t, "5", headers[0].Get("x-archive-size-hint"))
	assert.Empty(t, headers[1].Get("x-archive-size-hint"))
	assert.Empty(t, headers[2].Get("x-archive-size-hint"))

	// Only the last PUT of the item queues the derive.
	assert.Equal(t, "0", headers[0].Get("x-archive-queue-derive"))
	assert.Equal(t, "0", headers[1].Get("x-archive-queue-derive"))
	assert.Equal(t, "1", headers[2].Get("x-archive-queue-derive"))
}
