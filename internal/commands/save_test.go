// save-command tests: flag→Options mapping, list parsing, exit codes, JSON
// mode, dry-run purity and the fake-server end-to-end run (docs/plan.md
// Task 12 Step 1).
package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/journal"
	"github.com/stretchr/testify/require"
)

// TestSaveFlagsMapToOptions: every flag flips exactly one Options field;
// unset flags keep the configuration values; target-site credentials ride
// flags only.
func TestSaveFlagsMapToOptions(t *testing.T) {
	cfg := config.Default()
	cfg.SPN2.CaptureOutlinks = true // config passthrough
	cfg.SPN2.IfNotArchivedWithin = "3d"
	cfg.SPN2.JSBehaviorTimeout = 5

	f := &saveFlags{
		captureAll:           true,
		captureScreenshot:    true,
		delayWBAvailability:  true,
		forceGet:             true,
		skipFirstArchive:     true,
		outlinksAvailability: true,
		emailResult:          true,
		ifNotArchivedWithin:  "120",
		jsBehaviorTimeout:    7,
		captureCookie:        "sid=1",
		useUserAgent:         "TargetUA/1",
		targetUsername:       "u",
		targetPassword:       "p",
	}
	o := saveOptionsFrom(cfg, f)
	require.True(t, o.CaptureAll, "capture-all")
	require.True(t, o.CaptureOutlinks, "capture-outlinks from config")
	require.True(t, o.CaptureScreenshot, "capture-screenshot")
	require.True(t, o.DelayWBAvailability, "delay-wb-availability")
	require.True(t, o.ForceGet, "force-get")
	require.True(t, o.SkipFirstArchive, "skip-first-archive")
	require.True(t, o.OutlinksAvailability, "outlinks-availability")
	require.True(t, o.EmailResult, "email-result")
	require.Equal(t, "120", o.IfNotArchivedWithin, "flag overrides config")
	require.Equal(t, 7, o.JSBehaviorTimeout, "flag overrides config")
	require.Equal(t, "sid=1", o.CaptureCookie)
	require.Equal(t, "TargetUA/1", o.UseUserAgent)
	require.Equal(t, "u", o.TargetUsername)
	require.Equal(t, "p", o.TargetPassword)

	zero := saveOptionsFrom(cfg, &saveFlags{jsBehaviorTimeout: -1})
	require.False(t, zero.CaptureAll, "unset flags stay off")
	require.True(t, zero.CaptureOutlinks, "config values pass through")
	require.Equal(t, "3d", zero.IfNotArchivedWithin)
	require.Equal(t, 5, zero.JSBehaviorTimeout)
	require.Empty(t, zero.CaptureCookie)
	require.Empty(t, zero.TargetPassword, "credentials never come from config")
}

// TestListFileParsing: one target per line, "#" comments, blank lines
// skipped, spaces trimmed, inline "#" (URL fragment) preserved.
func TestListFileParsing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "list.txt")
	body := "# top comment\n" +
		"https://example.com/a\n" +
		"\n" +
		"   \n" +
		"  # indented comment\n" +
		"https://example.com/b#frag\n" +
		"  https://example.com/c  \n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

	got, err := parseTargetList(p)
	require.NoError(t, err)
	require.Equal(t, []string{
		"https://example.com/a",
		"https://example.com/b#frag",
		"https://example.com/c",
	}, got)

	_, err = parseTargetList(filepath.Join(t.TempDir(), "absent.txt"))
	require.Error(t, err, "a missing list file is an error, not an empty list")
}

// TestExitCodes: 2 usage, 3 auth, 1 failed run, 0 clean run.
func TestExitCodes(t *testing.T) {
	cfgFull := writeTestConfig(t, testConfigFull)
	cfgNoAuth := writeTestConfig(t, testConfigNoAuth)

	// 2 — usage: upload without --item/--identifier-from fails offline.
	file := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	r := runRoot(t, "--config", cfgFull, "upload", file,
		"--collection", "movies", "--mediatype", "movies")
	require.Equal(t, ExitUsage, r.code, "errOut=%s", r.errOut)

	// 2 — configuration: broken TOML.
	bad := writeTestConfig(t, "[spn2\nbroken")
	r = runRoot(t, "--config", bad, "save", "--dry-run", "https://example.com/x")
	require.Equal(t, ExitUsage, r.code)

	// 3 — auth: valid configuration without any credentials.
	r = runRoot(t, "--config", cfgNoAuth, "save", "--dry-run", "https://example.com/x")
	require.Equal(t, ExitAuth, r.code, "errOut=%s", r.errOut)

	// 1 — a save run whose only target dies terminally.
	fb := newFakeBackend(t)
	fb.failTarget("https://example.com/err")
	r = runRoot(t, "--config", cfgFull, "--journal", filepath.Join(t.TempDir(), "j.bbolt"),
		"save", "https://example.com/err")
	require.Equal(t, ExitSomeFailed, r.code, "out=%s errOut=%s", r.out, r.errOut)

	// 0 — a clean single-URL save.
	fb = newFakeBackend(t)
	r = runRoot(t, "--config", cfgFull, "--journal", filepath.Join(t.TempDir(), "j.bbolt"),
		"save", "https://example.com/ok")
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
}

// TestJSONModeEvents: in --json mode every stdout line is one compact JSON
// object, glyphs are gone, and the terminal done event carries the snapshot
// URL.
func TestJSONModeEvents(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	r := runRoot(t, "--config", cfg, "--journal", filepath.Join(t.TempDir(), "j.bbolt"),
		"--json", "save", "https://example.com/json")
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)

	maps := decodeJSONObjects(t, r.out)
	var done map[string]any
	for _, m := range maps {
		if m["kind"] == "state" && m["state"] == "done" {
			done = m
		}
	}
	require.NotNil(t, done, "a done event must be present: %v", maps)
	require.Equal(t, "https://example.com/json", done["target"])
	require.Contains(t, done["snapshot_url"], "https://web.archive.org/web/20260101010101/")
	require.NotContains(t, r.out, "Итог", "the human summary line never appears in JSON mode")
}

// TestStatusUserJSON: `status --user --json` prints exactly one JSON object
// with the session counters.
func TestStatusUserJSON(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setUserStatus(`{"available":3,"processing":1}`)
	cfg := writeTestConfig(t, testConfigFull)

	r := runRoot(t, "--config", cfg, "--json", "status", "--user")
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)

	lines := nonEmptyLines(r.out)
	require.Len(t, lines, 1, "exactly one JSON object expected, got: %q", r.out)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &m))
	require.Equal(t, float64(3), m["available"])
	require.Equal(t, float64(1), m["processing"])
}

// TestDryRunTouchesNothing: with both fakes wired and counting, save and
// upload dry-runs issue zero HTTP requests, never create the journal and
// still print every would-be action in Russian.
func TestDryRunTouchesNothing(t *testing.T) {
	fb := newFakeBackend(t) // every request would land in the log
	cfg := writeTestConfig(t, testConfigFull)
	dir := t.TempDir()
	jp := filepath.Join(dir, "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp,
		"save", "--dry-run", "https://example.com/dry1", "https://example.com/dry2")
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Empty(t, fb.requests(), "dry-run must issue zero HTTP requests")
	journalFileAbsent(t, jp)
	require.Contains(t, r.out, "Сохранить: https://example.com/dry1")
	require.Contains(t, r.out, "Сохранить: https://example.com/dry2")

	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(f1, []byte("one"), 0o600))
	require.NoError(t, os.WriteFile(f2, []byte("two"), 0o600))
	jp2 := filepath.Join(dir, "j2.bbolt")
	r = runRoot(t, "--config", cfg, "--journal", jp2, "upload", "--dry-run", "--no-derive",
		"--item", "dry-item-1", "--collection", "movies", "--mediatype", "movies", f1, f2)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Empty(t, fb.requests(), "upload dry-run must issue zero HTTP requests")
	journalFileAbsent(t, jp2)
	require.Contains(t, r.out, "Загрузить: ")
	require.Contains(t, r.out, "dry-item-1")
}

// TestDryRunExitCodes: journal/force logic works without side effects — a
// journaled done target would be skipped (exit 1), a fresh one would run
// (exit 0).
func TestDryRunExitCodes(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	// Seed a done record through the real journal API (test setup, not the
	// dry-run path).
	st, err := journal.Open(jp)
	require.NoError(t, err)
	require.NoError(t, st.Put(&journal.Record{
		Kind: journal.KindSave, Target: "https://example.com/done", State: journal.StateDone,
	}))
	require.NoError(t, st.Close())

	r := runRoot(t, "--config", cfg, "--journal", jp,
		"save", "--dry-run", "https://example.com/done", "https://example.com/fresh")
	require.Equal(t, ExitSomeFailed, r.code, "a would-be skip is exit 1: out=%s", r.out)
	require.Contains(t, r.out, "Пропущено")
	require.Contains(t, r.out, "--force")
	require.Contains(t, r.out, "Сохранить: https://example.com/fresh")

	r = runRoot(t, "--config", cfg, "--journal", jp, "save", "--dry-run", "https://example.com/fresh")
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
}

// TestUnknownKeyWarning: an unknown TOML key produces a Russian warning on
// stderr naming the key, without failing the command (Task 2 ruling).
func TestUnknownKeyWarning(t *testing.T) {
	newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull+"capture_typo = true\n")
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	r := runRoot(t, "--config", cfg, "--journal", jp, "save", "--dry-run", "https://example.com/warn")
	require.Equal(t, ExitOK, r.code, "a unknown key warns but does not fail: errOut=%s", r.errOut)
	require.Contains(t, r.errOut, "Неизвестн")
	require.Contains(t, r.errOut, "capture_typo")
}

// TestEndToEndFake: fake SPN2 + fake IA-S3, save three URLs (one terminal
// error) and upload two files; assert journal states, exit codes and the
// summary line.
func TestEndToEndFake(t *testing.T) {
	fb := newFakeBackend(t)
	cfg := writeTestConfig(t, testConfigFull)
	jp := filepath.Join(t.TempDir(), "journal.bbolt")

	fb.failTarget("https://example.com/two")
	r := runRoot(t, "--config", cfg, "--journal", jp, "save",
		"https://example.com/one", "https://example.com/two", "https://example.com/three")
	require.Equal(t, ExitSomeFailed, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "Итог: сохранено 2 · пропущено 0 · отложено 0 · ошибок 1")

	st, err := journal.Open(jp)
	require.NoError(t, err)

	for target, wantState := range map[string]journal.State{
		"https://example.com/one":   journal.StateDone,
		"https://example.com/two":   journal.StateFailed,
		"https://example.com/three": journal.StateDone,
	} {
		rec, ok, err := st.Get(journal.KindSave, target)
		require.NoError(t, err)
		require.True(t, ok, "record for %s must exist", target)
		require.Equal(t, wantState, rec.State, "target %s", target)
	}
	rec, _, err := st.Get(journal.KindSave, "https://example.com/one")
	require.NoError(t, err)
	require.Contains(t, rec.SnapshotURL, "https://web.archive.org/web/20260101010101/https://example.com/one")
	require.Equal(t, 3, fb.saves, "three submits on the fake")
	// Release the bbolt flock before the upload run opens the same file.
	require.NoError(t, st.Close())

	// Upload two files with derive disabled; both land in the fake S3.
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(f1, []byte("content-one"), 0o600))
	require.NoError(t, os.WriteFile(f2, []byte("content-two-two"), 0o600))

	r = runRoot(t, "--config", cfg, "--journal", jp, "upload", "--no-derive",
		"--item", "e2e-item-1", "--collection", "movies", "--mediatype", "movies", f1, f2)
	require.Equal(t, ExitOK, r.code, "out=%s errOut=%s", r.out, r.errOut)
	require.Contains(t, r.out, "Итог: загружено 2 · пропущено 0 · отложено 0 · ошибок 0")
	require.Equal(t, 2, fb.puts)

	// Reopen for the upload assertions.
	st, err = journal.Open(jp)
	require.NoError(t, err)
	defer st.Close() //nolint:errcheck // final assertion handle
	abs1, err := filepath.Abs(f1)
	require.NoError(t, err)
	rec, ok, err := st.Get(journal.KindUpload, abs1+"\x00e2e-item-1/a.txt")
	require.NoError(t, err)
	require.True(t, ok, "upload record must exist for the composite target")
	require.Equal(t, journal.StateDone, rec.State)
	require.NotEmpty(t, rec.MD5, "the upload record keeps the MD5 digest")
}
