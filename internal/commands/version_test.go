// The version surface of the root command: the -v/--version flag, the help
// flag, and the `version` subcommand. The version line is a machine contract
// (the CI workflow greps it from a built binary), so the exact bytes are
// pinned here.
package commands

import (
	"fmt"
	"testing"

	"github.com/4q4r/wauploader/internal/version"
	"github.com/stretchr/testify/require"
)

// TestVersionFlagPrintsTheVersionLine: -v and --version on the root command
// exit 0 and print exactly the one-line version banner.
func TestVersionFlagPrintsTheVersionLine(t *testing.T) {
	want := fmt.Sprintf("%s version %s\n", version.Name, version.Version)

	for _, flag := range []string{"-v", "--version"} {
		res := runRoot(t, flag)
		require.Equal(t, ExitOK, res.code, "flag=%s err=%v errOut=%q", flag, res.err, res.errOut)
		require.Equal(t, want, res.out, "flag=%s must print the version line", flag)
		require.Equal(t, 1, len(nonEmptyLines(res.out)), "flag=%s must print one line", flag)
		require.NotContains(t, res.out, "Usage:", "flag=%s must not print the usage text", flag)
	}
}

// TestHelpFlagPrintsUsage: -h and --help exit 0 and print the usage text.
// The root command has no Run of its own, so the help output is the whole
// documented surface of the binary.
func TestHelpFlagPrintsUsage(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		res := runRoot(t, flag)
		require.Equal(t, ExitOK, res.code, "flag=%s err=%v errOut=%q", flag, res.err, res.errOut)
		require.Contains(t, res.out, "Usage:", "flag=%s must print the usage text", flag)
		require.Contains(t, res.out, version.Name, "flag=%s must name the tool", flag)
		require.Contains(t, res.out, "--version", "flag=%s must advertise the version flag", flag)
	}
}

// TestVersionSubcommandStillWorks: the `version` subcommand keeps its
// one-line output and its exit code.
func TestVersionSubcommandStillWorks(t *testing.T) {
	res := runRoot(t, "version")
	require.Equal(t, ExitOK, res.code, "err=%v errOut=%q", res.err, res.errOut)
	require.Equal(t, fmt.Sprintf("%s version %s\n", version.Name, version.Version), res.out)
}
