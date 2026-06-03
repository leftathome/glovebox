package passthrough

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// writeSource is a test helper that drops `data` at `<dir>/<name>` and
// returns the absolute path. Fatals on error.
func writeSource(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatalf("write source %s: %v", p, err)
	}
	return p
}

func TestEnricher_Name(t *testing.T) {
	e := &Enricher{}
	if got := e.Name(); got != "passthrough" {
		t.Errorf("Name() = %q, want %q", got, "passthrough")
	}
}

func TestApplies_TextPlain(t *testing.T) {
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain"}
	if !e.Applies(meta, "/does/not/matter") {
		t.Error("Applies returned false for text/plain, want true")
	}
}

func TestApplies_TextPlainWithCharset(t *testing.T) {
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain; charset=utf-8"}
	if !e.Applies(meta, "/does/not/matter") {
		t.Error("Applies returned false for text/plain; charset=utf-8, want true")
	}
}

func TestApplies_TextMarkdown(t *testing.T) {
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/markdown"}
	if !e.Applies(meta, "/does/not/matter") {
		t.Error("Applies returned false for text/markdown, want true")
	}
}

func TestApplies_NotApplicablePDF(t *testing.T) {
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "application/pdf"}
	if e.Applies(meta, "/does/not/matter") {
		t.Error("Applies returned true for application/pdf, want false")
	}
}

func TestApplies_NotApplicableHTML(t *testing.T) {
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/html"}
	if e.Applies(meta, "/does/not/matter") {
		t.Error("Applies returned true for text/html, want false")
	}
}

func TestApplies_EmptyContentType_SniffsAsText(t *testing.T) {
	tmp := t.TempDir()
	src := writeSource(t, tmp, "content.raw", []byte("Hello, world.\nThis is ASCII text.\n"))

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: ""}
	if !e.Applies(meta, src) {
		t.Error("Applies returned false for empty ContentType + ASCII text source, want true")
	}
}

func TestApplies_EmptyContentType_SniffsAsBinary(t *testing.T) {
	tmp := t.TempDir()
	// PNG magic header bytes -- definitively binary.
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00}
	src := writeSource(t, tmp, "content.raw", pngHeader)

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: ""}
	if e.Applies(meta, src) {
		t.Error("Applies returned true for empty ContentType + binary source, want false")
	}
}

func TestApplies_EmptyContentType_NonexistentFile(t *testing.T) {
	// Defensive: if the source file is unreadable, we cannot sniff;
	// return false rather than panicking.
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: ""}
	if e.Applies(meta, "/nonexistent/path/that/does/not/exist") {
		t.Error("Applies returned true for unreadable source, want false")
	}
}

func TestEnrich_TextPlainIdentity_PrimaryContent(t *testing.T) {
	tmp := t.TempDir()
	input := []byte("Hello, world.")
	src := writeSource(t, tmp, "content.raw", input)

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain"}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d: %+v", len(arts), arts)
	}
	want := enrich.Artifact{
		Filename: "content.extracted.md",
		Kind:     "extracted-text",
		Producer: "passthrough",
	}
	if arts[0] != want {
		t.Errorf("artifact mismatch: got %+v, want %+v", arts[0], want)
	}

	outPath := filepath.Join(tmp, "content.extracted.md")
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output %s: %v", outPath, err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("output bytes mismatch: got %q, want %q", got, input)
	}
}

func TestEnrich_EmptyContent(t *testing.T) {
	tmp := t.TempDir()
	src := writeSource(t, tmp, "content.raw", []byte{})

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain"}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich returned error on empty input: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}

	outPath := filepath.Join(tmp, "content.extracted.md")
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat output %s: %v", outPath, err)
	}
	if info.Size() != 0 {
		t.Errorf("output size = %d, want 0 (empty input → empty output)", info.Size())
	}
}

func TestEnrich_UTF8NonASCII(t *testing.T) {
	tmp := t.TempDir()
	input := []byte("café — naïve\n日本語\n")
	src := writeSource(t, tmp, "content.raw", input)

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain; charset=utf-8"}
	if _, err := e.Enrich(context.Background(), src, meta, tmp); err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}

	outPath := filepath.Join(tmp, "content.extracted.md")
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("UTF-8 roundtrip mismatch:\n got %q\nwant %q", got, input)
	}
}

func TestEnrich_AttachmentSource(t *testing.T) {
	tmp := t.TempDir()
	input := []byte("attachment body text\n")
	// Attachment naming follows the staging convention
	// `attachment-<n>-<filename>`; basename-without-ext for
	// `attachment-2-report.txt` is `attachment-2-report`.
	src := writeSource(t, tmp, "attachment-2-report.txt", input)

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain"}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	wantName := "content.attachment-2.extracted.md"
	if arts[0].Filename != wantName {
		t.Errorf("Filename = %q, want %q", arts[0].Filename, wantName)
	}

	outPath := filepath.Join(tmp, wantName)
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output %s: %v", outPath, err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("output bytes mismatch: got %q, want %q", got, input)
	}
}

func TestEnrich_AttachmentSourceNoExtension(t *testing.T) {
	tmp := t.TempDir()
	input := []byte("no extension attachment\n")
	src := writeSource(t, tmp, "attachment-1-LICENSE", input)

	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain"}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}
	wantName := "content.attachment-1.extracted.md"
	if arts[0].Filename != wantName {
		t.Errorf("Filename = %q, want %q", arts[0].Filename, wantName)
	}
}

func TestEnrich_SourceUnreadable(t *testing.T) {
	tmp := t.TempDir()
	e := &Enricher{}
	meta := staging.ItemMetadata{ContentType: "text/plain"}
	_, err := e.Enrich(context.Background(), filepath.Join(tmp, "no-such-file"), meta, tmp)
	if err == nil {
		t.Error("Enrich returned nil error for unreadable source, want error")
	}
}

// TestInit_Registered confirms the package init() registered the
// enricher with enrich.Default. We probe via Default.ApplyAll: with a
// text/plain meta, a passthrough-registered Default must produce at
// least one artifact whose Producer is "passthrough".
func TestInit_Registered(t *testing.T) {
	tmp := t.TempDir()
	src := writeSource(t, tmp, "content.raw", []byte("init registration probe\n"))
	meta := staging.ItemMetadata{ContentType: "text/plain"}

	arts, errs := enrich.Default.ApplyAll(context.Background(), src, meta, tmp)
	if len(errs) != 0 {
		t.Fatalf("Default.ApplyAll returned errors: %+v", errs)
	}
	found := false
	for _, a := range arts {
		if a.Producer == "passthrough" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no passthrough artifact in Default.ApplyAll output: %+v", arts)
	}
}
