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

	// Human user status.
	r = runRoot(t, append(base, "status", "--user")...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Contains(t, r.out, "3")
	require.Contains(t, r.out, "1")

	// System status reports the fake's ok state in Russian.
	r = runRoot(t, append(base, "status", "--system")...)
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.True(t, strings.Contains(r.out, "работает") || strings.Contains(r.out, "ok"),
		"system line must report the state, got: %q", r.out)

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
