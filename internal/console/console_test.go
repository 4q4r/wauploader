package console

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ansiPattern matches ANSI SGR escape sequences so tests can strip coloring
// and compare the underlying text byte-for-byte.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func ttyColorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
}

func TestColorGatePlainInNonTTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, false, false)
	c.Infof("Информация: %s", "тест")
	c.Successf("Готово: %s", "тест")
	c.Warnf("Внимание: %s", "тест")
	c.Dimf("Детали: %s", "тест")
	c.Errorf("Ошибка: %s", "тест")

	for _, b := range out.Bytes() {
		if b == 0x1b {
			t.Fatalf("non-TTY output on out contains escape byte: %q", out.String())
		}
	}
	for _, b := range errOut.Bytes() {
		if b == 0x1b {
			t.Fatalf("non-TTY output on errOut contains escape byte: %q", errOut.String())
		}
	}
}

func TestColorGateNoColorFlag(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, true)
	c.Successf("Готово")

	if strings.ContainsRune(out.String(), 0x1b) {
		t.Fatalf("--no-color output contains escape byte: %q", out.String())
	}
}

func TestColorGateDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.Successf("Готово")

	if strings.ContainsRune(out.String(), 0x1b) {
		t.Fatalf("TERM=dumb output contains escape byte: %q", out.String())
	}
}

func TestColorGateNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.Successf("Готово")

	if strings.ContainsRune(out.String(), 0x1b) {
		t.Fatalf("NO_COLOR output contains escape byte: %q", out.String())
	}
}

func TestColoredInTTY(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.Infof("Инфо")
	c.Successf("Успех")
	c.Warnf("Предупреждение")
	c.Dimf("Детали")
	c.Errorf("Ошибка")

	if !strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("TTY output on out contains no escape sequence: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "\x1b[") {
		t.Fatalf("TTY output on errOut contains no escape sequence: %q", errOut.String())
	}
}

// TestColorByteParity proves the gate only toggles escape sequences: with the
// sequences stripped, colored and plain outputs are byte-identical.
func TestColorByteParity(t *testing.T) {
	calls := []struct {
		name string
		run  func(c *Console)
	}{
		{"info", func(c *Console) { c.Infof("Информация: %s", "тест") }},
		{"success", func(c *Console) { c.Successf("Готово: %s", "тест") }},
		{"warn", func(c *Console) { c.Warnf("Внимание: %s", "тест") }},
		{"dim", func(c *Console) { c.Dimf("Детали: %s", "тест") }},
		{"error", func(c *Console) { c.Errorf("Ошибка: %s", "тест") }},
	}

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			ttyColorEnv(t)

			var plainOut, plainErr, colorOut, colorErr bytes.Buffer
			tc.run(New(&plainOut, &plainErr, false, false))
			tc.run(New(&colorOut, &colorErr, true, false))

			if got, want := ansiPattern.ReplaceAllString(colorOut.String(), ""), plainOut.String(); got != want {
				t.Fatalf("parity out: colored-stripped %q != plain %q", got, want)
			}
			if got, want := ansiPattern.ReplaceAllString(colorErr.String(), ""), plainErr.String(); got != want {
				t.Fatalf("parity errOut: colored-stripped %q != plain %q", got, want)
			}
		})
	}
}

func TestWritersRouting(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, false, false)

	c.Infof("Инфо")
	c.Successf("Успех")
	c.Warnf("Предупреждение")
	c.Dimf("Детали")
	if out.Len() == 0 {
		t.Fatal("Info/Success/Warn/Dim wrote nothing to out")
	}
	if errOut.Len() != 0 {
		t.Fatalf("Info/Success/Warn/Dim leaked to errOut: %q", errOut.String())
	}

	before := out.Len()
	c.Errorf("Ошибка")
	if errOut.Len() == 0 {
		t.Fatal("Errorf wrote nothing to errOut")
	}
	if out.Len() != before {
		t.Fatalf("Errorf leaked to out: %q", out.String()[before:])
	}
}

func TestGlyphsPresent(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, false, false)

	c.Successf("Успех")
	c.Warnf("Предупреждение")
	c.Dimf("Детали")
	c.Errorf("Ошибка")

	if !strings.Contains(out.String(), "✓") {
		t.Fatalf("success output missing ✓ glyph: %q", out.String())
	}
	if !strings.Contains(out.String(), "?") {
		t.Fatalf("warn output missing ? glyph: %q", out.String())
	}
	if !strings.Contains(out.String(), "·") {
		t.Fatalf("dim output missing · glyph: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "✗") {
		t.Fatalf("error output missing ✗ glyph: %q", errOut.String())
	}
}

func TestJSONNeverColored(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.JSON(map[string]string{"status": "ok", "url": "https://example.org"})

	for _, b := range out.Bytes() {
		if b == 0x1b {
			t.Fatalf("JSON output contains escape byte: %q", out.String())
		}
	}
	var v map[string]string
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v (%q)", err, out.String())
	}
}

func TestJSONOneObjectPerCall(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.JSON(map[string]int{"a": 1})
	c.JSON(map[string]int{"b": 2})

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSON objects, got %d in %q", len(lines), out.String())
	}
	for i, line := range lines {
		if strings.ContainsAny(line, " \t") {
			t.Fatalf("JSON object %d is not compact: %q", i, line)
		}
		var v map[string]int
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("JSON object %d is not valid JSON: %v (%q)", i, err, line)
		}
	}
}

func TestJSONWritesToOut(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.JSON(map[string]string{"ok": "да"})

	if errOut.Len() != 0 {
		t.Fatalf("JSON leaked to errOut: %q", errOut.String())
	}
	if out.Len() == 0 {
		t.Fatal("JSON wrote nothing to out")
	}
}

func TestJSONMarshalErrorGoesToErrOut(t *testing.T) {
	ttyColorEnv(t)

	var out, errOut bytes.Buffer
	c := New(&out, &errOut, true, false)
	c.JSON(map[string]chan int{"bad": make(chan int)})

	if errOut.Len() == 0 {
		t.Fatal("marshal error was swallowed; want a loud failure on errOut")
	}
	if strings.Contains(out.String(), "\x00") {
		t.Fatalf("partial JSON written to out: %q", out.String())
	}
}

func TestFormatBytesRUBoundaries(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0,0 Б"},
		{"just below kibibyte", 1023, "1023,0 Б"},
		{"kibibyte", 1024, "1,0 КБ"},
		{"one and a half kibibyte", 1536, "1,5 КБ"},
		{"mebibyte", 1048576, "1,0 МБ"},
		{"gibibyte", 1073741824, "1,0 ГБ"},
		{"five gibibytes", 5 * 1024 * 1024 * 1024, "5,0 ГБ"},
		{"tebibyte", 1099511627776, "1,0 ТБ"},
		{"negative uses abs", -512, "512,0 Б"},
		{"negative kibibyte uses abs", -1024, "1,0 КБ"},
		// The exact absolute value of MinInt64 overflows int64; the
		// magnitude must be computed in uint64 — no minus sign, exact
		// unsigned magnitude 2^63 = 8388608 ТБ (the unit ladder caps
		// at terabytes).
		{"min int64 exact unsigned magnitude", math.MinInt64, "8388608,0 ТБ"},
		{"min int64 plus one", math.MinInt64 + 1, "8388608,0 ТБ"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatBytesRU(tc.n); got != tc.want {
				t.Fatalf("FormatBytesRU(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

func TestIsTTYFalseForPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	if IsTTY(r) {
		t.Fatal("IsTTY(pipe) = true, want false")
	}
	if IsTTY(nil) {
		t.Fatal("IsTTY(nil) = true, want false")
	}
}
