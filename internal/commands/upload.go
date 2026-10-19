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
	"github.com/4q4r/wauploader/internal/pdfmeta"
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
	autoMeta                bool
	identifierScheme        string
}

// schemeDirNNSlug is the only supported --identifier-scheme value: the
// <dir>-<NN>-<slug> scheme derived from the file's parent directory and
// the NN_author_topic_words.pdf filename convention (pdfmeta.Identifier).
const schemeDirNNSlug = "dir-nn-slug"

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

// uploadTargetsOrAutoMeta resolves the upload targets for the two
// identifier modes: the explicit --item/--identifier-from contract
// (uploadTargetsFrom) or --auto-meta (targets built later from the PDFs
// themselves — here only the flag contract is enforced). Every violation
// is a usage error (exit 2).
func uploadTargetsOrAutoMeta(paths []string, f *uploadFlags) ([]runner.UploadTarget, error) {
	if f.identifierScheme != "" && f.identifierScheme != schemeDirNNSlug {
		return nil, errors.New("недопустимое значение --identifier-scheme: поддерживается только dir-nn-slug")
	}

	if f.autoMeta {
		if f.identifierScheme == "" {
			return nil, errors.New("--auto-meta требует --identifier-scheme dir-nn-slug (единственная поддерживаемая схема идентификаторов)")
		}

		for _, conflict := range []struct {
			flag, value string
		}{
			{"--item", f.item},
			{"--identifier-from", f.identifierFrom},
			{"--key", f.key},
		} {
			if conflict.value != "" {
				return nil, errors.New(conflict.flag + " несовместим с --auto-meta: идентификаторы и ключи вычисляются из имён файлов")
			}
		}

		return nil, nil // the real targets are derived later, after the cheap checks
	}

	if f.identifierScheme != "" {
		return nil, errors.New("--identifier-scheme допустим только вместе с --auto-meta")
	}

	return uploadTargetsFrom(paths, f)
}

// deriveAutoMetaTargets derives targets and metadata from the PDFs and
// announces each derived identifier + title in human-readable mode. The
// returned error is already exit-classified: duplicate identifiers are a
// usage error (exit 2 — silent item-merge is worse), while convention and
// poppler failures are data errors of the file set (exit 1, review ruling
// of fix round 1).
func deriveAutoMetaTargets(con *console.Console, paths []string, jsonOut bool) ([]runner.UploadTarget, []pdfmeta.Meta, error) {
	targets, metas, err := uploadAutoMetaTargets(paths)
	if err != nil {
		var dup *duplicateIdentifierError
		if errors.As(err, &dup) {
			return nil, nil, exitUsage("%v", err)
		}

		return nil, nil, &ExitError{Code: ExitSomeFailed, Err: err}
	}

	if !jsonOut {
		for idx, t := range targets {
			con.Infof("Автометаданные: %s → %s «%s»", t.Path, t.Identifier, metas[idx].Title)
		}
	}

	return targets, metas, nil
}

// duplicateIdentifierError marks an --auto-meta batch whose files collide
// on one derived identifier — uploading it would silently merge items.
type duplicateIdentifierError struct {
	Identifier string
	First      string
	Second     string
}

func (e *duplicateIdentifierError) Error() string {
	return "повторяющийся идентификатор «" + e.Identifier + "»: элементы сольются — " +
		e.First + " и " + e.Second + "; переименуйте один из файлов"
}

// printDryRunDetail renders one file's complete derived plan for the
// dry-run review surface: title, creator, collection, language, year,
// subjects and a description head.
func printDryRunDetail(con *console.Console, meta pdfmeta.Meta, collection string) {
	detail := "Название: «" + meta.Title + "»"
	if meta.Creator != "" {
		detail += " · Автор: " + meta.Creator
	}

	detail += " · Коллекция: " + collection

	if meta.Language != "" {
		detail += " · Язык: " + meta.Language
	}

	if meta.Year != "" {
		detail += " · Год: " + meta.Year
	}

	if len(meta.Subjects) > 0 {
		detail += " · Темы: " + strings.Join(meta.Subjects, ", ")
	}

	con.Dimf("    %s", detail)

	if head := descriptionHead(meta.Description); head != "" {
		con.Dimf("    Описание: %s", head)
	}
}

// descriptionHead cuts the derived description to a 60-char preview head
// for the dry-run plan line (ellipsis marks the cut).
func descriptionHead(d string) string {
	head := d
	if len([]rune(d)) > 60 {
		head = string([]rune(d)[:60]) + "…"
	}

	return strings.TrimRight(head, " ")
}

// uploadAutoMetaTargets derives the targets and item metadata from the
// PDFs themselves: pdfmeta.DeriveMeta for the metadata, pdfmeta.Identifier
// for the dir-nn-slug scheme. Subjects/language/year land in the per-target
// Extra; Title/Description ride the target-level overrides.
func uploadAutoMetaTargets(paths []string) ([]runner.UploadTarget, []pdfmeta.Meta, error) {
	targets := make([]runner.UploadTarget, 0, len(paths))
	metas := make([]pdfmeta.Meta, 0, len(paths))
	seenIDs := make(map[string]string, len(paths)) // identifier → first path

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, nil, errors.New("не удалось определить путь " + p + ": " + err.Error())
		}

		dirName := filepath.Base(filepath.Dir(abs))

		meta, err := pdfmeta.DeriveMeta(abs, dirName)
		if err != nil {
			return nil, nil, err
		}

		id, err := pdfmeta.Identifier(dirName, filepath.Base(abs))
		if err != nil {
			return nil, nil, err
		}

		if prev, seen := seenIDs[id]; seen {
			return nil, nil, &duplicateIdentifierError{Identifier: id, First: prev, Second: abs}
		}

		seenIDs[id] = abs

		extra := ias3.Meta{}
		if len(meta.Subjects) > 0 {
			extra["subject"] = meta.Subjects
		}

		if meta.Language != "" {
			extra["language"] = []string{meta.Language}
		}

		if meta.Year != "" {
			extra["year"] = []string{meta.Year}
		}

		if meta.Creator != "" {
			extra["creator"] = []string{meta.Creator}
		}

		targets = append(targets, runner.UploadTarget{
			Path:        abs,
			Identifier:  id,
			Key:         filepath.Base(abs),
			Title:       meta.Title,
			Description: meta.Description,
			Extra:       extra,
		})
		metas = append(metas, meta)
	}

	return targets, metas, nil
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

	// Names are validated here, at flag-parse time, so a malformed
	// --meta name is a usage error before any per-file work (poppler
	// extraction, hashing) — never a late per-file failure.
	if err := ias3.ValidateExtraMeta(meta); err != nil {
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
	fs.BoolVar(&f.autoMeta, "auto-meta", false,
		"вывести метаданные каждого PDF из самого файла (pdfinfo/pdftotext, poppler-utils): название, автор, год, описание, темы, язык; требует poppler-utils и имена вида NN_автор_тема.pdf")
	fs.StringVar(&f.identifierScheme, "identifier-scheme", "",
		"схема идентификаторов при --auto-meta, единственное значение dir-nn-slug: <каталог>-<номер>-<имя-файла>")

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

	targets, err := uploadTargetsOrAutoMeta(rawPaths, f)
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

	// Cheap flag validation (incl. the --meta name contract) precedes
	// the poppler work: a usage error must not be masked by a data
	// error from per-file extraction.
	opts, err := headerOptsFrom(cfg, f)
	if err != nil {
		return exitUsage("%v", err)
	}

	var derived []pdfmeta.Meta
	if f.autoMeta {
		targets, derived, err = deriveAutoMetaTargets(con, rawPaths, jsonOut)
		if err != nil {
			return err
		}
	}

	if dryRun {
		return runUploadDry(con, journalFlag, targets, derived, cfg.Upload.Collection)
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
// network or creating the journal file. With --auto-meta the derived plan
// shows identifier, title, collection and language per file — the review
// surface the owner inspects before a live run.
func runUploadDry(con *console.Console, journalFlag string, targets []runner.UploadTarget, derived []pdfmeta.Meta, collection string) error {
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

	for idx, t := range targets {
		composite := t.Path + "\x00" + t.Identifier + "/" + t.Key
		if doneSet[composite] {
			con.Dimf("Пропущено (уже в журнале): %s → %s/%s", t.Path, t.Identifier, t.Key)

			skipped++

			continue
		}

		con.Infof("Загрузить: %s → %s/%s", t.Path, t.Identifier, t.Key)

		if idx < len(derived) {
			printDryRunDetail(con, derived[idx], collection)
		}
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
