// Command wauploader saves pages to web.archive.org (Save Page Now 2) and
// uploads files to archive.org items (IA-S3).
package main

import (
	"errors"
	"os"

	"github.com/4q4r/wauploader/internal/commands"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/spf13/cobra"
)

// cliFlags carries the global persistent flag values. Grouped in a struct
// so command constructors take explicit state.
type cliFlags struct {
	configPath  string
	journalPath string
	jsonOutput  bool
	noColor     bool
	dryRun      bool
}

var flags cliFlags

func main() {
	if err := newRootCommand().Execute(); err != nil {
		var ee *commands.ExitError
		if errors.As(err, &ee) {
			// A nil Err means the failure was already reported per-event
			// (summary-based exits); otherwise print the Russian text.
			if ee.Err != nil {
				printCLIError(ee.Err)
			}
			os.Exit(ee.Code)
		}
		printCLIError(err)
		os.Exit(1)
	}
}

// printCLIError reports a failed command through the console (glyphed,
// color-gated) so cobra's English "Error:" prefix never reaches the user.
func printCLIError(err error) {
	con := console.New(os.Stderr, os.Stderr, console.IsTTY(os.Stderr), flags.noColor)
	con.Errorf("%v", err)
}

func newRootCommand() *cobra.Command {
	return commands.NewRootCommand(&flags.configPath, &flags.journalPath,
		&flags.jsonOutput, &flags.noColor, &flags.dryRun)
}
