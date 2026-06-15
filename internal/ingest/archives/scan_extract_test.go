package archives

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderExtractedMarkdown_Basic(t *testing.T) {
	ocr := []byte("Invoice 2026\nTotal: $40")
	md, err := renderExtractedMarkdown(ocr)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(md)
	if !strings.Contains(s, "# Scanned document") {
		t.Errorf("missing heading, got: %q", s)
	}
	if !strings.Contains(s, "Invoice 2026\nTotal: $40") {
		t.Errorf("missing verbatim OCR body, got: %q", s)
	}
}

func TestRenderExtractedMarkdown_Empty(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("   \n\t ")} {
		if _, err := renderExtractedMarkdown(in); !errors.Is(err, ErrScanMissingOCR) {
			t.Errorf("renderExtractedMarkdown(%q) err=%v, want ErrScanMissingOCR", in, err)
		}
	}
}

func TestWriteExtractedMarkdown(t *testing.T) {
	finalizeDir := t.TempDir()
	treeDir := filepath.Join(finalizeDir, "tree")
	if err := os.MkdirAll(treeDir, 0o700); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(treeDir, "ocr.txt"), []byte("Hello scan"), 0o600); err != nil {
		t.Fatalf("write ocr: %v", err)
	}

	if err := writeExtractedMarkdown(finalizeDir); err != nil {
		t.Fatalf("writeExtractedMarkdown: %v", err)
	}

	mdPath := filepath.Join(finalizeDir, "content.extracted.md")
	fi, err := os.Stat(mdPath)
	if err != nil {
		t.Fatalf("stat content.extracted.md: %v", err)
	}
	if fi.Mode().Perm() != finalizeFileMode {
		t.Errorf("mode=%v, want %v", fi.Mode().Perm(), finalizeFileMode)
	}
	body, _ := os.ReadFile(mdPath)
	if !strings.Contains(string(body), "Hello scan") {
		t.Errorf("content.extracted.md missing OCR body, got: %q", string(body))
	}
}

func TestWriteExtractedMarkdown_MissingOCR(t *testing.T) {
	finalizeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(finalizeDir, "tree"), 0o700); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	// No ocr.txt present.
	if err := writeExtractedMarkdown(finalizeDir); !errors.Is(err, ErrScanMissingOCR) {
		t.Errorf("err=%v, want ErrScanMissingOCR", err)
	}
}
