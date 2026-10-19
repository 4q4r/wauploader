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

// TestUploadRequiresKeysUpFront: a cookies-only configuration fails with
// exit 3 BEFORE any hashing or network work — IA-S3 never accepts cookies
// (auth.Resolve doc delegation).
func TestUploadRequiresKeysUpFront(t *testing.T) {
	fb := newFakeBackend(t)
	cfg := writeTestConfig(t, `
[auth]
cookie_user = "cookieuser"
cookie_sig = "cookiesig"

[limits]
save_per_minute = 100000
save_concurrency = 1
upload_concurrency = 1
per_host_concurrency = 1
job_wait = "20m"
`)
	dir := t.TempDir()
	file := filepath.Join(dir, "big.bin")
	require.NoError(t, os.WriteFile(file, []byte("payload"), 0o600))
	jp := filepath.Join(dir, "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "upload", "--no-derive",
		"--item", "keys-item-1", "--collection", "movies", "--mediatype", "movies", file)
	require.Equal(t, ExitAuth, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Empty(t, fb.requests(), "cookie-only config must fail before any HTTP request")
	require.Contains(t, r.errOut, "s3_access")
	require.Contains(t, r.errOut, "s3_secret")
}

// TestUploadDeriveFlagsContradiction: --no-derive and --wait-derive are
// mutually exclusive; the pair is a usage error (exit 2), never wait-wins.
func TestUploadDeriveFlagsContradiction(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	r := runRoot(t, "--config", cfg, "upload", "--no-derive", "--wait-derive",
		"--item", "contra-item-1", "--collection", "movies", "--mediatype", "movies", file)
	require.Equal(t, ExitUsage, r.code)
	require.Contains(t, r.errOut, "--wait-derive")
	require.Contains(t, r.errOut, "--no-derive")
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
