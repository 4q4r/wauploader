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

// runInit drives the interactive bootstrap. stdin must be the process's
// terminal file for the no-echo password read; stdinTTY reports whether it
// is one. With a non-terminal stdin it fails fast with nonTTYHint — the
// non-interactive path never prompts. out/errOut are injectable for tests.
func runInit(cfgPath string, jsonOut, noColor bool, stdin *os.File, stdinTTY bool, out, errOut io.Writer) error {
	if !stdinTTY || stdin == nil {
		return nonTTYHint(cfgPath)
	}
	con := console.New(out, errOut, stdinTTY, noColor)

	email, err := promptLine(stdin, out, "Адрес электронной почты archive.org: ")
	if err != nil {
		return err
	}
	fmt.Fprint(out, "Пароль (ввод не отображается): ")
	raw, err := term.ReadPassword(int(stdin.Fd()))
	fmt.Fprintln(out)
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
	creds, err := auth.Bootstrap(context.Background(), cl, email, password)
	password = "" // drop the plaintext reference before any reporting
	if err != nil {
		return err
	}

	savePath := expandHomePath(cfgPath)
	if err := auth.SaveToConfig(savePath, creds, ""); err != nil {
		return err
	}
	writeInitSuccess(out, errOut, stdinTTY, noColor, jsonOut, savePath, creds)
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
// first 4 characters plus length, in both human and JSON modes.
func writeInitSuccess(out, errOut io.Writer, isTTY, noColor, jsonOut bool, cfgPath string, c auth.Credentials) {
	con := console.New(out, errOut, isTTY, noColor)
	if jsonOut {
		con.JSON(map[string]any{
			"event":  "init",
			"status": "ok",
			"config": cfgPath,
			"masked": map[string]string{
				"s3_access":   auth.Mask(c.Access),
				"s3_secret":   auth.Mask(c.Secret),
				"cookie_user": auth.Mask(c.CookieUser),
				"cookie_sig":  auth.Mask(c.CookieSig),
			},
		})
		return
	}
	con.Successf("Учётные данные сохранены в %s", cfgPath)
	con.Dimf("s3_access: %s", auth.Mask(c.Access))
	con.Dimf("s3_secret: %s", auth.Mask(c.Secret))
	con.Dimf("cookie_user: %s", auth.Mask(c.CookieUser))
	con.Dimf("cookie_sig: %s", auth.Mask(c.CookieSig))
}
