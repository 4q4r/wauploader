// Upload-command tests: the --item/--identifier-from contract, the meta
// flag parsing and the identifier template rendering (docs/plan.md
// Task 12 Step 1).
package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUploadRequiresItemOrTemplate: the identifier source is mandatory and
// exclusive — neither flag or both flags are usage errors (exit 2).
func TestUploadRequiresItemOrTemplate(t *testing.T) {
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	jp := filepath.Join(dir, "journal.bbolt")
	base := []string{"--config", cfg, "--journal", jp}

	// Neither --item nor --identifier-from.
	r := runRoot(t, append(base, "upload", file, "--collection", "movies", "--mediatype", "movies")...)
	require.Equal(t, ExitUsage, r.code)
	require.Contains(t, r.errOut, "--item", "the error must name the flags")
	require.Contains(t, r.errOut, "--identifier-from")

	// Both --item and --identifier-from.
	r = runRoot(t, append(base, "upload", file, "--item", "item-a",
		"--identifier-from", "x-{{.Base}}", "--collection", "movies", "--mediatype", "movies")...)
	require.Equal(t, ExitUsage, r.code)

	// --key without --item.
	r = runRoot(t, append(base, "upload", file, "--key", "k.txt",
		"--identifier-from", "x-{{.Base}}", "--collection", "movies", "--mediatype", "movies")...)
	require.Equal(t, ExitUsage, r.code)
}

// TestUploadIdentifierFromTemplate: the template renders per file over
// {{.Base}}, invalid identifiers and broken templates are usage errors.
func TestUploadIdentifierFromTemplate(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "page1")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	jp := filepath.Join(dir, "journal.bbolt")
	base := []string{"--config", cfg, "--journal", jp, "upload", "--dry-run",
		"--collection", "movies", "--mediatype", "movies"}

	r := runRoot(t, append(base, "--identifier-from", "custom-{{.Base}}", file)...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Contains(t, r.out, "custom-page1")
	require.Contains(t, r.out, "Загрузить: ")

	// The rendered identifier violates the archive.org rules (too short).
	r = runRoot(t, append(base, "--identifier-from", "a", file)...)
	require.Equal(t, ExitUsage, r.code)

	// The template does not compile.
	r = runRoot(t, append(base, "--identifier-from", "{{.Nope", file)...)
	require.Equal(t, ExitUsage, r.code)
}

// TestUploadMetaParsing: NAME=VALUE pairs, repeated names accumulate,
// malformed pairs are rejected.
func TestUploadMetaParsing(t *testing.T) {
	_, err := parseMetaFlags([]string{"broken"})
	require.Error(t, err, "a pair without '=' is rejected")

	_, err = parseMetaFlags([]string{"=novalue"})
	require.Error(t, err, "an empty name is rejected")

	m, err := parseMetaFlags([]string{"year=2001", "tags=one", "tags=two", "title=hello world"})
	require.NoError(t, err)
	require.Equal(t, []string{"2001"}, m["year"])
	require.Equal(t, []string{"one", "two"}, m["tags"])
	require.Equal(t, []string{"hello world"}, m["title"])
}
