package connector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStagingWriter_CommitProducesCorrectStructure(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, err := NewStagingWriter(stagingDir, "test-connector")
	if err != nil {
		t.Fatal(err)
	}
	item, err := w.NewItem(ItemOptions{
		Source:           "email",
		Sender:           "alice@example.com",
		Subject:          "Hello",
		Timestamp:        time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	item.WriteContent([]byte("email body"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		if _, err := os.Stat(contentPath); err == nil {
			if _, err := os.Stat(metaPath); err == nil {
				found = true
			}
		}
	}
	if !found {
		t.Error("staging dir should contain item with content.raw + metadata.json")
	}
}

func TestStagingWriter_MetadataSchema(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "imap",
		Sender:           "bob@test.com",
		Timestamp:        time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/html",
		Ordered:          true,
	})
	item.WriteContent([]byte("content"))
	item.Commit()

	entries, _ := os.ReadDir(stagingDir)
	metaPath := filepath.Join(stagingDir, entries[0].Name(), "metadata.json")
	data, _ := os.ReadFile(metaPath)

	var meta map[string]any
	json.Unmarshal(data, &meta)

	if meta["source"] != "imap" {
		t.Errorf("source = %v", meta["source"])
	}
	if meta["destination_agent"] != "messaging" {
		t.Errorf("destination_agent = %v", meta["destination_agent"])
	}
	if meta["ordered"] != true {
		t.Errorf("ordered = %v", meta["ordered"])
	}
}

func TestStagingWriter_ValidationRejectsOversizedFields(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "email",
		Sender:           strings.Repeat("a", 1025),
		Timestamp:        time.Now(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	item.WriteContent([]byte("content"))

	err := item.Commit()
	if err == nil {
		t.Error("should reject oversized sender")
	}
}

func TestStagingWriter_ValidationRejectsControlChars(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "email",
		Sender:           "bad\x00sender",
		Timestamp:        time.Now(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	item.WriteContent([]byte("content"))

	err := item.Commit()
	if err == nil {
		t.Error("should reject control chars in sender")
	}
}

func TestStagingWriter_ValidationRejectsEmptyDestination(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:      "email",
		Sender:      "a@b.com",
		Timestamp:   time.Now(),
		ContentType: "text/plain",
	})
	item.WriteContent([]byte("content"))

	err := item.Commit()
	if err == nil {
		t.Error("should reject empty destination_agent")
	}
}

func TestStagingWriter_OrphanCleanup(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "my-connector")

	orphanDir := filepath.Join(w.tmpDir, "orphan-item")
	os.MkdirAll(orphanDir, 0755)
	os.WriteFile(filepath.Join(orphanDir, "content.raw"), []byte("stale"), 0644)

	otherTmpDir := filepath.Join(stagingDir+"-tmp", "other-connector", "other-item")
	os.MkdirAll(otherTmpDir, 0755)

	w.CleanOrphans()

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("orphan should have been cleaned up")
	}
	if _, err := os.Stat(otherTmpDir); err != nil {
		t.Error("other connector's temp should NOT be cleaned")
	}
}

func TestStagingWriter_MultipleWriteContentAppends(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "email",
		Sender:           "a@b.com",
		Timestamp:        time.Now(),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	item.WriteContent([]byte("part1"))
	item.WriteContent([]byte("part2"))
	item.Commit()

	entries, _ := os.ReadDir(stagingDir)
	data, _ := os.ReadFile(filepath.Join(stagingDir, entries[0].Name(), "content.raw"))
	if string(data) != "part1part2" {
		t.Errorf("content = %q, want part1part2", data)
	}
}

func TestStagingWriter_NewStagingWriterReturnsError(t *testing.T) {
	_, err := NewStagingWriter("/proc/nonexistent/staging", "test")
	if err == nil {
		t.Error("should return error for unwritable directory")
	}
}
