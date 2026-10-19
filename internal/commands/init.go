// Package commands hosts the wauploader subcommands. This file implements
// `wauploader init`: the interactive xauthn bootstrap that exchanges an
// email+password prompt for IA-S3 keys and login cookies, saves them to the
// TOML config, and prints only masked secrets.
package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/console"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// nonTTYHint is the actionable guidance for a non-interactive terminal: it
// names the exact TOML keys, the key-generation URL, and the interactive
// escape hatch, per the global constraint.
func nonTTYHint(cfgPath string) error {
	return fmt.Errorf("интерактивный вход недоступен: стандартный ввод не является терминалом.\n"+
		"Отредактируйте файл конфигурации вручную (%s), заполнив раздел [auth]:\n"+
		"  s3_access и s3_secret — ключи генерируются на https://archive.org/account/s3.php;\n"+
		"  cookie_user и cookie_sig — куки входа archive.org.\n"+
		"Или запустите команду интерактивно в терминале: wauploader init", cfgPath)
}

// NewInitCommand builds the `init` subcommand. The pointers reference the
// caller's persistent flag values (same pattern as version.NewVersionCommand).
func NewInitCommand(configPath *string, jsonOut, noColor *bool) *cobra.Command {
	return &cobra.Command{
		Use:          "init",
		Short:        "Получить ключи archive.org по email и паролю и записать их в конфигурацию",
		Args:         cobra.NoArgs,
		SilenceUsage: true, // the Russian guidance below replaces the usage dump
		RunE: func(_ *cobra.Command, _ []string) error {
			return runInit(derefString(configPath, "~/.config/wauploader/wauploader.toml"),
				derefBool(jsonOut), derefBool(noColor),
				os.Stdin, console.IsTTY(os.Stdin), os.Stdout, os.Stderr)
		},
	}
}

func derefString(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func derefBool(p *bool) bool { return p != nil && *p }

// bootstrapFn and readPasswordFn are test seams: production always uses
// the implementations assigned here. Tests override them (restoring via
// t.Cleanup) so a runInit test never touches the network or a real
// terminal's no-echo read.
var (
	bootstrapFn = func(ctx context.Context, cl *httpx.Client, email, password string) (auth.Credentials, error) {
		return auth.Bootstrap(ctx, cl, email, password)
	}
	readPasswordFn = func(stdin *os.File) ([]byte, error) {
		return term.ReadPassword(int(stdin.Fd()))
	}
)

// runInit drives the interactive bootstrap. stdin must be the process's
// terminal file for the no-echo password read; stdinTTY reports whether it
// is one and gates interactivity only. Color decisions follow the OUT
// stream: outTTY is computed from out when it is an *os.File, so a
// redirected out (wauploader init --json > result.json) never receives
// ANSI, even on an interactive stdin. Prompts and progress go to errOut,
// keeping out byte-pure for --json consumers. out/errOut are injectable
// for tests.
func runInit(cfgPath string, jsonOut, noColor bool, stdin *os.File, stdinTTY bool, out, errOut io.Writer) error {
	if !stdinTTY || stdin == nil {
		return nonTTYHint(cfgPath)
	}
	var outTTY bool
	if f, ok := out.(*os.File); ok {
		outTTY = console.IsTTY(f)
	}
	// The prompt/progress console writes to errOut as well: its out-bound
	// lines (Infof) must never contaminate the machine-readable out stream.
	con := console.New(errOut, errOut, outTTY, noColor)

	email, err := promptLine(stdin, errOut, "Адрес электронной почты archive.org: ")
	if err != nil {
		return err
	}
	fmt.Fprint(errOut, "Пароль (ввод не отображается): ")
	raw, err := readPasswordFn(stdin)
	fmt.Fprintln(errOut)
	if err != nil {
		return fmt.Errorf("не удалось прочитать пароль: %w", err)
	}
	password := string(raw)
	if email == "" {
		return errors.New("не указан адрес электронной почты archive.org")
	}
	if password == "" {
		return errors.New("не указан пароль archive.org")
	}

	con.Infof("Выполняется вход на archive.org…")
	cl := httpx.NewClient(nil, "")
	creds, err := bootstrapFn(context.Background(), cl, email, password)
	password = "" // drop the plaintext reference before any reporting
	if err != nil && !errors.Is(err, auth.ErrCookiesNotReturned) {
		return err
	}
	// Keys without cookies are a usable state: save the keys and warn —
	// the error, when present, is exactly the ErrCookiesNotReturned
	// sentinel whose Russian text becomes the user-visible warning.
	warnCookies := errors.Is(err, auth.ErrCookiesNotReturned)

	savePath := expandHomePath(cfgPath)
	if err := auth.SaveToConfig(savePath, creds, ""); err != nil {
		return err
	}
	writeInitSuccess(out, errOut, outTTY, noColor, jsonOut, savePath, warnCookies, creds)
	return nil
}

// promptLine writes prompt and reads one trimmed line from r.
func promptLine(r io.Reader, w io.Writer, prompt string) (string, error) {
	if _, err := fmt.Fprint(w, prompt); err != nil {
		return "", fmt.Errorf("не удалось вывести приглашение: %w", err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("не удалось прочитать ввод: %w", err)
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return "", errors.New("ввод пуст: укажите значение")
	}
	return value, nil
}

// expandHomePath resolves a leading ~ so the tool never materializes a
// literal "~" directory on disk.
func expandHomePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// writeInitSuccess reports the saved credentials with masked values only:
// first 4 characters plus length, in both human and JSON modes. outTTY is
// the OUT stream's terminal state (color gate). When warnCookies is set
// (login returned keys without cookies), the human surface gains a warning
// line and the JSON object a "warning" field — both carry
// auth.ErrCookiesNotReturned's Russian text.
func writeInitSuccess(out, errOut io.Writer, outTTY, noColor, jsonOut bool, cfgPath string, warnCookies bool, c auth.Credentials) {
	con := console.New(out, errOut, outTTY, noColor)
	if jsonOut {
		payload := map[string]any{
			"event":  "init",
			"status": "ok",
			"config": cfgPath,
			"masked": map[string]string{
				"s3_access":   auth.Mask(c.Access),
				"s3_secret":   auth.Mask(c.Secret),
				"cookie_user": auth.Mask(c.CookieUser),
				"cookie_sig":  auth.Mask(c.CookieSig),
			},
		}
		if warnCookies {
			payload["warning"] = auth.ErrCookiesNotReturned.Error()
		}
		con.JSON(payload)
		return
	}
	if warnCookies {
		con.Warnf("%s", auth.ErrCookiesNotReturned.Error())
	}
	con.Successf("Учётные данные сохранены в %s", cfgPath)
	con.Dimf("s3_access: %s", auth.Mask(c.Access))
	con.Dimf("s3_secret: %s", auth.Mask(c.Secret))
	con.Dimf("cookie_user: %s", auth.Mask(c.CookieUser))
	con.Dimf("cookie_sig: %s", auth.Mask(c.CookieSig))
}
