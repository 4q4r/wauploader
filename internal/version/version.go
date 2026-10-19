// Package version carries the wauploader identity used in CLI output,
// build-time stamping, and HTTP User-Agent headers.
package version

import "fmt"

// Name is the canonical CLI and module display name.
const Name = "wauploader"

// Version is the release version. It is overridden at build time with
// -ldflags "-X github.com/4q4r/wauploader/internal/version.Version=x.y.z".
var Version = "dev"

// UserAgent returns the HTTP User-Agent used by all wauploader HTTP clients.
func UserAgent() string {
	return fmt.Sprintf("%s/%s (+https://github.com/4q4r/wauploader)", Name, Version)
}
