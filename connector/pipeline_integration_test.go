package connector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// fakeEnricher is a test-only Enricher used to exercise the pipeline
// hook wired into StagingItem.Commit(). It is intentionally duplicated
// from connector/enrich/pipeline_test.go's unexported fakeEnricher
// because that one is in package enrich and is not exported. Fakes are
// cheap; duplication is preferable to widening the enrich package API
// just for tests.
type fakeEnricher struct {
	name      string
	appliesFn func(meta staging.ItemMetadata, sourcePath string) bool
	enrichFn  func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error)

	appliesCalls int
	enrichCalls  int
}

func (f *fakeEnricher) Name() string { return f.name }

func (f *fakeEnricher) Applies(meta staging.ItemMetadata, sourcePath string) bool {
	f.appliesCalls++
	if f.appliesFn == nil {
		return true
	}
	return f.appliesFn(meta, sourcePath)
}

func (f *fakeEnricher) Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	f.enrichCalls++
	if f.enrichFn == nil {
		return nil, nil
	}
	return f.enrichFn(ctx, sourcePath, meta, outputDir)
}

// newTestWriter builds a StagingWriter with an isolated enrich.Registry
// so tests don't leak state through enrich.Default.
func newTestWriter(t *testing.T, stagingDir, name string) (*StagingWriter, *enrich.Registry) {
	t.Helper()
	w, err := NewStagingWriter(stagingDir, name)
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	reg := enrich.NewRegistry()
	w.SetEnrichRegistry(reg)
	return w, reg
}

func readMetadata(t *testing.T, stagingDir string) (string, staging.ItemMetadata) {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		var m staging.ItemMetadata
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		return filepath.Join(stagingDir, e.Name()), m
	}
	t.Fatal("no staging item directory found")
	return "", staging.ItemMetadata{}
}

func TestCommit_NoRegisteredEnrichers(t *testing.T) {
	stagingDir := t.TempDir()
	w, _ := newTestWriter(t, stagingDir, "test")

	item, err := w.NewItem(ItemOptions{
		Source:           "test",
		Sender:           "alice@example.com",
		Subject:          "Hello",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	itemDir, meta := readMetadata(t, stagingDir)

	// No sidecars beyond content.raw + metadata.json.
	entries, err := os.ReadDir(itemDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if !got["content.raw"] || !got["metadata.json"] {
		t.Errorf("missing required files; got entries %v", got)
	}
	if len(entries) != 2 {
		t.Errorf("expected exactly 2 entries (content.raw, metadata.json); got %v", got)
	}

	// Enrichments must be empty/nil.
	if len(meta.Enrichments) != 0 {
		t.Errorf("expected empty Enrichments, got %+v", meta.Enrichments)
	}
}

func TestCommit_OneEnricherOneArtifact(t *testing.T) {
	stagingDir := t.TempDir()
	w, reg := newTestWriter(t, stagingDir, "test")

	fe := &fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
			if err := os.WriteFile(filepath.Join(outputDir, "content.extracted.md"), []byte("FAKE"), 0644); err != nil {
				return nil, err
			}
			return []enrich.Artifact{{
				Filename: "content.extracted.md",
				Kind:     "extracted-text",
				Producer: "fake",
			}}, nil
		},
	}
	reg.Register(fe)

	item, err := w.NewItem(ItemOptions{
		Source:           "test",
		Sender:           "alice@example.com",
		Subject:          "Hello",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	itemDir, meta := readMetadata(t, stagingDir)

	// Sidecar file present with expected content.
	data, err := os.ReadFile(filepath.Join(itemDir, "content.extracted.md"))
	if err != nil {
		t.Fatalf("content.extracted.md missing: %v", err)
	}
	if string(data) != "FAKE" {
		t.Errorf("content.extracted.md = %q, want FAKE", string(data))
	}

	// Enrichments record present.
	if len(meta.Enrichments) != 1 {
		t.Fatalf("expected 1 enrichment record, got %d: %+v", len(meta.Enrichments), meta.Enrichments)
	}
	r := meta.Enrichments[0]
	if r.Producer != "fake" {
		t.Errorf("Producer = %q, want fake", r.Producer)
	}
	if r.Filename != "content.extracted.md" {
		t.Errorf("Filename = %q, want content.extracted.md", r.Filename)
	}
	if r.Status != "ok" {
		t.Errorf("Status = %q, want ok", r.Status)
	}
	if r.Source != "content.raw" {
		t.Errorf("Source = %q, want content.raw", r.Source)
	}
	if r.Error != "" {
		t.Errorf("Error must be empty for ok status, got %q", r.Error)
	}
}

func TestCommit_EnricherFailsButCommitSucceeds(t *testing.T) {
	stagingDir := t.TempDir()
	w, reg := newTestWriter(t, stagingDir, "test")

	fe := &fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
			return nil, errors.New("boom")
		},
	}
	reg.Register(fe)

	item, err := w.NewItem(ItemOptions{
		Source:           "test",
		Sender:           "alice@example.com",
		Subject:          "Hello",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit must NOT abort on enricher error: %v", err)
	}

	itemDir, meta := readMetadata(t, stagingDir)

	// Atomic rename happened (item is in staging/, not tmp/).
	if !strings.HasPrefix(itemDir, stagingDir+string(os.PathSeparator)) {
		t.Errorf("item dir %q is not under staging dir %q", itemDir, stagingDir)
	}

	// Marker file present.
	markerPath := filepath.Join(itemDir, "content.fake.error.md")
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("expected content.fake.error.md: %v", err)
	}
	body := string(markerData)
	for _, want := range []string{"enrich/fake", "extraction-failed", "boom", "CHECK", "FIX"} {
		if !strings.Contains(body, want) {
			t.Errorf("marker missing %q; full body:\n%s", want, body)
		}
	}

	// Metadata records the failure.
	if len(meta.Enrichments) != 1 {
		t.Fatalf("expected 1 enrichment record, got %d: %+v", len(meta.Enrichments), meta.Enrichments)
	}
	r := meta.Enrichments[0]
	if r.Status != "failed" {
		t.Errorf("Status = %q, want failed", r.Status)
	}
	if r.Error == "" {
		t.Errorf("Error must be populated for failed status, got empty")
	}
	if !strings.Contains(r.Error, "boom") {
		t.Errorf("Error = %q, want to contain boom", r.Error)
	}
	if r.Producer != "fake" {
		t.Errorf("Producer = %q, want fake", r.Producer)
	}
	if r.Source != "content.raw" {
		t.Errorf("Source = %q, want content.raw", r.Source)
	}
	if r.Filename != "" {
		t.Errorf("Filename must be empty for failed record, got %q", r.Filename)
	}
}

func TestCommit_MultipartItem(t *testing.T) {
	stagingDir := t.TempDir()
	w, reg := newTestWriter(t, stagingDir, "test")

	// Fake enricher applies to both content.raw and any attachment-*
	// file, producing a distinct artifact per source.
	fe := &fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
			base := filepath.Base(sourcePath)
			var out string
			if base == "content.raw" {
				out = "content.extracted.md"
			} else {
				out = base + ".extracted.md"
			}
			if err := os.WriteFile(filepath.Join(outputDir, out), []byte("FAKE for "+base), 0644); err != nil {
				return nil, err
			}
			return []enrich.Artifact{{
				Filename: out,
				Kind:     "extracted-text",
				Producer: "fake",
			}}, nil
		},
	}
	reg.Register(fe)

	item, err := w.NewItem(ItemOptions{
		Source:           "test",
		Sender:           "alice@example.com",
		Subject:          "with attachment",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("body")); err != nil {
		t.Fatal(err)
	}
	// Simulate an attachment by writing directly to the item directory.
	if err := os.WriteFile(filepath.Join(item.dir, "attachment-1-report.pdf"), []byte("PDFDATA"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	itemDir, meta := readMetadata(t, stagingDir)

	// Both sidecars exist.
	for _, f := range []string{"content.extracted.md", "attachment-1-report.pdf.extracted.md"} {
		if _, err := os.Stat(filepath.Join(itemDir, f)); err != nil {
			t.Errorf("missing sidecar %s: %v", f, err)
		}
	}

	if len(meta.Enrichments) != 2 {
		t.Fatalf("expected 2 enrichment records, got %d: %+v", len(meta.Enrichments), meta.Enrichments)
	}

	var fromPrimary, fromAttachment bool
	for _, r := range meta.Enrichments {
		if r.Producer != "fake" {
			t.Errorf("Producer = %q, want fake", r.Producer)
		}
		if r.Status != "ok" {
			t.Errorf("Status = %q, want ok", r.Status)
		}
		switch r.Source {
		case "content.raw":
			fromPrimary = true
			if r.Filename != "content.extracted.md" {
				t.Errorf("primary Filename = %q", r.Filename)
			}
		case "attachment-1-report.pdf":
			fromAttachment = true
			if r.Filename != "attachment-1-report.pdf.extracted.md" {
				t.Errorf("attachment Filename = %q", r.Filename)
			}
		default:
			t.Errorf("unexpected Source %q", r.Source)
		}
	}
	if !fromPrimary {
		t.Error("did not find primary enrichment record")
	}
	if !fromAttachment {
		t.Error("did not find attachment enrichment record")
	}
}
