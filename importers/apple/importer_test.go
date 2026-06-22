package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/importer"
	"github.com/leftathome/glovebox/internal/ingest/archives"
	"github.com/leftathome/glovebox/internal/staging"
)

// appleDataSubjectDefault is a synthetic example entity_id standing in for the
// operator-configured data_subject_default. Apple deliveries carry no
// producer-asserted subject, so the owner's principal is supplied by config
// (left empty in the PUBLIC config.json -- glovebox-0nzk) and wired into the
// StagingWriter via SetConfigDataSubject; this test sets it explicitly.
const appleDataSubjectDefault = "e_111111"

// testLogger returns a discard logger so test output stays clean.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// zeroDecision is the resume decision a fresh v1 import receives; apple does
// not implement resume so the value is unused but required by the signature.
func zeroDecision() importer.ResumeDecision {
	return importer.ResumeDecision{}
}

// newTestImporter builds an appleImporter wired to a real StagingWriter that
// commits items into a temp staging dir. The config-level data_subject /
// audience defaults are set on the writer to mirror what
// connector.NewFramework wires from config.json (SetConfigDataSubject /
// SetConfigAudience). A fresh enrich.Registry is injected so the enrichment
// pipeline does not leak global state between tests.
func newTestImporter(t *testing.T, rules []connector.Rule) (*appleImporter, string) {
	t.Helper()

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "apple")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	writer.SetEnrichRegistry(enrich.NewRegistry())
	writer.SetConfigDataSubject(appleDataSubjectDefault)
	writer.SetConfigAudience([]string{"subject"})

	fw := &connector.Framework{
		Name:    "apple",
		Logger:  testLogger(),
		Matcher: connector.NewRuleMatcher(rules),
		Backend: writer,
	}

	m := &appleImporter{
		fw:          fw,
		sourceName:  "apple-src",
		concurrency: 2,
	}
	return m, stagingDir
}

// stagedItem is a committed staging item read back from disk for assertions.
type stagedItem struct {
	meta    staging.ItemMetadata
	content []byte
}

// collectStaged reads every committed item under stagingDir (each item is a
// directory containing metadata.json + content.raw). The .tmp subtree is
// skipped. Results are sorted by content_type for deterministic assertions.
func collectStaged(t *testing.T, stagingDir string) []stagedItem {
	t.Helper()

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}

	var items []stagedItem
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".tmp" {
			continue
		}
		itemDir := filepath.Join(stagingDir, e.Name())
		metaBytes, err := os.ReadFile(filepath.Join(itemDir, "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata.json in %s: %v", itemDir, err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			t.Fatalf("unmarshal metadata.json in %s: %v", itemDir, err)
		}
		content, err := os.ReadFile(filepath.Join(itemDir, "content.raw"))
		if err != nil {
			t.Fatalf("read content.raw in %s: %v", itemDir, err)
		}
		items = append(items, stagedItem{meta: meta, content: content})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].meta.ContentType < items[j].meta.ContentType
	})
	return items
}

// writeArchive builds a synthetic finalized archive directory: metadata.json
// (the FinalizeReceipt) plus a tree/ subdir with the provided files. files
// maps tree-relative paths to their content bytes.
func writeArchive(t *testing.T, receipt *archives.FinalizeReceipt, files map[string][]byte) string {
	t.Helper()

	archiveDir := t.TempDir()

	metaBytes, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, "metadata.json"), metaBytes, 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	for rel, content := range files {
		full := filepath.Join(archiveDir, "tree", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write tree file %s: %v", rel, err)
		}
	}
	return archiveDir
}

// appleReceipt returns a finalized Apple-export receipt: a generic tarball
// whose matcher_id claims the apple/ namespace. Note there is NO data_subject
// -- Apple deliveries carry none; the subject comes from config.
func appleReceipt() *archives.FinalizeReceipt {
	return &archives.FinalizeReceipt{
		ArchiveID: "arch-apple-1",
		MediaType: "archive/generic-tarball",
		MatcherID: "apple/media-services",
	}
}

// --- happy path ---------------------------------------------------------------

func TestImport_StagesTreeEntries(t *testing.T) {
	csvBytes := []byte("track,artist\nfoo,bar\n")
	jsonBytes := []byte(`{"result":"ok"}`)

	archiveDir := writeArchive(t, appleReceipt(), map[string][]byte{
		"media-services/plays.csv": csvBytes,
		"media-services/meta.json": jsonBytes,
	})

	m, stagingDir := newTestImporter(t, []connector.Rule{
		{Match: "*", Destination: "media-agent"},
	})

	if err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision()); err != nil {
		t.Fatalf("Import: %v", err)
	}

	items := collectStaged(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("staged %d items, want 2", len(items))
	}

	// Sorted by content_type: application/json (meta.json) then text/csv
	// (plays.csv).
	jsonItem := items[0]
	csvItem := items[1]

	if jsonItem.meta.ContentType != "application/json" {
		t.Errorf("item[0] ContentType = %q, want application/json", jsonItem.meta.ContentType)
	}
	if csvItem.meta.ContentType != "text/csv" {
		t.Errorf("item[1] ContentType = %q, want text/csv", csvItem.meta.ContentType)
	}

	if string(jsonItem.content) != string(jsonBytes) {
		t.Errorf("json content = %q, want %q", jsonItem.content, jsonBytes)
	}
	if string(csvItem.content) != string(csvBytes) {
		t.Errorf("csv content = %q, want %q", csvItem.content, csvBytes)
	}

	// Provenance: data_subject comes from the config default (e_111111), the
	// apple_bucket tag carries the matcher_id, and the destination comes from
	// the matched rule.
	for _, it := range items {
		if it.meta.DataSubject != appleDataSubjectDefault {
			t.Errorf("DataSubject = %q, want %q", it.meta.DataSubject, appleDataSubjectDefault)
		}
		if it.meta.Tags["apple_bucket"] != "apple/media-services" {
			t.Errorf("tags[apple_bucket] = %q, want apple/media-services", it.meta.Tags["apple_bucket"])
		}
		if it.meta.DestinationAgent != "media-agent" {
			t.Errorf("DestinationAgent = %q, want media-agent", it.meta.DestinationAgent)
		}
	}
}

// --- error: matcher_id not in apple/ namespace --------------------------------

func TestImport_RejectsNonAppleMatcher(t *testing.T) {
	r := appleReceipt()
	r.MatcherID = "meta/posts"
	archiveDir := writeArchive(t, r, map[string][]byte{"posts/a.json": []byte("x")})

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for non-apple matcher_id, got nil")
	}
}

// --- error: wrong media type --------------------------------------------------

func TestImport_RejectsWrongMediaType(t *testing.T) {
	r := appleReceipt()
	r.MediaType = "archive/walhelm-export"
	archiveDir := writeArchive(t, r, map[string][]byte{"media-services/a.csv": []byte("x")})

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for wrong media type, got nil")
	}
}
