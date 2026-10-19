package version

import (
	"bytes"
	"testing"
)

func TestUserAgentExactString(t *testing.T) {
	want := "wauploader/dev (+https://github.com/4q4r/wauploader)"
	if got := UserAgent(); got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
}

func TestVersionOutputPlain(t *testing.T) {
	var out bytes.Buffer
	cmd := NewVersionCommand(new(bool))
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "wauploader version dev\n"
	if out.String() != want {
		t.Fatalf("plain output = %q, want %q", out.String(), want)
	}
}

func TestVersionOutputJSON(t *testing.T) {
	var out bytes.Buffer
	jsonFlag := true
	cmd := NewVersionCommand(&jsonFlag)
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := `{"name":"wauploader","version":"dev"}` + "\n"
	if out.String() != want {
		t.Fatalf("json output = %q, want %q", out.String(), want)
	}
}

func TestVersionStampOverridable(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()
	Version = "9.9.9-test"

	var out bytes.Buffer
	cmd := NewVersionCommand(new(bool))
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "wauploader version 9.9.9-test\n"
	if out.String() != want {
		t.Fatalf("stamped output = %q, want %q", out.String(), want)
	}
}
