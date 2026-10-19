package commands

import (
	"io"
	"strings"
	"testing"

	"github.com/4q4r/wauploader/internal/auth"
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
	writeInitSuccess(&out, &errOut, false, false, false, "/tmp/x/wauploader.toml", creds)
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
	writeInitSuccess(&out, &errOut, false, false, true, "/tmp/x/wauploader.toml", creds)
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
