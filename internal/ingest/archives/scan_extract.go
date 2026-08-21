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

	"github.com/leftathome/glovebox/internal/engine"
)

// ErrScanMissingOCR is returned when a recognizer-scan delivery has no
// (or an empty) tree/ocr.txt. A scan with no recallable text fails loudly
// rather than publishing an empty recall document.
var ErrScanMissingOCR = errors.New("recognizer-scan: missing or empty ocr.txt")

// ErrExtractUnscanned is returned when the scanner lane is active but no
// ExtractScanner was configured. Publishing the recall document without
// scanning it would break the design invariant that no item reaches an
// agent workspace unscanned (spec 04 section 1.1), so this fails the
// finalize rather than publishing.
var ErrExtractUnscanned = errors.New("recognizer-scan: no scanner configured for extracted text")

// ExtractScanner scans agent-facing extracted text. *scan.Scanner
// satisfies it; the interface keeps this package free of a dependency on
// the scanner's construction.
type ExtractScanner interface {
	Scan(content []byte, contentType string) (engine.ScanResult, error)
}

// renderExtractedMarkdown wraps the pre-extracted OCR text in a minimal
// markdown document. The OCR body is preserved verbatim (plain text is valid
// markdown); only a heading is prepended for legibility. Empty/whitespace-only
// input is rejected with ErrScanMissingOCR.
//
// When the scan quarantines the text, the body is withheld entirely rather
// than inerted in place: this document exists to be indexed and recalled by
// the operator agent, so the safe form of a hostile scan is one that carries
// no payload at all. The raw text stays in tree/ocr.txt for a human to
// inspect, and the archive itself is unchanged.
func renderExtractedMarkdown(ocr []byte, verdict engine.ScanResult) ([]byte, error) {
	if len(bytes.TrimSpace(ocr)) == 0 {
		return nil, ErrScanMissingOCR
	}
	var b bytes.Buffer
	if verdict.Verdict == engine.VerdictQuarantine {
		b.WriteString("# Scanned document (withheld)\n\n")
		b.WriteString("The extracted text of this scan was quarantined by the content scanner ")
		b.WriteString("and is not reproduced here.\n\n")
		fmt.Fprintf(&b, "- score: %.2f\n", verdict.TotalScore)
		b.WriteString("- signals: ")
		for i, sig := range verdict.Signals {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(sig.Name)
		}
		b.WriteString("\n- raw text: tree/ocr.txt (unmodified, for human review)\n")
		return b.Bytes(), nil
	}
	b.WriteString("# Scanned document\n\n")
	b.Write(ocr)
	b.WriteString("\n")
	return b.Bytes(), nil
}

// writeExtractedMarkdown reads <finalizeDir>/tree/ocr.txt, renders it, and
// writes <finalizeDir>/content.extracted.md atomically at finalizeFileMode.
// A missing ocr.txt maps to ErrScanMissingOCR.
func writeExtractedMarkdown(finalizeDir string, scanner ExtractScanner) error {
	ocrPath := filepath.Join(finalizeDir, "tree", "ocr.txt")
	ocr, err := os.ReadFile(ocrPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrScanMissingOCR
		}
		return fmt.Errorf("read ocr.txt: %w", err)
	}
	// OCR text comes off a physical document an attacker can print, post
	// or mail. It was previously rendered straight into the operator
	// agent's recall document without ever passing the injection engine --
	// the connector lane's boundary did not extend to this lane.
	if scanner == nil {
		return ErrExtractUnscanned
	}
	verdict, err := scanner.Scan(ocr, "text/plain")
	if err != nil {
		return fmt.Errorf("scan extracted text: %w", err)
	}
	md, err := renderExtractedMarkdown(ocr, verdict)
	if err != nil {
		return err
	}
	mdPath := filepath.Join(finalizeDir, "content.extracted.md")
	if err := atomicWriteSibling(mdPath, md, finalizeFileMode); err != nil {
		return fmt.Errorf("write content.extracted.md: %w", err)
	}
	return nil
}
