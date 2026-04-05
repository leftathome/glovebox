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
	var itemDir string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			itemDir = e.Name()
			break
		}
	}
	if itemDir == "" {
		t.Fatal("no committed item directory found in staging")
	}
	metaPath := filepath.Join(stagingDir, itemDir, "metadata.json")
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
	var itemDir string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			itemDir = e.Name()
			break
		}
	}
	if itemDir == "" {
		t.Fatal("no committed item directory found in staging")
	}
	data, _ := os.ReadFile(filepath.Join(stagingDir, itemDir, "content.raw"))
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

// readCommittedMetadata is a test helper that reads the first committed item's
// metadata.json from the staging directory and returns it as a raw map.
func readCommittedMetadata(t *testing.T, stagingDir string) map[string]any {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("unmarshal metadata.json: %v", err)
		}
		return meta
	}
	t.Fatal("no committed item found in staging dir")
	return nil
}

func TestStagingWriter_CommitWritesIdentity(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "github",
		Sender:           "octocat",
		Timestamp:        time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		Identity: &Identity{
			AccountID:  "steve@github",
			Provider:   "github",
			AuthMethod: "oauth",
			Scopes:     []string{"repo", "read:org"},
			Tenant:     "steve",
		},
	})
	item.WriteContent([]byte("pr body"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	meta := readCommittedMetadata(t, stagingDir)
	idRaw, ok := meta["identity"]
	if !ok {
		t.Fatal("metadata.json missing identity field")
	}
	idMap, ok := idRaw.(map[string]any)
	if !ok {
		t.Fatalf("identity is not an object: %T", idRaw)
	}
	if idMap["account_id"] != "steve@github" {
		t.Errorf("identity.account_id = %v, want steve@github", idMap["account_id"])
	}
	if idMap["provider"] != "github" {
		t.Errorf("identity.provider = %v, want github", idMap["provider"])
	}
	if idMap["auth_method"] != "oauth" {
		t.Errorf("identity.auth_method = %v, want oauth", idMap["auth_method"])
	}
	if idMap["tenant"] != "steve" {
		t.Errorf("identity.tenant = %v, want steve", idMap["tenant"])
	}
}

func TestStagingWriter_CommitWritesTags(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "github",
		Sender:           "octocat",
		Timestamp:        time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		Tags:             map[string]string{"team": "platform", "env": "production"},
	})
	item.WriteContent([]byte("content"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	meta := readCommittedMetadata(t, stagingDir)
	tagsRaw, ok := meta["tags"]
	if !ok {
		t.Fatal("metadata.json missing tags field")
	}
	tagsMap, ok := tagsRaw.(map[string]any)
	if !ok {
		t.Fatalf("tags is not an object: %T", tagsRaw)
	}
	if tagsMap["team"] != "platform" {
		t.Errorf("tags.team = %v, want platform", tagsMap["team"])
	}
	if tagsMap["env"] != "production" {
		t.Errorf("tags.env = %v, want production", tagsMap["env"])
	}
}

func TestStagingWriter_TagMerge_ItemWinsOverRuleTags(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "github",
		Sender:           "octocat",
		Timestamp:        time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		RuleTags:         map[string]string{"a": "1", "b": "2"},
		Tags:             map[string]string{"b": "override"},
	})
	item.WriteContent([]byte("content"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	meta := readCommittedMetadata(t, stagingDir)
	tagsMap, ok := meta["tags"].(map[string]any)
	if !ok {
		t.Fatal("metadata.json missing or invalid tags field")
	}
	if tagsMap["a"] != "1" {
		t.Errorf("tags.a = %v, want 1", tagsMap["a"])
	}
	if tagsMap["b"] != "override" {
		t.Errorf("tags.b = %v, want override (item tags should win)", tagsMap["b"])
	}
}

func TestStagingWriter_IdentityMerge_ConfigAndItem(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	w.SetConfigIdentity(&ConfigIdentity{
		Provider: "github",
		Tenant:   "steve",
	})
	item, _ := w.NewItem(ItemOptions{
		Source:           "github",
		Sender:           "octocat",
		Timestamp:        time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		Identity: &Identity{
			AccountID: "steve@github",
		},
	})
	item.WriteContent([]byte("content"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	meta := readCommittedMetadata(t, stagingDir)
	idMap, ok := meta["identity"].(map[string]any)
	if !ok {
		t.Fatal("metadata.json missing or invalid identity field")
	}
	if idMap["provider"] != "github" {
		t.Errorf("identity.provider = %v, want github (from config)", idMap["provider"])
	}
	if idMap["tenant"] != "steve" {
		t.Errorf("identity.tenant = %v, want steve (from config)", idMap["tenant"])
	}
	if idMap["account_id"] != "steve@github" {
		t.Errorf("identity.account_id = %v, want steve@github (from item)", idMap["account_id"])
	}
}

func TestStagingWriter_NoIdentityNoTags_OmittedFromMetadata(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, _ := NewStagingWriter(stagingDir, "test")
	item, _ := w.NewItem(ItemOptions{
		Source:           "rss",
		Sender:           "feed",
		Timestamp:        time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	item.WriteContent([]byte("content"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	meta := readCommittedMetadata(t, stagingDir)
	if _, ok := meta["identity"]; ok {
		t.Error("metadata.json should omit identity when not set")
	}
	if _, ok := meta["tags"]; ok {
		t.Error("metadata.json should omit tags when not set")
	}
}
