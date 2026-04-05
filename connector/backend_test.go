package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackendNewItemProducesValidStagingItem creates a StagingItem via the
// StagingBackend interface, writes content, commits, and verifies the
// resulting files in the staging directory.
func TestBackendNewItemProducesValidStagingItem(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, err := NewStagingWriter(stagingDir, "backend-test")
	if err != nil {
		t.Fatal(err)
	}

	var backend StagingBackend = w

	item, err := backend.NewItem(ItemOptions{
		Source:           "test-source",
		Sender:           "sender@example.com",
		Subject:          "Backend test",
		Timestamp:        time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := item.WriteContent([]byte("hello from backend")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	// Verify committed item directory exists with both files.
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")

		contentData, cErr := os.ReadFile(contentPath)
		_, mErr := os.Stat(metaPath)
		if cErr == nil && mErr == nil {
			if string(contentData) != "hello from backend" {
				t.Errorf("content.raw = %q, want %q", contentData, "hello from backend")
			}
			found = true
		}
	}
	if !found {
		t.Error("staging dir should contain committed item with content.raw + metadata.json")
	}
}

// TestBackendSetConfigIdentity verifies that identity fields set via
// SetConfigIdentity on the StagingBackend interface flow through to
// committed metadata.
func TestBackendSetConfigIdentity(t *testing.T) {
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	os.MkdirAll(stagingDir, 0755)

	w, err := NewStagingWriter(stagingDir, "identity-test")
	if err != nil {
		t.Fatal(err)
	}

	var backend StagingBackend = w
	backend.SetConfigIdentity(&ConfigIdentity{
		Provider:   "github",
		Tenant:     "steve",
		AuthMethod: "oauth",
	})

	item, err := backend.NewItem(ItemOptions{
		Source:           "github",
		Sender:           "octocat",
		Timestamp:        time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		Identity: &Identity{
			AccountID: "steve@github",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := item.WriteContent([]byte("identity test content")); err != nil {
		t.Fatal(err)
	}
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

	if idMap["provider"] != "github" {
		t.Errorf("identity.provider = %v, want github (from config)", idMap["provider"])
	}
	if idMap["tenant"] != "steve" {
		t.Errorf("identity.tenant = %v, want steve (from config)", idMap["tenant"])
	}
	if idMap["auth_method"] != "oauth" {
		t.Errorf("identity.auth_method = %v, want oauth (from config)", idMap["auth_method"])
	}
	if idMap["account_id"] != "steve@github" {
		t.Errorf("identity.account_id = %v, want steve@github (from item)", idMap["account_id"])
	}
}
