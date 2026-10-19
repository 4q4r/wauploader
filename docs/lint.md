# golangci-lint: pinned version and the toolchain pairing landmine

## Pinned version

- **golangci-lint v2.14.0** (released 2026-09-24; go1.27 support landed in v2.13.0)
- **govulncheck v1.7.0**

Current install on the dev machine: `~/.local/bin/golangci-lint`, reported as:

```
golangci-lint has version 2.14.0 built with go1.27.2-... linux/amd64
```

## The landmine: binary must match the Go toolchain

A golangci-lint binary embeds the Go toolchain it was built with. If the repo's
Go toolchain is NEWER than the toolchain inside the binary, the linter fails
with typecheck errors that look like phantom compile errors
(`could not import ...`, `-: could not load export data`) — the binary simply
cannot read modules compiled for a newer Go. Never mix, say, a golangci-lint
built with go1.26 against a repo on go1.27.

Rule: **build/install golangci-lint with the same Go version the module
declares** (`go 1.27` in go.mod), or use the official binary release built for
that Go.

## Reinstall

```sh
# with the repo's own toolchain (recommended):
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

# or the official install script, pinned:
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
  | sh -s -- -b "$(go env GOPATH)/bin" v2.14.0

golangci-lint version   # must print v2.14.0 built with go1.27.x
```

`govulncheck` follows the same pairing rule:

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.7.0
```

## Config notes (`.golangci.yml`)

- `issues.max-issues-per-linter: 0` and `max-same-issues: 0` are load-bearing:
  the defaults (50 / 3) silently hide findings — with the defaults the first
  run reported 100 issues where there were ~900.
- The plan named linter `wsl`; it is deprecated since v2.2.0 and replaced by
  **wsl_v5** (golangci-lint's own migration path). wsl_v5 is enabled with the
  settings golangci-lint itself suggests (`allow-first-in-block: true`,
  `allow-whole-block: false`, `branch-max-lines: 2`).
- `funlen` pins one number (200) per the plan; both axes (`lines`,
  `statements`) follow it so the linter measures what the plan specified.
- `varnamelen` carries a type-aware `ignore-decls` allowlist of idiomatic
  single-letter Go names (`io.Reader` n, `strings.Builder` b, `errgroup` g,
  bbolt bucket b, and so on). Any short name with another type, or used
  beyond `max-distance`, still reports.
- Test files are excluded from analysis (`run.tests: false`).
