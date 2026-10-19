// save.go implements the `wauploader save` command: it maps the CLI flags
// onto spn2.Options 1:1, wires the journal and limits from configuration,
// and drives the Task 10 save runner. The flag surface mirrors the plan
// verbatim; --dry-run resolves targets and journal logic without touching
// the network or the journal file.

package commands

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/journal"
	"github.com/4q4r/wauploader/internal/runner"
	"github.com/4q4r/wauploader/internal/spn2"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
)

// saveFlags holds the parsed save-command flags. Sentinel values mark
// unset flags: "" for strings, -1 for js-behavior-timeout, false for the
// booleans (the spn2 boolean options have no true config defaults, so
// false is a safe "unset" marker).
type saveFlags struct {
	list                 string
	force                bool
	captureAll           bool
	captureOutlinks      bool
	captureScreenshot    bool
	delayWBAvailability  bool
	forceGet             bool
	skipFirstArchive     bool
	outlinksAvailability bool
	emailResult          bool
	ifNotArchivedWithin  string
	jsBehaviorTimeout    int
	captureCookie        string
	useUserAgent         string
	targetUsername       string
	targetPassword       string
}

// saveOptionsFrom merges configuration and flags into spn2.Options 1:1:
// every flag flips exactly one Options field; an unset flag keeps the
// configuration value. The target-site credentials exist only as flags and
// never enter events, logs, or errors.
func saveOptionsFrom(cfg *config.Config, f *saveFlags) spn2.Options {
	o := spn2.Options{
		// The spn2 boolean options have no true config defaults, so
		// OR-ing the flag over the config value implements "flag wins"
		// exactly: a provided true wins, a provided false equals the
		// all-false config default.
		CaptureAll:           cfg.SPN2.CaptureAll || f.captureAll,
		CaptureOutlinks:      cfg.SPN2.CaptureOutlinks || f.captureOutlinks,
		CaptureScreenshot:    cfg.SPN2.CaptureScreenshot || f.captureScreenshot,
		DelayWBAvailability:  cfg.SPN2.DelayWBAvailability || f.delayWBAvailability,
		ForceGet:             cfg.SPN2.ForceGet || f.forceGet,
		SkipFirstArchive:     cfg.SPN2.SkipFirstArchive || f.skipFirstArchive,
		OutlinksAvailability: cfg.SPN2.OutlinksAvailability || f.outlinksAvailability,
		EmailResult:          cfg.SPN2.EmailResult || f.emailResult,
		IfNotArchivedWithin:  cfg.SPN2.IfNotArchivedWithin,
		JSBehaviorTimeout:    cfg.SPN2.JSBehaviorTimeout,
	}
	if f.ifNotArchivedWithin != "" {
		o.IfNotArchivedWithin = f.ifNotArchivedWithin
	}

	if f.jsBehaviorTimeout >= 0 {
		o.JSBehaviorTimeout = f.jsBehaviorTimeout
	}

	o.CaptureCookie = f.captureCookie
	o.UseUserAgent = f.useUserAgent
	o.TargetUsername = f.targetUsername
	o.TargetPassword = f.targetPassword

	return o
}

// validateSaveOptions re-checks the two flag-overridable fields against the
// merged values (config.Validate covers the config file only). Violations
// are usage errors (exit 2).
func validateSaveOptions(opts spn2.Options) error {
	if opts.IfNotArchivedWithin != "" {
		if _, err := config.ParseIATimedelta(opts.IfNotArchivedWithin); err != nil {
			return exitUsage("недопустимое значение --if-not-archived-within: %v", err)
		}
	}

	if opts.JSBehaviorTimeout < 0 || opts.JSBehaviorTimeout > 30 {
		return exitUsage("--js-behavior-timeout должен быть от 0 до 30 секунд, сейчас %d", opts.JSBehaviorTimeout)
	}

	return nil
}

// NewSaveCommand builds the `save` subcommand. The pointers reference the
// caller's persistent flag values.
func NewSaveCommand(configPath, journalPath *string, jsonOut, noColor, dryRun *bool) *cobra.Command {
	f := &saveFlags{jsBehaviorTimeout: -1}
	cmd := &cobra.Command{
		Use:          "save (URL... | --list FILE)",
		Short:        "Сохранить страницы в web.archive.org (Save Page Now 2)",
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSave(cmd, args, f,
				derefString(configPath, "~/.config/wauploader/wauploader.toml"),
				derefString(journalPath, ""), derefBool(jsonOut), derefBool(noColor), derefBool(dryRun))
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.list, "list", "",
		"файл списка целей: по одной на строку, строки, начинающиеся с # — комментарии")
	fs.BoolVar(&f.force, "force", false,
		"повторно сохранять цели, уже отмеченные в журнале как выполненные (цели, отложенные по квоте, ставятся в очередь повторно при явном перечислении и без этого флага)")
	fs.BoolVar(&f.captureAll, "capture-all", false, "capture_all: сохранять всё содержимое страницы")
	fs.BoolVar(&f.captureOutlinks, "capture-outlinks", false, "capture_outlinks: сохранять найденные ссылки")
	fs.BoolVar(&f.captureScreenshot, "capture-screenshot", false, "capture_screenshot: снять скриншот страницы")
	fs.BoolVar(&f.delayWBAvailability, "delay-wb-availability", false, "delay_wb_availability: отложить появление снимка в Wayback")
	fs.BoolVar(&f.forceGet, "force-get", false, "force_get: запросить сохранение даже при наличии свежего снимка")
	fs.BoolVar(&f.skipFirstArchive, "skip-first-archive", false, "skip_first_archive: пропустить первый архивный запрос")
	fs.BoolVar(&f.outlinksAvailability, "outlinks-availability", false, "outlinks_availability: проверить доступность ссылок")
	fs.BoolVar(&f.emailResult, "email-result", false, "email_result: прислать результат письмом")
	fs.StringVar(&f.ifNotArchivedWithin, "if-not-archived-within", "",
		"дедупликация: не сохранять, если снимок есть за последние N (форма IA timedelta: \"120\" или \"3d 5h 20m\")")
	fs.IntVar(&f.jsBehaviorTimeout, "js-behavior-timeout", -1,
		"тайм-аут JS-поведения в секундах (0–30; 0 отключает JS)")
	fs.StringVar(&f.captureCookie, "capture-cookie", "", "capture_cookie: куки для сайта-цели (секрет, нигде не выводится)")
	fs.StringVar(&f.useUserAgent, "use-user-agent", "", "use_user_agent: User-Agent для запроса к сайту-цели")
	fs.StringVar(&f.targetUsername, "target-username", "", "target_username: имя пользователя для сайта-цели")
	fs.StringVar(&f.targetPassword, "target-password", "", "target_password: пароль сайта-цели (секрет, нигде не выводится)")

	return cmd
}

// runSave drives one save batch. Order of checks: targets (usage) → config
// (usage) → merged options (usage) → credentials (auth) → dry-run or run.
func runSave(cmd *cobra.Command, args []string, f *saveFlags,
	cfgPath, journalFlag string, jsonOut, noColor, dryRun bool,
) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	con := console.New(out, errOut, writerIsTTY(out), noColor)

	targets, err := resolveTargets(args, expandHomePath(f.list))
	if err != nil {
		return exitUsage("%v", err)
	}

	cfg, err := loadConfig(expandHomePath(cfgPath), errOut)
	if err != nil {
		return err
	}

	if err := cfg.Validate(config.ModeSave); err != nil {
		return exitUsage("%v", err)
	}

	opts := saveOptionsFrom(cfg, f)
	if err := validateSaveOptions(opts); err != nil {
		return err
	}

	creds, err := auth.Resolve(cfg)
	if err != nil {
		return exitAuth(err)
	}

	if dryRun {
		return runSaveDry(con, journalFlag, targets, f.force)
	}

	jp, err := journalPathOr(journalFlag)
	if err != nil {
		return err
	}

	st, err := journal.Open(jp)
	if err != nil {
		return &ExitError{Code: ExitSomeFailed, Err: err}
	}
	defer st.Close() //nolint:errcheck // a read-only handle at this point

	// Limits from configuration. rate.Limit's unit is events per SECOND
	// despite the field name (Task 10 ledger note): the per-minute config
	// value converts with rate.Every(time.Minute / N). A value below 1 is
	// passed through as 0 so the runner's own validation reports it in
	// Russian instead of dividing by zero here.
	lim := runner.Limits{
		Concurrency: cfg.Limits.SaveConcurrency,
		PerHost:     cfg.Limits.PerHostConcurrency,
	}
	if cfg.Limits.SavePerMinute >= 1 {
		lim.PerMinute = rate.Every(time.Minute / time.Duration(cfg.Limits.SavePerMinute))
	}

	jobWait, err := config.ParseIATimedelta(cfg.Limits.JobWait)
	if err != nil {
		return exitUsage("недопустимое значение job_wait: %v", err)
	}

	lim.JobWait = jobWait

	sp := spn2.NewClient(newHTTPClient(), creds)
	r := runner.NewSaveRunner(sp, st, lim, runnerConsole(con, jsonOut))

	events, done := r.Run(cmd.Context(), targets, opts, f.force)

	var p eventPump
	p.pumpStart(events, con, jsonOut)

	summary, rerr := done()

	p.wait()

	if !jsonOut {
		saveSummaryLine(con, summary)
	}

	return finishRun(rerr, summary, p.authPaused.Load())
}

// runSaveDry implements the dry-run purity contract: resolve targets, apply
// the journal/--force logic, print every would-be action in Russian — and
// touch nothing. The journal file is only opened when it already exists;
// an absent file stays absent.
func runSaveDry(con *console.Console, journalFlag string, targets []string, force bool) error {
	jp, err := journalPathOr(journalFlag)
	if err != nil {
		return err
	}

	doneSet, cleanup, err := saveDoneTargets(jp, targets)
	if err != nil {
		return err
	}
	defer cleanup()

	skipped := 0

	for _, t := range targets {
		if doneSet[t] && !force {
			con.Dimf("Пропущено (уже сохранено; для повтора --force): %s", t)

			skipped++

			continue
		}

		con.Infof("Сохранить: %s", t)
	}

	con.Infof("Итог dry-run: будет выполнено %d · пропущено %d", len(targets)-skipped, skipped)

	if skipped > 0 {
		return &ExitError{Code: ExitSomeFailed, Err: nil}
	}

	return nil
}

// saveDoneTargets returns the subset of targets already journaled as done.
// When the journal file does not exist nothing is created — the dry-run
// purity contract is asserted by TestDryRunTouchesNothing.
func saveDoneTargets(journalPath string, targets []string) (map[string]bool, func(), error) {
	if _, err := os.Stat(journalPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]bool{}, func() {}, nil
		}

		return nil, nil, exitUsage("не удалось проверить файл журнала %s: %v", journalPath, err)
	}

	st, err := journal.Open(journalPath)
	if err != nil {
		return nil, nil, &ExitError{Code: ExitSomeFailed, Err: err}
	}

	done := make(map[string]bool, len(targets))
	for _, t := range targets {
		rec, ok, err := st.Get(journal.KindSave, t)
		if err != nil {
			_ = st.Close()
			return nil, nil, &ExitError{Code: ExitSomeFailed, Err: err}
		}

		if ok && rec.State == journal.StateDone {
			done[t] = true
		}
	}

	return done, func() { _ = st.Close() }, nil
}
