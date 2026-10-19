// Package version carries the wauploader identity used in CLI output,
// build-time stamping, and HTTP User-Agent headers. It also owns the
// `version` subcommand so its output contracts stay unit-testable.
package version

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Name is the canonical CLI and module display name.
const Name = "wauploader"

// Version is the release version. It is overridden at build time with
// -ldflags "-X github.com/4q4r/wauploader/internal/version.Version=x.y.z".
var Version = "dev"

// UserAgent returns the HTTP User-Agent used by all wauploader HTTP clients.
func UserAgent() string {
	return fmt.Sprintf("%s/%s (+https://github.com/4q4r/wauploader)", Name, Version)
}

// versionPayload is the machine-readable shape printed by the version
// command when JSON output mode is on.
type versionPayload struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// NewVersionCommand builds the `version` subcommand. jsonOut points at the
// caller's --json flag value: when true the command prints a single compact
// JSON object, otherwise the plain text line. Output goes through the
// command's output writer so callers can capture it in tests.
func NewVersionCommand(jsonOut *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the wauploader version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if jsonOut != nil && *jsonOut {
				return json.NewEncoder(w).Encode(versionPayload{Name: Name, Version: Version})
			}
			_, err := fmt.Fprintf(w, "%s version %s\n", Name, Version)
			return err
		},
	}
}
