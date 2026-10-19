// Command wauploader saves pages to web.archive.org (Save Page Now 2) and
// uploads files to archive.org items (IA-S3).
package main

import (
	"os"

	"github.com/4q4r/wauploader/internal/commands"
	"github.com/4q4r/wauploader/internal/version"
	"github.com/spf13/cobra"
)

// cliFlags carries the global persistent flag values. Grouped in a struct so
// command constructors take explicit state; configuration and console layers
// (later tasks) consume it.
type cliFlags struct {
	configPath string
	jsonOutput bool
	noColor    bool
}

var flags cliFlags

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   version.Name,
		Short: "Save pages to web.archive.org and upload files to archive.org items",
	}
	root.PersistentFlags().StringVar(&flags.configPath, "config",
		"~/.config/wauploader/wauploader.toml", "path to the configuration file")
	root.PersistentFlags().BoolVar(&flags.jsonOutput, "json", false,
		"emit machine-readable JSON output")
	root.PersistentFlags().BoolVar(&flags.noColor, "no-color", false,
		"disable colored terminal output")

	root.AddCommand(version.NewVersionCommand(&flags.jsonOutput))
	root.AddCommand(commands.NewInitCommand(&flags.configPath, &flags.jsonOutput, &flags.noColor))
	return root
}
