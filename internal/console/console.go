// Package console provides the human-facing terminal surface for wauploader:
// Russian status lines with TTY-gated ANSI colors and a strict JSON channel.
//
// The surface rules (docs/plan.md): Russian, human-readable, no time=/level=
// prefixes, full words. With the color gate closed, output is byte-for-byte
// plain. The JSON channel is never colored.
package console

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Raw ANSI SGR sequences (no color libraries).
const (
	sgrReset  = "\x1b[0m"
	sgrRed    = "\x1b[31m"
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrCyan   = "\x1b[36m"
	sgrDim    = "\x1b[2m"
)

// Status glyphs, hhauto T53 dictionary.
const (
	glyphSuccess = "✓"
	glyphError   = "✗"
	glyphWarn    = "?"
	glyphDim     = "·"
)

// byteUnits is the Russian byte-unit ladder; the last entry caps the loop.
var byteUnits = [...]string{"Б", "КБ", "МБ", "ГБ", "ТБ"}

// Console writes human-readable status lines to out (Info/Success/Warn/Dim
// and JSON) and errOut (Error). It is constructed with explicit inputs and
// never reads flags itself.
type Console struct {
	out     io.Writer
	errOut  io.Writer
	isTTY   bool
	noColor bool
}

// New constructs a Console writing to out and errOut. isTTY and noColor come
// from the caller; the rest of the color gate is read from the environment.
func New(out, errOut io.Writer, isTTY bool, noColor bool) *Console {
	return &Console{out: out, errOut: errOut, isTTY: isTTY, noColor: noColor}
}

// colorsEnabled reports the color gate:
// isTTY && !noColor && NO_COLOR=="" && TERM!="dumb".
// The environment is read per call so overrides apply after construction.
func (c *Console) colorsEnabled() bool {
	return c.isTTY && !c.noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// Infof writes an informational line to out (cyan).
func (c *Console) Infof(format string, args ...any) {
	c.write(c.out, sgrCyan, "", format, args...)
}

// Successf writes a success line to out (green, ✓).
func (c *Console) Successf(format string, args ...any) {
	c.write(c.out, sgrGreen, glyphSuccess, format, args...)
}

// Warnf writes a warning line to out (yellow, ?).
func (c *Console) Warnf(format string, args ...any) {
	c.write(c.out, sgrYellow, glyphWarn, format, args...)
}

// Errorf writes an error line to errOut (red, ✗).
func (c *Console) Errorf(format string, args ...any) {
	c.write(c.errOut, sgrRed, glyphError, format, args...)
}

// Dimf writes a secondary line to out (dim, ·): hashes, identifiers.
func (c *Console) Dimf(format string, args ...any) {
	c.write(c.out, sgrDim, glyphDim, format, args...)
}

// write emits one line: color, optional glyph, formatted message, reset,
// newline. With the color gate closed the bytes are identical without the
// escape sequences. Write errors are ignored: the console surface has no
// error return, and a broken pipe on stdout must not panic the process.
func (c *Console) write(w io.Writer, color, glyph, format string, args ...any) {
	var b strings.Builder

	colored := c.colorsEnabled()
	if colored {
		b.WriteString(color)
	}

	if glyph != "" {
		b.WriteString(glyph)
		b.WriteByte(' ')
	}

	fmt.Fprintf(&b, format, args...)

	if colored {
		b.WriteString(sgrReset)
	}

	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

// JSON writes exactly one compact JSON object per call, followed by a
// newline, to out. Colors are never applied to the JSON channel. A marshal
// failure is a programming error: it is reported loudly on errOut instead of
// writing partial JSON.
func (c *Console) JSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		_, _ = fmt.Fprintf(c.errOut, "%s Ошибка форматирования JSON: %v\n", glyphError, err)
		return
	}

	_, _ = c.out.Write(append(data, '\n'))
}

// IsTTY reports whether f is attached to a terminal.
func IsTTY(f *os.File) bool {
	if f == nil {
		return false
	}

	return term.IsTerminal(int(f.Fd()))
}

// FormatBytesRU renders a byte count in Russian units (Б/КБ/МБ/ГБ/ТБ) with
// one decimal digit and a comma separator, e.g. "1,5 ГБ". Negative values
// use the absolute magnitude — computed exactly in uint64, since -MinInt64
// overflows int64.
func FormatBytesRU(n int64) string {
	u := uint64(n)
	if n < 0 {
		u = uint64(-(n + 1)) + 1 // exact abs: MinInt64 → 2^63, no overflow
	}

	value := float64(u)

	unit := 0
	for value >= 1024 && unit < len(byteUnits)-1 {
		value /= 1024
		unit++
	}

	return strings.Replace(strconv.FormatFloat(value, 'f', 1, 64), ".", ",", 1) +
		" " + byteUnits[unit]
}
