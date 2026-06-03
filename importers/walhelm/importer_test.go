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
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/ingest/archives"
	"github.com/leftathome/glovebox/internal/staging"
)

// testLogger returns a discard logger so test output stays clean.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// zeroDecision is the resume decision a fresh v1 import receives; walhelm does
// not implement resume so the value is unused but required by the signature.
func zeroDecision() importer.ResumeDecision {
	return importer.ResumeDecision{}
}

// newTestImporter builds a walhelmImporter wired to a real StagingWriter that
// commits items into a temp staging dir. The StagingWriter is the test double:
// because connector.StagingItem is a concrete type with unexported fields, a
// hand-rolled fake Backend cannot return a usable *StagingItem. Instead we let
// the real writer persist items to disk and inspect the committed metadata.json
// + content.raw files via collectStaged. A fresh enrich.Registry is injected so
// the enrichment pipeline does not leak global state between tests.
func newTestImporter(t *testing.T, rules []connector.Rule) (*walhelmImporter, string) {
	t.Helper()

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "walhelm")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	writer.SetEnrichRegistry(enrich.NewRegistry())

	fw := &connector.Framework{
		Name:    "walhelm",
		Logger:  testLogger(),
		Matcher: connector.NewRuleMatcher(rules),
		Backend: writer,
	}

	m := &walhelmImporter{
		fw:          fw,
		sourceName:  "walhelm-src",
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
// (the walhelm FinalizeReceipt) plus a tree/ subdir with the provided files.
// files maps tree-relative paths to their content bytes.
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

func walhelmReceipt() *archives.FinalizeReceipt {
	return &archives.FinalizeReceipt{
		ArchiveID:   "arch1",
		MediaType:   "archive/walhelm-export",
		DataSubject: "walhelm:9f2a",
		Audience:    []string{"subject"},
		Acquisition: &ingest.Identity{
			Provider:   "kp-wa",
			AuthMethod: "browser_session",
			AccountID:  "leftathome",
		},
	}
}

// --- happy path ---------------------------------------------------------------

func TestImport_StagesTreeEntries(t *testing.T) {
	emlBytes := []byte("From: a@example.com\r\nSubject: hi\r\n\r\nbody\r\n")
	jsonBytes := []byte(`{"result":"ok"}`)

	archiveDir := writeArchive(t, walhelmReceipt(), map[string][]byte{
		"message/a.eml": emlBytes,
		"lab/b.json":    jsonBytes,
	})

	m, stagingDir := newTestImporter(t, []connector.Rule{
		{Match: "*", Destination: "health-agent"},
	})

	if err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision()); err != nil {
		t.Fatalf("Import: %v", err)
	}

	items := collectStaged(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("staged %d items, want 2", len(items))
	}

	// Sorted by content_type: application/json (lab/b.json) then
	// message/rfc822 (message/a.eml).
	jsonItem := items[0]
	emlItem := items[1]

	if jsonItem.meta.ContentType != "application/json" {
		t.Errorf("item[0] ContentType = %q, want application/json", jsonItem.meta.ContentType)
	}
	if emlItem.meta.ContentType != "message/rfc822" {
		t.Errorf("item[1] ContentType = %q, want message/rfc822", emlItem.meta.ContentType)
	}

	if string(jsonItem.content) != string(jsonBytes) {
		t.Errorf("json content = %q, want %q", jsonItem.content, jsonBytes)
	}
	if string(emlItem.content) != string(emlBytes) {
		t.Errorf("eml content = %q, want %q", emlItem.content, emlBytes)
	}

	// Provenance is stamped from the receipt on every item.
	for _, it := range items {
		if it.meta.DataSubject != "walhelm:9f2a" {
			t.Errorf("DataSubject = %q, want walhelm:9f2a", it.meta.DataSubject)
		}
		if len(it.meta.Audience) != 1 || it.meta.Audience[0] != "subject" {
			t.Errorf("Audience = %v, want [subject]", it.meta.Audience)
		}
		if it.meta.Identity == nil {
			t.Fatalf("Identity nil, want acquisition identity")
		}
		if it.meta.Identity.Provider != "kp-wa" {
			t.Errorf("Identity.Provider = %q, want kp-wa", it.meta.Identity.Provider)
		}
		if it.meta.Identity.AuthMethod != "browser_session" {
			t.Errorf("Identity.AuthMethod = %q, want browser_session", it.meta.Identity.AuthMethod)
		}
		if it.meta.Identity.AccountID != "leftathome" {
			t.Errorf("Identity.AccountID = %q, want leftathome", it.meta.Identity.AccountID)
		}
		if it.meta.DestinationAgent != "health-agent" {
			t.Errorf("DestinationAgent = %q, want health-agent", it.meta.DestinationAgent)
		}
	}
}

// --- error: wrong media type --------------------------------------------------

func TestImport_RejectsWrongMediaType(t *testing.T) {
	r := walhelmReceipt()
	r.MediaType = "archive/mbox"
	archiveDir := writeArchive(t, r, map[string][]byte{"message/a.eml": []byte("x")})

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for wrong media type, got nil")
	}
}

// --- error: empty data subject ------------------------------------------------

func TestImport_RejectsEmptyDataSubject(t *testing.T) {
	r := walhelmReceipt()
	r.DataSubject = ""
	archiveDir := writeArchive(t, r, map[string][]byte{"message/a.eml": []byte("x")})

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for empty data subject, got nil")
	}
}

// --- error: missing tree/ -----------------------------------------------------

func TestImport_RejectsMissingTree(t *testing.T) {
	r := walhelmReceipt()
	// writeArchive with no files leaves tree/ absent.
	archiveDir := writeArchive(t, r, nil)

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for missing tree/, got nil")
	}
}

// --- error: empty tree/ -------------------------------------------------------

func TestImport_RejectsEmptyTree(t *testing.T) {
	r := walhelmReceipt()
	archiveDir := writeArchive(t, r, nil)
	// Create an empty tree/ dir (present but no regular files).
	if err := os.MkdirAll(filepath.Join(archiveDir, "tree"), 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for empty tree/, got nil")
	}
}

// --- error: missing metadata.json ---------------------------------------------

func TestImport_RejectsMissingMetadata(t *testing.T) {
	archiveDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(archiveDir, "tree"), 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}

	m, _ := newTestImporter(t, []connector.Rule{{Match: "*", Destination: "d"}})
	err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision())
	if err == nil {
		t.Fatal("expected error for missing metadata.json, got nil")
	}
}
