// Handwritten minimal valid PDF fixtures (raw PDF syntax with computed
// xref offsets) plus the integration test against the real poppler
// binaries — skipped when poppler-utils is absent.
package pdfmeta

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildFixturePDF assembles a one-page PDF with an /Info dictionary
// (title/author/creationDate) and one line of page text, computing the
// xref offsets programmatically, and writes it to dir/name.
func buildFixturePDF(t *testing.T, dir, name, title, author, creationDate, pageText string) string {
	t.Helper()

	const textFont = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"
	info := fmt.Sprintf("<< /Title (%s) /Author (%s) /CreationDate (%s) >>", title, author, creationDate)
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET\n", pageText)

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		textFont,
		info,
	}

	var b strings.Builder
	b.WriteString("%PDF-1.4\n")

	offsets := make([]int, len(objects)+1) // 1-based object numbers
	for i, body := range objects {
		offsets[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xrefStart := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n", len(objects)+1)
	b.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, len(objects), xrefStart)

	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

// TestRealPopplerExtractDeriveAndText runs the whole pipeline against the
// system poppler-utils — the only test that exercises the real binaries.
func TestRealPopplerExtractDeriveAndText(t *testing.T) {
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("poppler-utils (pdfinfo) не найден — интеграционный тест пропущен")
	}
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("poppler-utils (pdftotext) не найден — интеграционный тест пропущен")
	}

	dir := t.TempDir()
	path := buildFixturePDF(t, dir, "01_fixture_author_topic.pdf",
		"Fixture Title", "Fixture Author", "D:20220304050607",
		"The quick brown fox jumps over the lazy dog. Second sentence for measurement.")

	info, err := Extract(path)
	require.NoError(t, err)
	require.Equal(t, "Fixture Title", info.Title)
	require.Equal(t, "Fixture Author", info.Author)
	require.Equal(t, 1, info.Pages)

	text, err := FirstPageText(path, 500)
	require.NoError(t, err)
	require.Contains(t, text, "quick brown fox")

	m, err := DeriveMeta(path, "fixtures")
	require.NoError(t, err)
	require.Equal(t, "Fixture Title", m.Title)
	require.Equal(t, "fixture, Fixture Author", m.Creator)
	require.Equal(t, "2022", m.Year)
	require.Equal(t, []string{"fixtures", "author topic"}, m.Subjects)
	require.Equal(t, "eng", m.Language)
	require.Contains(t, m.Description, "quick brown fox")

	id, err := Identifier("fixtures", "01_fixture_author_topic.pdf")
	require.NoError(t, err)
	require.Equal(t, "fixtures-1-fixture-author-topic", id)
}
