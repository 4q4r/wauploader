// status-command tests: the user-status surface in JSON and human modes,
// plus the selection contract (exactly one of JOB_ID / --user / --system).
package commands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStatusUserJSON covers the user-status JSON shape (kept in
// save_test.go's suite name per the plan) and the human variant.
func TestStatusSelection(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setUserStatus(`{"available":3,"processing":1}`)
	cfg := writeTestConfig(t, testConfigFull)
	base := []string{"--config", cfg}

	// No selection and mixed selection are usage errors (exit 2).
	r := runRoot(t, append(base, "status")...)
	require.Equal(t, ExitUsage, r.code)
	r = runRoot(t, append(base, "status", "--user", "--system")...)
	require.Equal(t, ExitUsage, r.code)
	r = runRoot(t, append(base, "status", "job-1", "--user")...)
	require.Equal(t, ExitUsage, r.code)

	// Human user status (exact line).
	r = runRoot(t, append(base, "status", "--user")...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Equal(t, "Сессия SPN2: доступно 3 · выполняется 1", strings.TrimRight(r.out, "\n"))

	// System status reports the fake's ok state in Russian (exact line,
	// glyph included — Successf renders «✓ …»).
	r = runRoot(t, append(base, "status", "--system")...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Equal(t, "✓ Служба SPN2: работает", strings.TrimRight(r.out, "\n"))

	// A job id renders its status; the fake answers success.
	r = runRoot(t, append(base, "status", "job-1")...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Contains(t, r.out, "job-1")

	// JSON job status: exactly one object.
	r = runRoot(t, append(base, "--json", "status", "job-1")...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	lines := nonEmptyLines(r.out)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], `"job_id":"job-1"`)
}

// TestStatusErrorNoSnapshotLine: a failed job with a leftover timestamp
// never advertises a snapshot — the Снимок line is gated on success.
func TestStatusErrorNoSnapshotLine(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setStatus("job-9", `{"status":"error","job_id":"job-9","original_url":"https://example.com/e",`+
		`"timestamp":"20260101010101","status_ext":"error:blocked-url","message":"URL в блок-листе Wayback"}`)
	cfg := writeTestConfig(t, testConfigFull)

	r := runRoot(t, "--config", cfg, "status", "job-9")
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Contains(t, r.errOut, "Ошибка (error:blocked-url)", "the error line rides stderr")
	require.NotContains(t, r.out, "Снимок", "a failed job must not advertise a snapshot")
	require.NotContains(t, r.errOut, "Снимок")
}
