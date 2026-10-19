// status.go implements the `wauploader status` command: the state of one
// SPN2 job, of the user's capture session (--user), or of the SPN2 service
// (--system). Exactly one of the three selectors is required (usage error
// otherwise). Every result is one JSON object in --json mode.

package commands

import (
	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/spn2"
	"github.com/spf13/cobra"
)

// statusJobJSON is the machine-readable shape of one job-status answer.
type statusJobJSON struct {
	Event        string  `json:"event"`
	Status       string  `json:"status"`
	JobID        string  `json:"job_id,omitempty"`
	OriginalURL  string  `json:"original_url,omitempty"`
	Timestamp    string  `json:"timestamp,omitempty"`
	DurationSec  float64 `json:"duration_sec,omitempty"`
	DownloadSize int64   `json:"download_size,omitempty"`
	HTTPStatus   int     `json:"http_status,omitempty"`
	StatusExt    string  `json:"status_ext,omitempty"`
	Message      string  `json:"message,omitempty"`
	Exception    string  `json:"exception,omitempty"`
	Screenshot   string  `json:"screenshot,omitempty"`
}

// NewStatusCommand builds the `status` subcommand.
func NewStatusCommand(configPath *string, jsonOut, noColor *bool) *cobra.Command {
	var userMode, systemMode bool

	cmd := &cobra.Command{
		Use:          "status [JOB_ID | --user | --system]",
		Short:        "Показать статус задачи SPN2, сессии пользователя или службы",
		Args:         usageMaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			selected := 0
			if len(args) == 1 {
				selected++
			}

			if userMode {
				selected++
			}

			if systemMode {
				selected++
			}

			if selected != 1 {
				return exitUsage("укажите ровно одно: идентификатор задачи, --user или --system")
			}

			return runStatus(cmd, args, userMode, systemMode,
				derefString(configPath, "~/.config/wauploader/wauploader.toml"),
				derefBool(jsonOut), derefBool(noColor))
		},
	}
	cmd.Flags().BoolVar(&userMode, "user", false,
		"статус сессии SPN2: сколько заданий доступно и выполняется")
	cmd.Flags().BoolVar(&systemMode, "system", false,
		"состояние службы SPN2")

	return cmd
}

// runStatus resolves credentials (auth errors exit 3), queries SPN2 and
// renders the answer. Query failures (network, unknown job) exit 1: the
// command itself was used correctly.
func runStatus(cmd *cobra.Command, args []string, userMode, systemMode bool, cfgPath string, jsonOut, noColor bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	con := console.New(out, errOut, writerIsTTY(out), noColor)

	cfg, err := loadConfig(expandHomePath(cfgPath), errOut)
	if err != nil {
		return err
	}

	creds, err := auth.Resolve(cfg)
	if err != nil {
		return exitAuth(err)
	}

	sp := spn2.NewClient(newHTTPClient(), creds)
	ctx := cmd.Context()

	switch {
	case userMode:
		avail, processing, err := sp.UserStatus(ctx)
		if err != nil {
			return &ExitError{Code: ExitSomeFailed, Err: err}
		}

		if jsonOut {
			con.JSON(map[string]int{"available": avail, "processing": processing})
			return nil
		}

		con.Infof("Сессия SPN2: доступно %d · выполняется %d", avail, processing)
	case systemMode:
		ok, overloaded, err := sp.SystemStatus(ctx)
		if err != nil {
			return &ExitError{Code: ExitSomeFailed, Err: err}
		}

		if jsonOut {
			con.JSON(map[string]bool{"ok": ok, "overloaded": overloaded})
			return nil
		}

		switch {
		case ok:
			con.Successf("Служба SPN2: работает")
		case overloaded:
			con.Warnf("Служба SPN2: перегружена")
		default:
			con.Errorf("Служба SPN2: недоступна")
		}
	default:
		st, err := sp.Status(ctx, args[0])
		if err != nil {
			return &ExitError{Code: ExitSomeFailed, Err: err}
		}

		if jsonOut {
			con.JSON(statusJobJSON{
				Event:        "status",
				Status:       st.State,
				JobID:        st.JobID,
				OriginalURL:  st.OriginalURL,
				Timestamp:    st.Timestamp,
				DurationSec:  st.DurationSec,
				DownloadSize: st.DownloadSize,
				HTTPStatus:   st.HTTPStatus,
				StatusExt:    st.StatusExt,
				Message:      st.Message,
				Exception:    st.Exception,
				Screenshot:   st.Screenshot,
			})

			return nil
		}

		renderJobStatus(con, st)
	}

	return nil
}

// renderJobStatus prints the human form of one job status. The Снимок line
// is gated behind the error check: a failed job with a leftover timestamp
// must not advertise a nonexistent snapshot.
func renderJobStatus(con *console.Console, st spn2.Status) {
	con.Infof("Статус задачи %s: %s", st.JobID, st.State)

	if st.OriginalURL != "" {
		con.Dimf("URL: %s", st.OriginalURL)
	}

	if st.DownloadSize > 0 {
		con.Dimf("Размер: %s", console.FormatBytesRU(st.DownloadSize))
	}

	if st.StatusExt != "" {
		con.Errorf("Ошибка (%s): %s", st.StatusExt, st.Message)
		return
	}

	if st.Timestamp != "" {
		con.Dimf("Снимок: https://web.archive.org/web/%s/%s", st.Timestamp, st.OriginalURL)
	}

	if st.Message != "" {
		con.Dimf("Сообщение: %s", st.Message)
	}

	if st.Exception != "" {
		con.Dimf("Исключение: %s", st.Exception)
	}
}
