// jobs.go implements the `wauploader jobs` command: a human listing of the
// journal records with an optional state filter, and --clear to remove the
// filtered records. quota_parked records are always visible here: they are
// never auto-resumed — requeueing happens by explicitly re-listing the
// targets in `save` (Task 10/12 ledger ruling).

package commands

import (
	"sort"

	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/journal"
	"github.com/spf13/cobra"
)

// jobsStateFilter maps the documented --state values onto journal states.
var jobsStateFilter = map[string]journal.State{
	"failed":  journal.StateFailed,
	"queued":  journal.StateQueued,
	"pending": journal.StatePending,
	"done":    journal.StateDone,
}

// NewJobsCommand builds the `jobs` subcommand. The listing is a human
// surface: --json does not apply (the plan scopes JSON output to events and
// status results).
func NewJobsCommand(journalPath *string, noColor *bool) *cobra.Command {
	var stateArg string

	var doClear bool

	cmd := &cobra.Command{
		Use:          "jobs [--state failed|queued|pending|done] [--clear]",
		Short:        "Показать записи журнала выполнения; --clear удаляет отфильтрованные записи",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJobs(cmd, derefString(journalPath, ""), stateArg, doClear, derefBool(noColor))
		},
	}
	cmd.Flags().StringVar(&stateArg, "state", "",
		"фильтр по состоянию: failed, queued, pending или done")
	cmd.Flags().BoolVar(&doClear, "clear", false,
		"удалить записи, подходящие под фильтр (все, если фильтр не задан)")

	return cmd
}

// runJobs opens the journal (creating it if absent — it is this command's
// own domain), lists or clears records.
func runJobs(cmd *cobra.Command, journalFlag, stateArg string, doClear, noColor bool) error {
	out := cmd.OutOrStdout()
	// The listing is one coherent stdout stream: the listing console binds
	// both of its writers to out, so failed records keep the ✗ glyph
	// without leaving the listing. Failures are returned as ExitError and
	// printed by the caller on stderr.
	con := console.New(out, out, writerIsTTY(out), noColor)

	jp, err := journalPathOr(journalFlag)
	if err != nil {
		return err
	}

	st, err := journal.Open(jp)
	if err != nil {
		return &ExitError{Code: ExitSomeFailed, Err: err}
	}
	defer st.Close() //nolint:errcheck // read-only after open

	var states []journal.State

	if stateArg != "" {
		s, ok := jobsStateFilter[stateArg]
		if !ok {
			return exitUsage("недопустимое значение --state %q: допустимы failed, queued, pending, done", stateArg)
		}

		states = []journal.State{s}
	}

	recs, err := st.ByStates(nil, states)
	if err != nil {
		return &ExitError{Code: ExitSomeFailed, Err: err}
	}

	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Kind != recs[j].Kind {
			return recs[i].Kind < recs[j].Kind
		}

		return recs[i].UpdatedAt.Before(recs[j].UpdatedAt)
	})

	if doClear {
		removed := 0

		for _, rec := range recs {
			ok, err := st.Delete(rec.Kind, rec.Target)
			if err != nil {
				return &ExitError{Code: ExitSomeFailed, Err: err}
			}

			if ok {
				removed++
			}
		}

		con.Successf("Удалено записей: %d", removed)

		return nil
	}

	if len(recs) == 0 {
		con.Infof("Журнал пуст")
		return nil
	}

	for _, rec := range recs {
		renderJobRecord(con, rec)
	}

	con.Dimf("Всего записей: %d", len(recs))

	return nil
}

// renderJobRecord prints one record line, glyphed by state.
func renderJobRecord(con *console.Console, rec journal.Record) {
	label := "сохранение"
	if rec.Kind == journal.KindUpload {
		label = "загрузка"
	}

	line := label + " " + displayTarget(rec.Target) + " — " + string(rec.State)
	if !rec.UpdatedAt.IsZero() {
		line += " (обновлено " + rec.UpdatedAt.Format("2006-01-02 15:04") + ")"
	}

	switch rec.State {
	case journal.StateDone:
		con.Successf("%s", line)
	case journal.StateFailed:
		con.Errorf("%s", line)
	case journal.StateQuotaParked:
		con.Warnf("%s", line)
	default:
		con.Infof("%s", line)
	}

	if rec.ErrMsg != "" {
		con.Dimf("%s", rec.ErrMsg)
	}
}
