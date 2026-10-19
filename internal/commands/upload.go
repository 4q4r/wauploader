// upload.go implements the `wauploader upload` command: target resolution
// (files or --list, --item or --identifier-from template), the required
// collection/mediatype fail-fast (owner decision: no defaults), the
// metadata header surface, and the Task 11 upload runner wiring.
package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/ias3"
	"github.com/4q4r/wauploader/internal/journal"
	"github.com/4q4r/wauploader/internal/runner"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
)

// uploadFlags holds the parsed upload-command flags.
type uploadFlags struct {
	list                    string
	item                    string
	key                     string
	collection              string
	mediatype               string
	meta                    []string
	title                   string
	description             string
	noDerive                bool
	noKeepOldVersion        bool
	interactivePriority     bool
	ignorePreexistingBucket bool
	verify                  bool
	waitDerive              bool
	checksumSkip            bool
	identifierFrom          string
}

// parseMetaFlags parses repeatable --meta NAME=VALUE pairs into the Extra
// metadata map (repeated names accumulate values).
func parseMetaFlags(meta []string) (ias3.Meta, error) {
	m := ias3.Meta{}
	for _, pair := range meta {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, errors.New("неверный формат --meta " + pair + ": ожидается ИМЯ=ЗНАЧЕНИЕ")
		}
		m[name] = append(m[name], value)
	}
	return m, nil
}

// uploadTargetsFrom resolves the upload targets: absolute paths, the
// identifier from --item or rendered per file from --identifier-from
// (text/template over {{.Base}}), the key from --key or the base name.
// Every violation is a usage error (exit 2) via the caller's wrap.
func uploadTargetsFrom(paths []string, f *uploadFlags) ([]runner.UploadTarget, error) {
	if f.item != "" && f.identifierFrom != "" {
		return nil, errors.New("нельзя использовать одновременно --item и --identifier-from")
	}
	if f.item == "" && f.identifierFrom == "" {
		return nil, errors.New("не указан идентификатор элемента: задайте --item ID или --identifier-from ШАБЛОН (например \"custom-{{.Base}}\")")
	}
	if f.key != "" && f.identifierFrom != "" {
		return nil, errors.New("--key допустим только вместе с --item")
	}
	var tmpl *template.Template
	if f.identifierFrom != "" {
		parsed, err := template.New("identifier").Parse(f.identifierFrom)
		if err != nil {
			return nil, errors.New("неверный шаблон --identifier-from: " + err.Error())
		}
		tmpl = parsed
	}

	targets := make([]runner.UploadTarget, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, errors.New("не удалось определить путь " + p + ": " + err.Error())
		}
		id := f.item
		if tmpl != nil {
			var b strings.Builder
			data := struct{ Base string }{Base: filepath.Base(abs)}
			if err := tmpl.Execute(&b, data); err != nil {
				return nil, errors.New("не удалось вычислить идентификатор для " + p + ": " + err.Error())
			}
			id = b.String()
		}
		if err := ias3.ValidateIdentifier(id); err != nil {
			return nil, errors.New("файл " + p + ": " + err.Error())
		}
		key := f.key
		if key == "" {
			key = filepath.Base(abs)
		}
		targets = append(targets, runner.UploadTarget{Path: abs, Identifier: id, Key: key})
	}
	return targets, nil
}

// headerOptsFrom builds the IA-S3 header surface from configuration and
// flags. Collection/mediatype come from configuration (already merged with
// flags and validated); the control booleans combine the configuration
// defaults with the flag overrides; --meta pairs land in Extra.
//
// Derive wiring (Task 11 report, binding): the runner's derive watch is
// armed by a PUT actually carrying QueueDerive=true, so --no-derive turns
// both queueing and watching off, while --wait-derive forces queueing on
// (overriding a config queue_derive=false) and thereby the watch.
func headerOptsFrom(cfg *config.Config, f *uploadFlags) (ias3.HeaderOpts, error) {
	meta, err := parseMetaFlags(f.meta)
	if err != nil {
		return ias3.HeaderOpts{}, err
	}
	queueDerive := cfg.Upload.QueueDerive
	if f.noDerive {
		queueDerive = false
	}
	if f.waitDerive {
		queueDerive = true
	}
	keepOld := cfg.Upload.KeepOldVersion
	if f.noKeepOldVersion {
		keepOld = false
	}
	return ias3.HeaderOpts{
		Collection:              cfg.Upload.Collection,
		Mediatype:               cfg.Upload.Mediatype,
		Title:                   f.title,
		Description:             f.description,
		Extra:                   meta,
		QueueDerive:             queueDerive,
		KeepOldVersion:          keepOld,
		InteractivePriority:     cfg.Upload.InteractivePriority || f.interactivePriority,
		IgnorePreexistingBucket: f.ignorePreexistingBucket,
	}, nil
}

// NewUploadCommand builds the `upload` subcommand.
func NewUploadCommand(configPath, journalPath *string, jsonOut, noColor, dryRun *bool) *cobra.Command {
	f := &uploadFlags{}
	cmd := &cobra.Command{
		Use:          "upload (FILE... | --list FILE) --item ID [--key NAME]",
		Short:        "Загрузить файлы в элемент archive.org (IA-S3)",
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpload(cmd, args, f,
				derefString(configPath, "~/.config/wauploader/wauploader.toml"),
				derefString(journalPath, ""), derefBool(jsonOut), derefBool(noColor), derefBool(dryRun))
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.list, "list", "",
		"файл списка файлов: по одному пути на строку, строки, начинающиеся с # — комментарии")
	fs.StringVar(&f.item, "item", "", "идентификатор элемента archive.org (взаимоисключим с --identifier-from)")
	fs.StringVar(&f.key, "key", "", "имя файла внутри элемента (по умолчанию — имя файла; только вместе с --item)")
	fs.StringVar(&f.collection, "collection", "", "коллекция элемента (обязательна, если не задана в конфигурации)")
	fs.StringVar(&f.mediatype, "mediatype", "", "тип содержимого: texts, etree, audio, movies, software, image, data или web")
	fs.StringArrayVar(&f.meta, "meta", nil, "метаданные элемента, ИМЯ=ЗНАЧЕНИЕ (повторяемый флаг)")
	fs.StringVar(&f.title, "title", "", "название элемента")
	fs.StringVar(&f.description, "description", "", "описание элемента")
	fs.BoolVar(&f.noDerive, "no-derive", false, "не ставить деривацию в очередь")
	fs.BoolVar(&f.noKeepOldVersion, "no-keep-old-version", false, "не сохранять прежние версии перезаписываемых файлов")
	fs.BoolVar(&f.interactivePriority, "interactive-priority", false, "приоритетная очередь деривации")
	fs.BoolVar(&f.ignorePreexistingBucket, "ignore-preexisting-bucket", false, "не проверять существование элемента перед созданием")
	fs.BoolVar(&f.verify, "verify", false, "проверить MD5 загруженных файлов после загрузки")
	fs.BoolVar(&f.waitDerive, "wait-derive", false, "дождаться завершения деривации (включает постановку деривации в очередь)")
	fs.BoolVar(&f.checksumSkip, "checksum-skip", false, "не пропускать файлы, уже присутствующие в элементе с тем же MD5")
	fs.StringVar(&f.identifierFrom, "identifier-from", "",
		"шаблон идентификатора для каждого файла, например \"custom-{{.Base}}\" (взаимоисключим с --item)")
	return cmd
}

// runUpload drives one upload batch. Order of checks: targets and
// identifier contract (usage) → config (usage) → credentials (auth) →
// dry-run or run.
func runUpload(cmd *cobra.Command, args []string, f *uploadFlags,
	cfgPath, journalFlag string, jsonOut, noColor, dryRun bool,
) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	con := console.New(out, errOut, writerIsTTY(out), noColor)

	rawPaths, err := resolveTargets(args, expandHomePath(f.list))
	if err != nil {
		return exitUsage("%v", err)
	}
	targets, err := uploadTargetsFrom(rawPaths, f)
	if err != nil {
		return exitUsage("%v", err)
	}
	if f.noDerive && f.waitDerive {
		return exitUsage("--wait-derive требует очереди деривации — уберите --no-derive")
	}
	cfg, err := loadConfig(expandHomePath(cfgPath), errOut)
	if err != nil {
		return err
	}
	cfg.MergeFlag(func(c *config.Config) {
		if f.collection != "" {
			c.Upload.Collection = f.collection
		}
		if f.mediatype != "" {
			c.Upload.Mediatype = f.mediatype
		}
	})
	if err := cfg.Validate(config.ModeUpload); err != nil {
		return exitUsage("%v", err)
	}
	creds, err := auth.Resolve(cfg)
	if err != nil {
		return exitAuth(err)
	}
	// IA-S3 never accepts cookies (auth.Resolve's doc delegates this to
	// upload commands): enforce the key pair before any hashing or network
	// work so a cookies-only configuration fails fast instead of
	// 403-ing after hashing a 50 GB file.
	if creds.Access == "" || creds.Secret == "" {
		return exitAuth(errors.New("загрузка в archive.org выполняется ключами S3: заполните s3_access и s3_secret в файле конфигурации (получить: https://archive.org/account/s3.php) или выполните wauploader init"))
	}
	opts, err := headerOptsFrom(cfg, f)
	if err != nil {
		return exitUsage("%v", err)
	}

	if dryRun {
		return runUploadDry(con, journalFlag, targets)
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

	// Limits from configuration. The IA-S3 politeness surface is the
	// check_limit preflight (there is no documented per-minute upload
	// cap), so PerMinute stays rate.Inf — a no-op admission gate (Task 11
	// contract). PerHost stays validated but unused (constant host).
	jobWait, err := config.ParseIATimedelta(cfg.Limits.JobWait)
	if err != nil {
		return exitUsage("недопустимое значение job_wait: %v", err)
	}
	lim := runner.Limits{
		PerMinute:   rate.Inf,
		Concurrency: cfg.Limits.UploadConcurrency,
		PerHost:     cfg.Limits.PerHostConcurrency,
		JobWait:     jobWait,
	}

	cl := ias3.NewClient(newHTTPClient(), creds)
	r := runner.NewUploadRunner(cl, st, lim, runnerConsole(con, jsonOut))

	events, done := r.Run(cmd.Context(), targets, opts, f.verify, !f.checksumSkip)
	var p eventPump
	p.pumpStart(events, con, jsonOut)
	summary, rerr := done()
	p.wait()

	if !jsonOut {
		uploadSummaryLine(con, summary)
	}
	return finishRun(rerr, summary, p.authPaused.Load())
}

// runUploadDry resolves targets and journal skips without touching the
// network or creating the journal file.
func runUploadDry(con *console.Console, journalFlag string, targets []runner.UploadTarget) error {
	jp, err := journalPathOr(journalFlag)
	if err != nil {
		return err
	}
	doneSet, cleanup, err := uploadDoneTargets(jp, targets)
	if err != nil {
		return err
	}
	defer cleanup()

	skipped := 0
	for _, t := range targets {
		composite := t.Path + "\x00" + t.Identifier + "/" + t.Key
		if doneSet[composite] {
			con.Dimf("Пропущено (уже в журнале): %s → %s/%s", t.Path, t.Identifier, t.Key)
			skipped++
			continue
		}
		con.Infof("Загрузить: %s → %s/%s", t.Path, t.Identifier, t.Key)
	}
	con.Infof("Итог dry-run: будет выполнено %d · пропущено %d", len(targets)-skipped, skipped)
	if skipped > 0 {
		return &ExitError{Code: ExitSomeFailed, Err: nil}
	}
	return nil
}

// uploadDoneTargets mirrors saveDoneTargets for upload composites.
func uploadDoneTargets(journalPath string, targets []runner.UploadTarget) (map[string]bool, func(), error) {
	if _, err := os.Stat(journalPath); err != nil {
		if os.IsNotExist(err) {
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
		composite := t.Path + "\x00" + t.Identifier + "/" + t.Key
		rec, ok, err := st.Get(journal.KindUpload, composite)
		if err != nil {
			_ = st.Close()
			return nil, nil, &ExitError{Code: ExitSomeFailed, Err: err}
		}
		if ok && rec.State == journal.StateDone {
			done[composite] = true
		}
	}
	return done, func() { _ = st.Close() }, nil
}
