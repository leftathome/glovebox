// Package archives -- recognizer-scanner content extraction (glovebox-9s60).
//
// Recognizer pre-extracts the OCR text for each scan and bundles it in the
// delivered tarball as a UTF-8 plaintext file at tar-root `ocr.txt` (locked
// decision 2026-06-15: recognizer owns extraction, glovebox owns rendering).
// After the tar is untarred into <finalize>/tree/, writeExtractedMarkdown
// renders that text into <finalize>/content.extracted.md so the openclaw
// operator agent can index and recall the scanned document. glovebox adds no
// PDF/A text-extraction dependency.
package archives

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrScanMissingOCR is returned when a recognizer-scan delivery has no
// (or an empty) tree/ocr.txt. A scan with no recallable text fails loudly
// rather than publishing an empty recall document.
var ErrScanMissingOCR = errors.New("recognizer-scan: missing or empty ocr.txt")

// renderExtractedMarkdown wraps the pre-extracted OCR text in a minimal
// markdown document. The OCR body is preserved verbatim (plain text is valid
// markdown); only a heading is prepended for legibility. Empty/whitespace-only
// input is rejected with ErrScanMissingOCR.
func renderExtractedMarkdown(ocr []byte) ([]byte, error) {
	if len(bytes.TrimSpace(ocr)) == 0 {
		return nil, ErrScanMissingOCR
	}
	var b bytes.Buffer
	b.WriteString("# Scanned document\n\n")
	b.Write(ocr)
	b.WriteString("\n")
	return b.Bytes(), nil
}

// writeExtractedMarkdown reads <finalizeDir>/tree/ocr.txt, renders it, and
// writes <finalizeDir>/content.extracted.md atomically at finalizeFileMode.
// A missing ocr.txt maps to ErrScanMissingOCR.
func writeExtractedMarkdown(finalizeDir string) error {
	ocrPath := filepath.Join(finalizeDir, "tree", "ocr.txt")
	ocr, err := os.ReadFile(ocrPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrScanMissingOCR
		}
		return fmt.Errorf("read ocr.txt: %w", err)
	}
	md, err := renderExtractedMarkdown(ocr)
	if err != nil {
		return err
	}
	mdPath := filepath.Join(finalizeDir, "content.extracted.md")
	if err := atomicWriteSibling(mdPath, md, finalizeFileMode); err != nil {
		return fmt.Errorf("write content.extracted.md: %w", err)
	}
	return nil
}
