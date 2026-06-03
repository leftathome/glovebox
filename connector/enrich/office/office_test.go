//go:build enrichruntime

// Tests in this file shell out to a real pandoc binary. They are gated
// behind the `enrichruntime` build tag so the normal `go test ./...`
// invocation in distroless / pandoc-less environments skips them. The
// CI matrix runs `go test -tags enrichruntime ./...` inside the
// `glovebox-enricher-runtime` image (which ships pandoc) to exercise
// these paths.
package office

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// requirePandoc skips the test if pandoc is not on PATH. This is a
// belt-and-suspenders check; the build tag is the primary gate.
func requirePandoc(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skipf("pandoc not in PATH: %v", err)
	}
}

// requirePandocFormat skips the test if the local pandoc does not
// support the given format as input (or, when which="output", as
// output). pptx-input lands in pandoc 3.0, xlsx-input in pandoc 3.5+;
// the spec target runtime image needs a recent pandoc. Skipping here
// keeps developer-laptop runs green without compromising the contract
// that the CI runtime image must pass these tests fully.
func requirePandocFormat(t *testing.T, which, format string) {
	t.Helper()
	flag := "--list-input-formats"
	if which == "output" {
		flag = "--list-output-formats"
	}
	out, err := exec.Command("pandoc", flag).CombinedOutput()
	if err != nil {
		t.Fatalf("pandoc %s: %v", flag, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == format {
			return
		}
	}
	t.Skipf("local pandoc lacks %s support for %q (needs newer pandoc; runtime image must include it)", which, format)
}

// makeDocx generates a tiny .docx from a markdown string using pandoc
// itself. Test fixtures stay self-consistent across pandoc versions
// without needing committed binary blobs. Returns the absolute path to
// the generated file.
func makeOOXML(t *testing.T, outDir, basename, toFormat, markdown string) string {
	t.Helper()
	src := filepath.Join(outDir, "in.md")
	if err := os.WriteFile(src, []byte(markdown), 0644); err != nil {
		t.Fatalf("write markdown source: %v", err)
	}
	dst := filepath.Join(outDir, basename)
	cmd := exec.Command("pandoc", "-f", "markdown", "-t", toFormat, "-o", dst, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pandoc generation failed (%s): %v\n%s", toFormat, err, out)
	}
	return dst
}

func TestEnrich_DocxSimple(t *testing.T) {
	requirePandoc(t)

	tmp := t.TempDir()
	src := makeOOXML(t, tmp, "content.raw", "docx",
		"# Hello\n\nThis is a paragraph about glovebox enrichment.\n")

	e := New()
	meta := staging.ItemMetadata{ContentType: ContentTypeDocx}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich(docx) error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}
	if arts[0].Filename != "content.extracted.md" {
		t.Errorf("artifact filename = %q, want content.extracted.md", arts[0].Filename)
	}
	if arts[0].Producer != Name {
		t.Errorf("artifact producer = %q, want %q", arts[0].Producer, Name)
	}
	if arts[0].Kind != "extracted-text" {
		t.Errorf("artifact kind = %q, want extracted-text", arts[0].Kind)
	}

	body, err := os.ReadFile(filepath.Join(tmp, arts[0].Filename))
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !strings.Contains(string(body), "glovebox enrichment") {
		t.Errorf("extracted markdown missing paragraph text:\n%s", body)
	}
}

func TestEnrich_XlsxTabular(t *testing.T) {
	requirePandoc(t)
	requirePandocFormat(t, "output", "xlsx")
	requirePandocFormat(t, "input", "xlsx")

	tmp := t.TempDir()
	// pandoc can render markdown tables into .xlsx; round-tripping
	// keeps the assertions tolerant to pandoc's exact cell layout.
	src := makeOOXML(t, tmp, "content.raw", "xlsx",
		"| Col1 | Col2 | Col3 |\n"+
			"|------|------|------|\n"+
			"| ZEBRA | TIGER | HORSE |\n"+
			"| LION | BEAR | WOLF |\n")

	e := New()
	meta := staging.ItemMetadata{ContentType: ContentTypeXlsx}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich(xlsx) error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}

	body, err := os.ReadFile(filepath.Join(tmp, arts[0].Filename))
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	for _, want := range []string{"ZEBRA", "TIGER", "HORSE", "LION", "BEAR", "WOLF"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("extracted xlsx missing cell %q:\n%s", want, body)
		}
	}
}

func TestEnrich_PptxText(t *testing.T) {
	requirePandoc(t)
	requirePandocFormat(t, "output", "pptx")
	requirePandocFormat(t, "input", "pptx")

	tmp := t.TempDir()
	src := makeOOXML(t, tmp, "content.raw", "pptx",
		"# UniqueTitleOne\n\nSlide one body.\n\n# UniqueTitleTwo\n\nSlide two body.\n")

	e := New()
	meta := staging.ItemMetadata{ContentType: ContentTypePptx}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich(pptx) error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}

	body, err := os.ReadFile(filepath.Join(tmp, arts[0].Filename))
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !strings.Contains(string(body), "UniqueTitleOne") {
		t.Errorf("extracted pptx missing first title:\n%s", body)
	}
	if !strings.Contains(string(body), "UniqueTitleTwo") {
		t.Errorf("extracted pptx missing second title:\n%s", body)
	}
}

func TestEnrich_LegacyDoc_ErrorMarkerPath(t *testing.T) {
	// Even with pandoc available, legacy .doc must short-circuit with
	// ErrLegacyFormat. This duplicates the untagged
	// TestEnrich_Legacy_ReturnsErrLegacyFormat to also pin the
	// behaviour under the enrichruntime tag (where the untagged test
	// is not compiled, since both files share the same package).
	e := New()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "content.raw")
	// CFB/OLE compound document magic.
	if err := os.WriteFile(src, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1, 0x00, 0x00}, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	meta := staging.ItemMetadata{ContentType: ContentTypeDoc}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err == nil {
		t.Fatal("Enrich(legacy .doc) returned nil error; want ErrLegacyFormat")
	}
	if !errors.Is(err, ErrLegacyFormat) {
		t.Errorf("Enrich(legacy .doc) error = %v; want errors.Is ErrLegacyFormat", err)
	}
	if len(arts) != 0 {
		t.Errorf("Enrich(legacy .doc) returned %d artifacts; want 0", len(arts))
	}
	if !strings.Contains(err.Error(), "OOXML") {
		t.Errorf("legacy error must hint at OOXML conversion: %q", err.Error())
	}
}

func TestEnrich_CorruptDocx_NoPanic(t *testing.T) {
	requirePandoc(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "content.raw")
	// Random non-zip bytes; pandoc must fail cleanly and we must
	// surface a non-nil error without panicking.
	if err := os.WriteFile(src, []byte("not a zip not a docx just garbage"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	meta := staging.ItemMetadata{ContentType: ContentTypeDocx}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enrich(corrupt) panicked: %v", r)
		}
	}()

	e := New()
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err == nil {
		t.Fatalf("Enrich(corrupt) returned nil error; want non-nil")
	}
	if len(arts) != 0 {
		t.Errorf("Enrich(corrupt) returned %d artifacts; want 0", len(arts))
	}
}
