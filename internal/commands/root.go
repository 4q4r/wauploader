// Package commands hosts the wauploader subcommands (init, save, upload,
// status, jobs). This file carries the shared glue: the exit-code contract,
// configuration loading with the unknown-key warning (Task 2 ruling),
// target-list parsing, and the event pump that bridges the unbuffered
// runner event stream to the console and --json channels.
//
// Event.Target vocabulary (Task 10/11 ledger carry):
//
//   - save events carry the bare URL;
//   - upload file events carry the journal composite "path\x00id/key";
//     the human console shows the display form "path → id/key" (the
//     item/key part), JSON keeps the raw composite with the NUL byte;
//   - upload derive-watch events carry the bare item identifier;
//   - auth events carry an empty Target (the pause is run-wide).
//
// Exit codes (docs/plan.md Task 12): 0 all ok, 1 some jobs failed or
// parked, 2 config/usage error, 3 auth error.
package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/4q4r/wauploader/internal/runner"
	"github.com/4q4r/wauploader/internal/version"
	"github.com/spf13/cobra"
)

// Process exit codes, docs/plan.md Task 12.
const (
	ExitOK         = 0 // everything succeeded
	ExitSomeFailed = 1 // some jobs failed or were parked (or interrupted)
	ExitUsage      = 2 // configuration or usage error
	ExitAuth       = 3 // archive.org authentication error
)

// ExitError carries a process exit code alongside the Russian error text.
// Commands return it from RunE; main maps it to os.Exit(Code) and prints
// Err (when set) through the console so cobra never emits its English
// "Error:" prefix. A nil Err means the failure was already reported
// per-event (summary-based exits) and nothing more should print.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// exitUsage wraps a configuration/usage failure (exit code 2).
func exitUsage(format string, a ...any) *ExitError {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, a...)}
}

// exitAuth wraps an authentication failure (exit code 3).
func exitAuth(err error) *ExitError { return &ExitError{Code: ExitAuth, Err: err} }

// newHTTPClient builds the shared HTTP client for one command run. It is a
// package variable so tests can repoint every host at httptest fakes (the
// same seam pattern as the init command's bootstrapFn).
var newHTTPClient = func() *httpx.Client { return httpx.NewClient(nil, "") }

// defaultJournalPath returns the XDG data path of the journal file:
// $XDG_DATA_HOME/wauploader/journal.bbolt, else
// ~/.local/share/wauploader/journal.bbolt. An empty XDG_DATA_HOME value is
// treated as unset per the XDG base-directory specification.
func defaultJournalPath() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wauploader", "journal.bbolt"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("не удалось определить домашний каталог для файла журнала: %w", err)
	}
	return filepath.Join(home, ".local", "share", "wauploader", "journal.bbolt"), nil
}

// journalPathOr resolves the effective journal path: the explicit --journal
// flag, else the XDG default. A tilde is expanded; resolution failures are
// usage errors (exit 2).
func journalPathOr(flagValue string) (string, error) {
	p := flagValue
	if p == "" {
		def, err := defaultJournalPath()
		if err != nil {
			return "", exitUsage("%v", err)
		}
		p = def
	}
	return expandHomePath(p), nil
}

// loadConfig loads the TOML configuration and emits one Russian warning
// line per unknown key to warn (Task 2 ruling carried into Task 12: a
// typo'd key must be named, not silently ignored). warn must be the
// command's stderr so --json stdout stays byte-pure. A load failure is a
// usage error (exit 2).
func loadConfig(path string, warn io.Writer) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, exitUsage("%v", err)
	}
	unknown, err := config.DetectUnknownKeys(path)
	if err != nil {
		return nil, exitUsage("%v", err)
	}
	for _, key := range unknown {
		fmt.Fprintf(warn, "? Неизвестный ключ конфигурации %q — проверьте написание в файле %s\n", key, path)
	}
	return cfg, nil
}

// resolveTargets merges positional targets with --list FILE (mutually
// exclusive; at least one source required). Violations are usage errors.
func resolveTargets(args []string, listPath string) ([]string, error) {
	switch {
	case len(args) > 0 && listPath != "":
		return nil, errors.New("нельзя одновременно перечислять цели в аргументах и в файле --list")
	case len(args) > 0:
		return args, nil
	case listPath != "":
		targets, err := parseTargetList(expandHomePath(listPath))
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("файл списка %s не содержит ни одной цели", listPath)
		}
		return targets, nil
	default:
		return nil, errors.New("не указаны цели: перечислите их в аргументах команды или через --list FILE")
	}
}

// parseTargetList reads a --list file: one target per line, lines whose
// first non-space character is "#" are comments, blank lines are skipped,
// surrounding spaces are trimmed. An inline "#" is never special: URLs may
// legally carry fragments.
func parseTargetList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать файл списка %s: %w", path, err)
	}
	var targets []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		targets = append(targets, trimmed)
	}
	return targets, nil
}

// displayTarget renders a journal/Event target for humans: upload
// composites ("path\x00id/key") become "path → id/key"; URLs, item ids and
// empty auth-event targets pass through unchanged.
func displayTarget(target string) string {
	if i := strings.IndexByte(target, 0); i >= 0 {
		return target[:i] + " → " + target[i+1:]
	}
	return target
}

// eventJSON is the machine-readable shape of one runner event (--json
// channel: exactly one compact object per event, no colors, no Russian
// decorations). Secret values (target site credentials, capture cookies)
// never ride events: the runner does not put them into Event fields.
type eventJSON struct {
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	State       string `json:"state,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	SnapshotURL string `json:"snapshot_url,omitempty"`
	ErrMsg      string `json:"error,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	Sent        int64  `json:"sent,omitempty"`
	Total       int64  `json:"total,omitempty"`
}

// eventToJSON maps a runner event onto its JSON shape 1:1.
func eventToJSON(ev runner.Event) eventJSON {
	return eventJSON{
		Kind:        ev.Kind,
		Target:      ev.Target,
		State:       string(ev.State),
		JobID:       ev.JobID,
		SnapshotURL: ev.SnapshotURL,
		ErrMsg:      ev.ErrMsg,
		Attempt:     ev.Attempt,
		Sent:        ev.Sent,
		Total:       ev.Total,
	}
}

// eventPump drains the runner's UNBUFFERED event stream concurrently (the
// completion func must only be called once the pump is running — the
// Task 10/11 contract). In --json mode every event becomes one JSON object
// on out. In human mode the runners print their own status lines through
// the shared console, so the pump renders only what they do not: the
// throttled upload progress events and the auth-pause flag used for exit
// classification.
type eventPump struct {
	authPaused atomic.Bool
	wg         sync.WaitGroup
}

// pumpStart starts the drain goroutine. Call before the completion func.
func (p *eventPump) pumpStart(events <-chan runner.Event, con *console.Console, jsonMode bool) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for ev := range events {
			if ev.Kind == runner.EventAuth {
				p.authPaused.Store(true)
			}
			if jsonMode {
				con.JSON(eventToJSON(ev))
				continue
			}
			if ev.Kind == runner.EventProgress {
				p.renderProgress(con, ev)
			}
		}
	}()
}

// renderProgress prints one throttled progress line (2%-milestone events
// emitted by the upload runner).
func (p *eventPump) renderProgress(con *console.Console, ev runner.Event) {
	pct := 100
	if ev.Total > 0 {
		pct = int(float64(ev.Sent) / float64(ev.Total) * 100)
	}
	con.Dimf("Загрузка: %s — %d%% (%s из %s)",
		displayTarget(ev.Target), pct, console.FormatBytesRU(ev.Sent), console.FormatBytesRU(ev.Total))
}

// wait blocks until the stream is fully drained (the runner closed it).
func (p *eventPump) wait() { p.wg.Wait() }

// finishRun classifies a finished runner batch into the exit-code contract:
// a fatal runner error is a usage/config error (limits come from config)
// unless it is the auth pause (3) or an interruption (1); an otherwise
// clean run with failed or parked jobs is exit 1.
func finishRun(rerr error, summary runner.Summary, authPaused bool) error {
	if rerr != nil {
		if errors.Is(rerr, context.Canceled) {
			return &ExitError{Code: ExitSomeFailed, Err: errors.New("выполнение прервано: часть задач осталась в журнале и продолжится при следующем запуске")}
		}
		if authPaused {
			return &ExitError{Code: ExitAuth, Err: rerr}
		}
		return &ExitError{Code: ExitUsage, Err: rerr}
	}
	if authPaused {
		return exitAuth(errors.New("ошибка авторизации archive.org: выполнение очереди приостановлено — проверьте ключи или куки и запустите снова"))
	}
	if summary.Failed > 0 || summary.Parked > 0 {
		return &ExitError{Code: ExitSomeFailed, Err: nil}
	}
	return nil
}

// runnerConsole returns the console handed to the runner. In --json mode
// the runners must not print human lines: their console is a discard, the
// event pump owns the whole out stream (one JSON object per event), and no
// two writers ever touch the same buffer concurrently.
func runnerConsole(con *console.Console, jsonMode bool) *console.Console {
	if jsonMode {
		return console.New(io.Discard, io.Discard, false, true)
	}
	return con
}

// saveSummaryLine prints the final human summary (plan wording).
func saveSummaryLine(con *console.Console, s runner.Summary) {
	con.Infof("Итог: сохранено %d · пропущено %d · отложено %d · ошибок %d", s.Done, s.Skipped, s.Parked, s.Failed)
}

// uploadSummaryLine prints the final human summary for uploads (Parked is
// structurally 0 for uploads — the Task 11 summary contract).
func uploadSummaryLine(con *console.Console, s runner.Summary) {
	con.Infof("Итог: загружено %d · пропущено %d · отложено %d · ошибок %d", s.Done, s.Skipped, s.Parked, s.Failed)
}

// NewRootCommand wires the whole CLI: global persistent flags, then every
// subcommand. Pointers reference the caller's flag values (the version and
// init commands share the pattern); main maps returned ExitError values to
// process exit codes.
func NewRootCommand(configPath, journalPath *string, jsonOut, noColor, dryRun *bool) *cobra.Command {
	root := &cobra.Command{
		Use:           version.Name,
		Short:         "Сохранение страниц в web.archive.org и загрузка файлов в элементы archive.org",
		SilenceUsage:  true, // Russian guidance replaces the usage dump
		SilenceErrors: true, // errors print through the console in Russian (main)
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true, // not part of the documented CLI surface (Task 1 M2 carry)
		},
	}
	root.PersistentFlags().StringVar(configPath, "config",
		"~/.config/wauploader/wauploader.toml", "путь к файлу конфигурации")
	root.PersistentFlags().StringVar(journalPath, "journal",
		"", "путь к файлу журнала (по умолчанию ~/.local/share/wauploader/journal.bbolt, учитывает XDG_DATA_HOME)")
	root.PersistentFlags().BoolVar(jsonOut, "json", false,
		"машинный вывод: один JSON-объект на событие или результат")
	root.PersistentFlags().BoolVar(noColor, "no-color", false,
		"отключить цветной вывод терминала")
	root.PersistentFlags().BoolVar(dryRun, "dry-run", false,
		"показать действия без выполнения: сеть и журнал не затрагиваются (save, upload; вывод в человекочитаемом виде; --json на dry-run не действует)")

	root.AddCommand(version.NewVersionCommand(jsonOut))
	root.AddCommand(NewInitCommand(configPath, jsonOut, noColor))
	root.AddCommand(NewSaveCommand(configPath, journalPath, jsonOut, noColor, dryRun))
	root.AddCommand(NewUploadCommand(configPath, journalPath, jsonOut, noColor, dryRun))
	root.AddCommand(NewStatusCommand(configPath, jsonOut, noColor))
	root.AddCommand(NewJobsCommand(journalPath, noColor))
	return root
}

// writerIsTTY reports whether w is the process terminal (color gate input
// for out-bound consoles; buffers in tests are never TTYs).
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && console.IsTTY(f)
}
