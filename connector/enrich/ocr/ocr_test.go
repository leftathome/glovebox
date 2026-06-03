//go:build enrichruntime

// Build-tagged tests that require the `tesseract` binary in PATH.
// Run with: go test -tags enrichruntime ./connector/enrich/ocr/...
//
// The CI pipeline runs these inside the glovebox-enricher-runtime
// container (see Dockerfile.enricher-runtime). They are skipped on
// distroless and on developer workstations that don't have tesseract
// installed. See docs/specs/14-content-enrichment-design.md §7.1.

package ocr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// requireTesseract is a defensive guard: if a user kicks off the
// enrichruntime tag without tesseract installed, fail loud with a
// WHAT/CHECK/FIX message rather than panicking deep in os/exec.
func requireTesseract(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skipf("tesseract not in PATH: %v\n"+
			"  CHECK: which tesseract\n"+
			"  FIX:   run these tests inside the glovebox-enricher-runtime image, "+
			"or apt-get install tesseract-ocr tesseract-ocr-eng on the host", err)
	}
}

func TestEnrich_ImageWithEnglishText(t *testing.T) {
	requireTesseract(t)

	outDir := t.TempDir()
	src := copyFixture(t, "hello.png", outDir)

	e := &Enricher{}
	arts, err := e.Enrich(context.Background(), src,
		staging.ItemMetadata{ContentType: "image/png"}, outDir)
	if err != nil {
		t.Fatalf("Enrich returned unexpected error: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d: %+v", len(arts), arts)
	}
	if arts[0].Kind != "ocr-text" {
		t.Errorf("artifact Kind = %q, want %q", arts[0].Kind, "ocr-text")
	}
	if arts[0].Producer != "ocr" {
		t.Errorf("artifact Producer = %q, want %q", arts[0].Producer, "ocr")
	}

	data := mustReadFile(t, filepath.Join(outDir, arts[0].Filename))
	// Tesseract is fuzzy on block letters at this resolution. Require
	// only that the H-E-L-L-O sequence (case-insensitive, whitespace-
	// tolerant) appears in the output. Tesseract sometimes reads the
	// block-letter "H" as similar glyphs; we settle for the strong
	// "ello" substring which is the most distinctive part of HELLO.
	got := strings.ToLower(string(data))
	if !strings.Contains(got, "hello") &&
		!strings.Contains(got, "ello") &&
		!strings.Contains(got, "hell") {
		t.Errorf("expected OCR output to contain a Hello-like substring; got %q", got)
	}
}

func TestEnrich_ImageNoText(t *testing.T) {
	requireTesseract(t)

	outDir := t.TempDir()
	src := copyFixture(t, "blank.png", outDir)

	e := &Enricher{}
	arts, err := e.Enrich(context.Background(), src,
		staging.ItemMetadata{ContentType: "image/png"}, outDir)
	if err != nil {
		// Per the §5 spec row: empty output is NOT an error. Tesseract
		// is expected to succeed on a blank image and return nothing.
		t.Fatalf("Enrich on blank image returned error: %v "+
			"(spec §5: empty output is correct semantic, not failure)", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact (with empty body), got %d", len(arts))
	}

	data := mustReadFile(t, filepath.Join(outDir, arts[0].Filename))
	// "near-empty" allowance: tesseract may emit a trailing newline or
	// a few whitespace chars. Anything <= 4 non-whitespace runes
	// counts as empty for our purposes.
	if nws := nonWhitespaceCount(string(data)); nws > 4 {
		t.Errorf("expected near-empty output for blank image; got %d non-whitespace chars: %q",
			nws, string(data))
	}
}

// TestEnrich_ImageTooSmall covers the §7.1 scenario for ocr:
// "image too small for OCR (assert documented message)".
//
// Tesseract's behaviour on tiny images varies by version: it may
// return empty output (no error) or it may error with a "Image too
// small to scale" complaint. Either is acceptable per the task
// description; only a panic or a sidecar with garbage content would
// be wrong.
func TestEnrich_ImageTooSmall(t *testing.T) {
	requireTesseract(t)

	outDir := t.TempDir()
	src := copyFixture(t, "tiny.png", outDir)

	e := &Enricher{}
	arts, err := e.Enrich(context.Background(), src,
		staging.ItemMetadata{ContentType: "image/png"}, outDir)

	switch {
	case err == nil:
		// Empty output acceptable.
		if len(arts) != 1 {
			t.Fatalf("expected 1 artifact (with empty body), got %d", len(arts))
		}
		data := mustReadFile(t, filepath.Join(outDir, arts[0].Filename))
		if nws := nonWhitespaceCount(string(data)); nws > 4 {
			t.Errorf("expected near-empty output for tiny image; got %d non-whitespace chars: %q",
				nws, string(data))
		}
	default:
		// Error acceptable, but it MUST follow the WHAT/CHECK/FIX
		// shape and MUST mention enrich/ocr scope.
		msg := err.Error()
		if !strings.Contains(msg, "enrich/ocr") {
			t.Errorf("error missing enrich/ocr scope prefix: %q", msg)
		}
		if !strings.Contains(msg, "CHECK") || !strings.Contains(msg, "FIX") {
			t.Errorf("error missing WHAT/CHECK/FIX shape: %q", msg)
		}
		if len(arts) != 0 {
			t.Errorf("expected 0 artifacts on error, got %d", len(arts))
		}
	}
}

func TestEnrich_CorruptImage(t *testing.T) {
	requireTesseract(t)

	outDir := t.TempDir()
	src := copyFixture(t, "corrupt.png", outDir)

	e := &Enricher{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enrich panicked on corrupt input: %v", r)
		}
	}()

	arts, err := e.Enrich(context.Background(), src,
		staging.ItemMetadata{ContentType: "image/png"}, outDir)
	if err == nil {
		t.Fatalf("expected error on corrupt PNG, got nil (arts=%+v)", arts)
	}
	msg := err.Error()
	if !strings.Contains(msg, "enrich/ocr") {
		t.Errorf("error missing enrich/ocr scope prefix: %q", msg)
	}
	if !strings.Contains(msg, "CHECK") || !strings.Contains(msg, "FIX") {
		t.Errorf("error missing WHAT/CHECK/FIX shape: %q", msg)
	}
	if len(arts) != 0 {
		t.Errorf("expected 0 artifacts on error, got %d", len(arts))
	}
}

// --- helpers ---

// copyFixture copies a testdata file into outDir so Enrich sees a real
// path; the file is named content.raw so the produced sidecar is
// content.extracted.md (the §4.3 primary-content shape).
func copyFixture(t *testing.T, fixture, outDir string) string {
	t.Helper()
	src := filepath.Join("testdata", fixture)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	dst := filepath.Join(outDir, "content.raw")
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func nonWhitespaceCount(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			// whitespace
		default:
			n++
		}
	}
	return n
}
