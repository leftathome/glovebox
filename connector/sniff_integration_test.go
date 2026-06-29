package connector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// ctDispatchEnricher applies only when the per-source ContentType equals
// wantCT. It records which sources it actually enriched so the test can
// assert per-attachment dispatch.
type ctDispatchEnricher struct {
	name   string
	wantCT string
}

func (e *ctDispatchEnricher) Name() string { return e.name }

func (e *ctDispatchEnricher) Applies(meta staging.ItemMetadata, _ string) bool {
	return meta.ContentType == e.wantCT
}

func (e *ctDispatchEnricher) Enrich(_ context.Context, sourcePath string, _ staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	base := filepath.Base(sourcePath)
	out := base + "." + e.name + ".md"
	if base == "content.raw" {
		out = "content." + e.name + ".md"
	}
	if err := os.WriteFile(filepath.Join(outputDir, out), []byte("x"), 0644); err != nil {
		return nil, err
	}
	return []enrich.Artifact{{Filename: out, Kind: "extracted-text", Producer: e.name}}, nil
}

// TestCommit_PerAttachmentContentTypeSniffing verifies that a multipart
// item whose primary ContentType is text/html still dispatches the PDF
// enricher to a PDF attachment. Before per-attachment sniffing, every
// source saw the item-level ContentType, so the html enricher fired on
// the PDF attachment and the pdf enricher never fired at all (spec 14
// §7.3 was unimplementable). See glovebox-afq4.17.
func TestCommit_PerAttachmentContentTypeSniffing(t *testing.T) {
	stagingDir := t.TempDir()
	w, reg := newTestWriter(t, stagingDir, "test")
	reg.Register(&ctDispatchEnricher{name: "html", wantCT: "text/html"})
	reg.Register(&ctDispatchEnricher{name: "pdf", wantCT: "application/pdf"})

	item, err := w.NewItem(ItemOptions{
		Source:           "test",
		Sender:           "alice@example.com",
		Subject:          "newsletter with attachment",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "messaging",
		ContentType:      "text/html",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("<html><body>hi</body></html>")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.dir, "attachment-1-report.pdf"), []byte("%PDF-1.4\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	itemDir, meta := readMetadata(t, stagingDir)

	// Expect exactly two ok records: html on the body, pdf on the
	// attachment. The html enricher must NOT fire on the PDF attachment,
	// and the pdf enricher must NOT fire on the html body.
	bySource := map[string]string{} // source -> producer
	for _, r := range meta.Enrichments {
		if r.Status != "ok" {
			t.Errorf("record %+v: status = %q, want ok", r, r.Status)
		}
		bySource[r.Source] = r.Producer
	}
	if len(meta.Enrichments) != 2 {
		t.Fatalf("expected 2 enrichment records, got %d: %+v", len(meta.Enrichments), meta.Enrichments)
	}
	if bySource["content.raw"] != "html" {
		t.Errorf("content.raw producer = %q, want html", bySource["content.raw"])
	}
	if bySource["attachment-1-report.pdf"] != "pdf" {
		t.Errorf("attachment producer = %q, want pdf (per-attachment sniffing)", bySource["attachment-1-report.pdf"])
	}

	// Corresponding sidecars exist; the wrong-enricher sidecars do not.
	mustExist := []string{"content.html.md", "attachment-1-report.pdf.pdf.md"}
	mustNotExist := []string{"content.pdf.md", "attachment-1-report.pdf.html.md"}
	for _, f := range mustExist {
		if _, err := os.Stat(filepath.Join(itemDir, f)); err != nil {
			t.Errorf("expected sidecar %s: %v", f, err)
		}
	}
	for _, f := range mustNotExist {
		if _, err := os.Stat(filepath.Join(itemDir, f)); err == nil {
			t.Errorf("unexpected sidecar %s (wrong enricher fired)", f)
		}
	}
}
