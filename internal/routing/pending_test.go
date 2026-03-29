package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/staging"
)

func orderedItem() staging.StagingItem {
	return staging.StagingItem{
		DirPath:     "/staging/20260328-1001-abc",
		ContentPath: "/staging/20260328-1001-abc/content.raw",
		Metadata: staging.ItemMetadata{
			Source:           "email",
			Sender:           "alice@example.com",
			Subject:          "Hello",
			Timestamp:        time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC),
			DestinationAgent: "messaging",
			ContentType:      "text/plain",
			Ordered:          true,
		},
	}
}

func TestWritePending_OrderedCreatesFile(t *testing.T) {
	dir := t.TempDir()
	item := orderedItem()
	if err := WritePending(item, dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pending.json") {
			found = true
		}
	}
	if !found {
		t.Error("expected .pending.json file for ordered item")
	}
}

func TestWritePending_UnorderedSkips(t *testing.T) {
	dir := t.TempDir()
	item := orderedItem()
	item.Metadata.Ordered = false
	if err := WritePending(item, dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pending.json") {
			t.Error("should not create .pending.json for unordered item")
		}
	}
}

func TestRemovePending(t *testing.T) {
	dir := t.TempDir()
	item := orderedItem()
	WritePending(item, dir)

	itemID := filepath.Base(item.DirPath)
	RemovePending(itemID, dir)

	path := filepath.Join(dir, PendingFilename(itemID))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("pending file should have been removed")
	}
}

func TestCleanStalePending(t *testing.T) {
	agentsDir := t.TempDir()
	inboxDir := filepath.Join(agentsDir, "messaging", "workspace", "inbox")
	os.MkdirAll(inboxDir, 0755)

	os.WriteFile(filepath.Join(inboxDir, "stale.pending.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(inboxDir, "real-content"), []byte("data"), 0644)

	CleanStalePending(agentsDir, []string{"messaging"})

	if _, err := os.Stat(filepath.Join(inboxDir, "stale.pending.json")); !os.IsNotExist(err) {
		t.Error("stale pending file should have been removed")
	}
	if _, err := os.Stat(filepath.Join(inboxDir, "real-content")); err != nil {
		t.Error("non-pending file should not have been removed")
	}
}

func TestWritePending_NoRawContent(t *testing.T) {
	dir := t.TempDir()
	item := orderedItem()
	WritePending(item, dir)

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pending.json") {
			data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if strings.Contains(string(data), "content.raw") {
				t.Error("pending file should not reference raw content")
			}
		}
	}
}
