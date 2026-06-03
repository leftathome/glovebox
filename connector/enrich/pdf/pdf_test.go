//go:build enrichruntime

package pdf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// TestEnrich_TextPDF runs the real pdftotext binary against a small PDF
// known to have extractable text. The fixture lives under testdata/. We
// copy it into the per-test temp dir as "content.raw" so the enricher
// resolves the primary-source naming branch (content.extracted.md), which
// mirrors how StagingItem.Commit() calls into Enrich() in production.
func TestEnrich_TextPDF(t *testing.T) {
	e := &Enricher{}
	out := t.TempDir()
	src := filepath.Join(out, "content.raw")
	mustCopy(t, filepath.Join("testdata", "text.pdf"), src)

	arts, err := e.Enrich(testCtx(t), src, staging.ItemMetadata{ContentType: "application/pdf"}, out)
	if err != nil {
		t.Fatalf("Enrich() text.pdf error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("Enrich() returned %d artifacts, want 1: %+v", len(arts), arts)
	}
	got := arts[0]
	if got.Producer != "pdf" {
		t.Errorf("Producer = %q, want %q", got.Producer, "pdf")
	}
	if got.Kind != "extracted-text" {
		t.Errorf("Kind = %q, want %q", got.Kind, "extracted-text")
	}
	if got.Filename != "content.extracted.md" {
		t.Errorf("Filename = %q, want %q", got.Filename, "content.extracted.md")
	}

	body, err := os.ReadFile(filepath.Join(out, got.Filename))
	if err != nil {
		t.Fatalf("read produced sidecar: %v", err)
	}
	if !strings.Contains(string(body), "Hello") {
		t.Errorf("extracted text does not contain expected substring 'Hello'; got:\n%s", body)
	}
}

// TestEnrich_AttachmentSourceNaming asserts the attachment naming
// convention: an "attachment-2-foo.pdf" source produces
// "content.attachment-2.extracted.md".
func TestEnrich_AttachmentSourceNaming(t *testing.T) {
	e := &Enricher{}

	// Copy the text fixture into the temp dir under an attachment-shaped
	// name so the enricher's outputDir == source dir behaves like the
	// real pipeline.
	out := t.TempDir()
	src := filepath.Join(out, "attachment-2-report.pdf")
	mustCopy(t, filepath.Join("testdata", "text.pdf"), src)

	arts, err := e.Enrich(testCtx(t), src, staging.ItemMetadata{ContentType: "application/pdf"}, out)
	if err != nil {
		t.Fatalf("Enrich() attachment error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("Enrich() returned %d artifacts, want 1", len(arts))
	}
	got := arts[0]
	if got.Filename != "content.attachment-2.extracted.md" {
		t.Errorf("attachment filename = %q, want %q", got.Filename, "content.attachment-2.extracted.md")
	}
}

// TestEnrich_ImageOnlyPDF: a PDF with no text layer. Per spec, this is
// NOT an error — the enricher tried, found nothing. Empty (or near-empty)
// output, no error.
func TestEnrich_ImageOnlyPDF(t *testing.T) {
	e := &Enricher{}
	src := mustExist(t, filepath.Join("testdata", "image-only.pdf"))

	out := t.TempDir()
	arts, err := e.Enrich(testCtx(t), src, staging.ItemMetadata{ContentType: "application/pdf"}, out)
	if err != nil {
		t.Fatalf("Enrich() image-only.pdf returned error: %v (expected nil; empty output is not a failure)", err)
	}
	if len(arts) != 1 {
		t.Fatalf("Enrich() returned %d artifacts, want 1", len(arts))
	}
	body, err := os.ReadFile(filepath.Join(out, arts[0].Filename))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	// pdftotext may emit a trailing form-feed for each page; the content
	// should not contain anything substantive.
	trimmed := strings.TrimSpace(strings.ReplaceAll(string(body), "\f", ""))
	if trimmed != "" {
		t.Errorf("image-only.pdf produced non-empty text: %q", trimmed)
	}
}

// TestEnrich_PasswordProtectedPDF asserts the enricher errors with a
// WHAT/CHECK/FIX-shaped message when the PDF is encrypted.
func TestEnrich_PasswordProtectedPDF(t *testing.T) {
	e := &Enricher{}
	src := mustExist(t, filepath.Join("testdata", "encrypted.pdf"))

	out := t.TempDir()
	_, err := e.Enrich(testCtx(t), src, staging.ItemMetadata{ContentType: "application/pdf"}, out)
	if err == nil {
		t.Fatalf("Enrich() encrypted.pdf returned nil error, want non-nil")
	}
	msg := err.Error()
	// Discipline: scope prefix + WHAT, then CHECK/FIX hints.
	if !strings.HasPrefix(msg, "enrich/pdf: ") {
		t.Errorf("error missing scope prefix 'enrich/pdf: '; got: %q", msg)
	}
	if !strings.Contains(msg, "CHECK") {
		t.Errorf("error missing CHECK hint; got: %q", msg)
	}
	if !strings.Contains(msg, "FIX") {
		t.Errorf("error missing FIX hint; got: %q", msg)
	}
	// The decryption-at-source FIX is documented in spec §5.1 / §8.
	if !strings.Contains(msg, "decrypt") {
		t.Errorf("error FIX hint should reference decrypt-at-source; got: %q", msg)
	}
}

// TestEnrich_CorruptPDF: truncated/garbage bytes. Must return error,
// must NOT panic. The error must carry the WHAT/CHECK/FIX shape.
func TestEnrich_CorruptPDF(t *testing.T) {
	e := &Enricher{}
	src := mustExist(t, filepath.Join("testdata", "corrupt.pdf"))

	out := t.TempDir()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enrich() panicked on corrupt input: %v", r)
		}
	}()

	_, err := e.Enrich(testCtx(t), src, staging.ItemMetadata{ContentType: "application/pdf"}, out)
	if err == nil {
		t.Fatalf("Enrich() corrupt.pdf returned nil error, want non-nil")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "enrich/pdf: ") {
		t.Errorf("error missing scope prefix 'enrich/pdf: '; got: %q", msg)
	}
	if !strings.Contains(msg, "CHECK") || !strings.Contains(msg, "FIX") {
		t.Errorf("error missing CHECK/FIX; got: %q", msg)
	}
}

// --- helpers ---

func mustExist(t *testing.T, p string) string {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("missing fixture %s: %v", p, err)
	}
	return p
}

func mustCopy(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}
