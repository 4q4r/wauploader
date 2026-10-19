// Command wauploader saves pages to web.archive.org (Save Page Now 2) and
// uploads files to archive.org items (IA-S3).
package main

import (
	"fmt"
	"os"

	"github.com/4q4r/wauploader/internal/version"
	"github.com/spf13/cobra"
)

// Global persistent flag values. Declared here so the CLI surface exists;
// configuration and console layers (later tasks) consume them.
var (
	configPath string
	jsonOutput bool
	noColor    bool
)

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
	root.PersistentFlags().StringVar(&configPath, "config",
		"~/.config/wauploader/wauploader.toml", "path to the configuration file")
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false,
		"emit machine-readable JSON output")
	root.PersistentFlags().BoolVar(&noColor, "no-color", false,
		"disable colored terminal output")

	root.AddCommand(newVersionCommand())
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the wauploader version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Printf("%s version %s\n", version.Name, version.Version)
			return nil
		},
	}
}
