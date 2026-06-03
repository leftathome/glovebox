package enrich

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummarize_AllOk(t *testing.T) {
	primary := []Artifact{
		{Filename: "content.extracted.md", Kind: "extracted-text", Producer: "html"},
	}
	attachments := []Artifact{
		{Filename: "content.attachment-1.extracted.md", Kind: "extracted-text", Producer: "pdf"},
	}

	records := Summarize("content.raw", primary, nil, []string{"attachment-1-report.pdf"}, attachments, nil)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d: %+v", len(records), records)
	}

	// Primary record
	r0 := records[0]
	if r0.Producer != "html" {
		t.Errorf("records[0].Producer = %q, want html", r0.Producer)
	}
	if r0.Status != "ok" {
		t.Errorf("records[0].Status = %q, want ok", r0.Status)
	}
	if r0.Filename != "content.extracted.md" {
		t.Errorf("records[0].Filename = %q, want content.extracted.md", r0.Filename)
	}
	if r0.Kind != "extracted-text" {
		t.Errorf("records[0].Kind = %q, want extracted-text", r0.Kind)
	}
	if r0.Source != "content.raw" {
		t.Errorf("records[0].Source = %q, want content.raw", r0.Source)
	}
	if r0.Error != "" {
		t.Errorf("records[0].Error must be empty for ok status, got %q", r0.Error)
	}

	// Attachment record
	r1 := records[1]
	if r1.Producer != "pdf" {
		t.Errorf("records[1].Producer = %q, want pdf", r1.Producer)
	}
	if r1.Status != "ok" {
		t.Errorf("records[1].Status = %q, want ok", r1.Status)
	}
	if r1.Filename != "content.attachment-1.extracted.md" {
		t.Errorf("records[1].Filename = %q, want content.attachment-1.extracted.md", r1.Filename)
	}
	if r1.Source != "attachment-1-report.pdf" {
		t.Errorf("records[1].Source = %q, want attachment-1-report.pdf", r1.Source)
	}
}

func TestSummarize_MixedOkAndFail(t *testing.T) {
	primaryArts := []Artifact{
		{Filename: "content.extracted.md", Kind: "extracted-text", Producer: "html"},
	}
	primaryErrs := []EnricherError{
		{Producer: "pdf", Err: errors.New("primary boom")},
	}

	attachmentArts := []Artifact{
		{Filename: "content.attachment-1.extracted.md", Kind: "extracted-text", Producer: "pdf"},
	}
	attachmentErrs := []EnricherError{
		{Producer: "ocr", Err: errors.New("attachment boom")},
	}

	records := Summarize(
		"content.raw", primaryArts, primaryErrs,
		[]string{"attachment-1-report.pdf"}, attachmentArts, attachmentErrs,
	)

	if len(records) != 4 {
		t.Fatalf("expected 4 records, got %d: %+v", len(records), records)
	}

	var okCount, failCount int
	var primaryFailFound, attachmentFailFound bool
	for _, r := range records {
		switch r.Status {
		case "ok":
			okCount++
			if r.Filename == "" {
				t.Errorf("ok record missing Filename: %+v", r)
			}
			if r.Error != "" {
				t.Errorf("ok record must not have Error, got %q", r.Error)
			}
		case "failed":
			failCount++
			if r.Filename != "" {
				t.Errorf("failed record must not have Filename, got %q", r.Filename)
			}
			if r.Error == "" {
				t.Errorf("failed record must have Error populated: %+v", r)
			}
			if r.Producer == "pdf" && r.Source == "content.raw" {
				primaryFailFound = true
				if !strings.Contains(r.Error, "primary boom") {
					t.Errorf("primary fail Error = %q, expected to contain primary boom", r.Error)
				}
			}
			if r.Producer == "ocr" && r.Source == "attachment-1-report.pdf" {
				attachmentFailFound = true
				if !strings.Contains(r.Error, "attachment boom") {
					t.Errorf("attachment fail Error = %q, expected to contain attachment boom", r.Error)
				}
			}
		default:
			t.Errorf("unexpected Status %q", r.Status)
		}
	}
	if okCount != 2 {
		t.Errorf("ok count = %d, want 2", okCount)
	}
	if failCount != 2 {
		t.Errorf("failed count = %d, want 2", failCount)
	}
	if !primaryFailFound {
		t.Error("did not find expected primary failed record (producer=pdf, source=content.raw)")
	}
	if !attachmentFailFound {
		t.Error("did not find expected attachment failed record (producer=ocr, source=attachment-1-report.pdf)")
	}
}

func TestWriteErrorMarkers_WritesPerFailureFiles(t *testing.T) {
	dir := t.TempDir()
	errs := []EnricherError{
		{Producer: "pdf", Err: errors.New("pdftotext exit 2")},
		{Producer: "ocr", Err: errors.New("tesseract timed out")},
	}

	if err := WriteErrorMarkers(dir, errs, nil); err != nil {
		t.Fatalf("WriteErrorMarkers returned error: %v", err)
	}

	for _, c := range []struct {
		filename string
		mustHave []string
	}{
		{"content.pdf.error.md", []string{"enrich/pdf", "extraction-failed", "pdftotext exit 2", "CHECK", "FIX"}},
		{"content.ocr.error.md", []string{"enrich/ocr", "extraction-failed", "tesseract timed out", "CHECK", "FIX"}},
	} {
		data, err := os.ReadFile(filepath.Join(dir, c.filename))
		if err != nil {
			t.Errorf("expected marker file %s: %v", c.filename, err)
			continue
		}
		body := string(data)
		for _, sub := range c.mustHave {
			if !strings.Contains(body, sub) {
				t.Errorf("%s missing %q; full body:\n%s", c.filename, sub, body)
			}
		}
	}
}

func TestWriteErrorMarkers_AttachmentErrorsAlsoWritten(t *testing.T) {
	dir := t.TempDir()
	if err := WriteErrorMarkers(dir, nil, []EnricherError{
		{Producer: "office", Err: errors.New("pandoc unsupported format")},
	}); err != nil {
		t.Fatalf("WriteErrorMarkers returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "content.office.error.md"))
	if err != nil {
		t.Fatalf("expected content.office.error.md: %v", err)
	}
	if !strings.Contains(string(data), "pandoc unsupported format") {
		t.Errorf("marker missing error message: %s", data)
	}
}

func TestWriteErrorMarkers_EmptyDoesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := WriteErrorMarkers(dir, nil, nil); err != nil {
		t.Fatalf("WriteErrorMarkers returned error: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty dir after no-op WriteErrorMarkers, got %d entries", len(entries))
	}
}
