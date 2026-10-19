package commands

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/httpx"
)

// The non-TTY failure must be actionable: it names the exact TOML keys, the
// s3.php key-generation URL, and tells the user to run init interactively.
func TestInitNonTTYFailsActionable(t *testing.T) {
	var out, errOut strings.Builder
	err := runInit("/home/u/.config/wauploader/wauploader.toml", false, false, nil, false, &out, &errOut)
	if err == nil {
		t.Fatal("runInit returned nil error for non-TTY stdin, want actionable Russian error")
	}
	msg := err.Error()
	for _, want := range []string{
		"s3_access", "s3_secret", "cookie_user", "cookie_sig",
		"https://archive.org/account/s3.php",
		"интерактивно",
		"wauploader.toml",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("non-TTY error does not mention %q: %q", want, msg)
		}
	}
}

// lastLine returns the last non-empty line of s.
func lastLine(t *testing.T, s string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	t.Fatal("output has no non-empty line")
	return ""
}

// overrideBootstrap swaps the bootstrap seam for fixed credentials and
// error, restoring the real one via t.Cleanup.
func overrideBootstrap(t *testing.T, creds auth.Credentials, err error) {
	t.Helper()
	old := bootstrapFn
	bootstrapFn = func(context.Context, *httpx.Client, string, string) (auth.Credentials, error) {
		return creds, err
	}
	t.Cleanup(func() { bootstrapFn = old })
}

// overrideReadPassword swaps the no-echo password read for a fixed value.
func overrideReadPassword(t *testing.T, password string) {
	t.Helper()
	old := readPasswordFn
	readPasswordFn = func(*os.File) ([]byte, error) { return []byte(password), nil }
	t.Cleanup(func() { readPasswordFn = old })
}

// stdinWithEmail returns a real *os.File carrying one email line, standing
// in for the interactive stdin of a runInit test.
func stdinWithEmail(t *testing.T, email string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("temp stdin: %v", err)
	}
	if _, err := f.WriteString(email + "\n"); err != nil {
		t.Fatalf("seed stdin: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("rewind stdin: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// runInitWithSeams drives a full runInit with the network and password
// seams in place; the config lands in a per-test temp dir.
func runInitWithSeams(t *testing.T, jsonOut bool, out, errOut io.Writer) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "wauploader.toml")
	err := runInit(cfgPath, jsonOut, false, stdinWithEmail(t, "user@example.com"), true, out, errOut)
	if err != nil {
		t.Fatalf("runInit returned error: %v", err)
	}
	return cfgPath
}

// TestInitKeysWithoutCookiesSavesAndWarns: a successful exchange that
// returns keys but no cookies is a usable state — init must SAVE the keys
// to the config and surface the Russian warning, never fail and never
// drop the keys.
func TestInitKeysWithoutCookiesSavesAndWarns(t *testing.T) {
	overrideBootstrap(t, auth.Credentials{Access: "ACCESS12345678", Secret: "SECRET87654321"},
		auth.ErrCookiesNotReturned)
	overrideReadPassword(t, "pw")

	// Human mode: warning on out, keys saved, cookies empty.
	var out, errOut strings.Builder
	cfgPath := runInitWithSeams(t, false, &out, &errOut)
	for _, want := range []string{"куки не получены", "SPN2 будет работать по ключам"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human init output lacks warning %q:\n%s", want, out.String())
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after keys-only init: %v", err)
	}
	if cfg.Auth.S3Access != "ACCESS12345678" || cfg.Auth.S3Secret != "SECRET87654321" {
		t.Error("keys-only init did not save the S3 keys to the config")
	}
	if cfg.Auth.CookieUser != "" || cfg.Auth.CookieSig != "" {
		t.Errorf("keys-only init stored cookies: %q / %q", cfg.Auth.CookieUser, cfg.Auth.CookieSig)
	}

	// JSON mode: the warning travels inside the JSON object, and the keys
	// are saved the same way. (Whole-stream JSON purity is asserted by the
	// dedicated redirect test once prompts move to errOut.)
	overrideBootstrap(t, auth.Credentials{Access: "ACCESS12345678", Secret: "SECRET87654321"},
		auth.ErrCookiesNotReturned)
	out.Reset()
	errOut.Reset()
	cfgPath = runInitWithSeams(t, true, &out, &errOut)
	var payload map[string]any
	if err := json.Unmarshal([]byte(lastLine(t, out.String())), &payload); err != nil {
		t.Fatalf("JSON init output line is not a JSON object: %v\n%s", err, out.String())
	}
	warn, _ := payload["warning"].(string)
	for _, want := range []string{"куки не получены", "SPN2 будет работать по ключам"} {
		if !strings.Contains(warn, want) {
			t.Errorf("JSON init output warning = %q, want it to contain %q", warn, want)
		}
	}
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after JSON keys-only init: %v", err)
	}
	if cfg.Auth.S3Access != "ACCESS12345678" {
		t.Error("JSON keys-only init did not save the S3 keys")
	}
}

// The init success surface shows masks (first 4 chars + length), never the
// full secret values, in both human and JSON modes.
func TestSecretsNeverLogged(t *testing.T) {
	creds := auth.Credentials{
		Access:     "ABCDEFGHIJKLMNOP",
		Secret:     "QRSTUVWXYZ123456",
		CookieUser: "cookie-user-long-value",
		CookieSig:  "cookie-sig-long-value",
	}
	plainValues := []string{
		"ABCDEFGHIJKLMNOP", "QRSTUVWXYZ123456", "cookie-user-long-value", "cookie-sig-long-value",
	}

	var out, errOut strings.Builder
	writeInitSuccess(&out, &errOut, false, false, false, "/tmp/x/wauploader.toml", false, creds)
	human := out.String() + errOut.String()
	for _, secret := range plainValues {
		if strings.Contains(human, secret) {
			t.Errorf("human init output leaks the full value %q:\n%s", secret, human)
		}
	}
	for _, want := range []string{"ABCD***", "QRST***", "cook***"} {
		if !strings.Contains(human, want) {
			t.Errorf("human init output lacks the mask %q:\n%s", want, human)
		}
	}

	out.Reset()
	errOut.Reset()
	writeInitSuccess(&out, &errOut, false, false, true, "/tmp/x/wauploader.toml", false, creds)
	jsonOut := out.String() + errOut.String()
	for _, secret := range plainValues {
		if strings.Contains(jsonOut, secret) {
			t.Errorf("JSON init output leaks the full value %q:\n%s", secret, jsonOut)
		}
	}
	if !strings.Contains(jsonOut, `"s3_access"`) || !strings.Contains(jsonOut, "***") {
		t.Errorf("JSON init output lacks masked fields:\n%s", jsonOut)
	}
}

// expandHomePath resolves a leading ~ so the tool never materializes a
// literal "~" directory on disk.
func TestExpandHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := expandHomePath("~/wauploader/wauploader.toml"); got != home+"/wauploader/wauploader.toml" {
		t.Errorf("expandHomePath(~/x) = %q, want %q", got, home+"/wauploader/wauploader.toml")
	}
	if got := expandHomePath("/absolute/path.toml"); got != "/absolute/path.toml" {
		t.Errorf("expandHomePath(absolute) = %q, want unchanged", got)
	}
}

// promptLine must consume the email line from the reader it is given
// (guards against reading from os.Stdin directly inside the helper).
func TestPromptLineReadsFromProvidedReader(t *testing.T) {
	var out strings.Builder
	got, err := promptLine(strings.NewReader("user@example.com\n"), &out, "Адрес: ")
	if err != nil {
		t.Fatalf("promptLine returned error: %v", err)
	}
	if got != "user@example.com" {
		t.Errorf("promptLine = %q, want %q", got, "user@example.com")
	}
	if !strings.Contains(out.String(), "Адрес: ") {
		t.Errorf("prompt %q was not written to the prompt writer", out.String())
	}
	if _, err := promptLine(strings.NewReader("   \n"), io.Discard, "Адрес: "); err == nil {
		t.Error("promptLine returned nil error for a blank line, want empty-input error")
	}
}
