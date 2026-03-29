package staging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var scanAllowlist = []string{"messaging", "media", "calendar", "itinerary"}

func createStagingItem(t *testing.T, dir string, content string, metadata string) string {
	t.Helper()
	itemDir := filepath.Join(dir, "20260328-test-item")
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "content.raw"), []byte(content), 0644)
	os.WriteFile(filepath.Join(itemDir, "metadata.json"), []byte(metadata), 0644)
	return itemDir
}

const validMetadataJSON = `{
	"source": "email",
	"sender": "alice@example.com",
	"timestamp": "2026-03-28T12:00:00Z",
	"destination_agent": "messaging",
	"content_type": "text/plain"
}`

func TestReadStagingItem_Valid(t *testing.T) {
	dir := t.TempDir()
	itemDir := createStagingItem(t, dir, "hello world", validMetadataJSON)

	item, err := ReadStagingItem(itemDir, scanAllowlist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.Metadata.Source != "email" {
		t.Errorf("source = %q, want email", item.Metadata.Source)
	}
	if item.ContentPath != filepath.Join(itemDir, "content.raw") {
		t.Errorf("content path = %q", item.ContentPath)
	}
}

func TestReadStagingItem_MissingContent(t *testing.T) {
	dir := t.TempDir()
	itemDir := filepath.Join(dir, "item")
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "metadata.json"), []byte(validMetadataJSON), 0644)

	_, err := ReadStagingItem(itemDir, scanAllowlist)
	if err == nil {
		t.Fatal("expected error for missing content.raw")
	}
	if !strings.Contains(err.Error(), "content.raw") {
		t.Errorf("error should mention content.raw: %v", err)
	}
}

func TestReadStagingItem_MissingMetadata(t *testing.T) {
	dir := t.TempDir()
	itemDir := filepath.Join(dir, "item")
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "content.raw"), []byte("data"), 0644)

	_, err := ReadStagingItem(itemDir, scanAllowlist)
	if err == nil {
		t.Fatal("expected error for missing metadata.json")
	}
	if !strings.Contains(err.Error(), "metadata.json") {
		t.Errorf("error should mention metadata.json: %v", err)
	}
}

func TestReadStagingItem_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	itemDir := filepath.Join(dir, "item")
	os.MkdirAll(itemDir, 0755)

	_, err := ReadStagingItem(itemDir, scanAllowlist)
	if err == nil {
		t.Fatal("expected error for empty directory")
	}
}

func TestReadStagingItem_ValidationFailure(t *testing.T) {
	dir := t.TempDir()
	badMeta := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"hacking","content_type":"text/plain"}`
	itemDir := createStagingItem(t, dir, "data", badMeta)

	_, err := ReadStagingItem(itemDir, scanAllowlist)
	if err == nil {
		t.Fatal("expected error for invalid destination_agent")
	}
	if !strings.Contains(err.Error(), "validation") {
		t.Errorf("error should mention validation: %v", err)
	}
}
