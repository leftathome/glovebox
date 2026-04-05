package main

import (
	"strings"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestConnector creates a OneDriveConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, apiBase string, rules []connector.Rule) (*OneDriveConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "onedrive")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &OneDriveConnector{
		config:       Config{},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-token"),
		apiBase:      apiBase,
	}

	return c, stagingDir, stateDir
}

func newCheckpoint(t *testing.T, stateDir string) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return cp
}

func countStagedItems(t *testing.T, stagingDir string) int {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

func TestPollFetchesChangesAndStages(t *testing.T) {
	deltaResp := map[string]interface{}{
		"value": []map[string]interface{}{
			{
				"id":                   "item-1",
				"name":                 "document.docx",
				"lastModifiedDateTime": "2026-03-29T10:00:00Z",
				"file":                 map[string]interface{}{},
			},
			{
				"id":                   "item-2",
				"name":                 "spreadsheet.xlsx",
				"lastModifiedDateTime": "2026-03-29T11:00:00Z",
				"file":                 map[string]interface{}{},
			},
		},
		"@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=next-delta-token",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(deltaResp)
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// Pre-seed a checkpoint so we use the stored deltaLink.
	if err := cp.Save(cpKey, srv.URL+"/v1.0/me/drive/root/delta?token=prev-token"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}
}

func TestCheckpointUsesDeltaLink(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"value": []map[string]interface{}{
					{
						"id":                   "item-1",
						"name":                 "doc.txt",
						"lastModifiedDateTime": "2026-03-29T10:00:00Z",
						"file":                 map[string]interface{}{},
					},
				},
				"@odata.deltaLink": "PLACEHOLDER_DELTA_LINK_2",
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"value":             []map[string]interface{}{},
				"@odata.deltaLink": "PLACEHOLDER_DELTA_LINK_3",
			})
		}
		callCount++
	}))
	defer srv.Close()

	// Replace placeholders with actual server URLs.
	// The connector should fetch the stored deltaLink directly on the second call.
	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, _, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// Pre-seed checkpoint with a deltaLink pointing to the test server.
	if err := cp.Save(cpKey, srv.URL+"/v1.0/me/drive/root/delta?token=initial"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// First poll: gets changes and updates checkpoint.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// Verify checkpoint was updated (the deltaLink from first response is not a real URL
	// since our mock returns a placeholder, but that is fine for verifying save behavior).
	val, ok := cp.Load(cpKey)
	if !ok {
		t.Fatal("expected checkpoint to be set after first poll")
	}
	if val != "PLACEHOLDER_DELTA_LINK_2" {
		t.Errorf("expected checkpoint PLACEHOLDER_DELTA_LINK_2, got %q", val)
	}

	if callCount != 1 {
		t.Errorf("expected 1 API call after first poll, got %d", callCount)
	}
}

func TestInitialDeltaFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Initial delta call -- should hit apiBase + deltaPath.
		if r.URL.Path == "/v1.0/me/drive/root/delta" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"value": []map[string]interface{}{
					{
						"id":                   "item-init",
						"name":                 "initial.txt",
						"lastModifiedDateTime": "2026-03-29T09:00:00Z",
						"file":                 map[string]interface{}{},
					},
				},
				"@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=after-init",
			})
			return
		}
		t.Errorf("unexpected request path: %s", r.URL.Path)
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, _, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// No checkpoint saved -- should fetch initial delta.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	val, ok := cp.Load(cpKey)
	if !ok {
		t.Fatal("expected checkpoint to be set after initial poll")
	}
	if val != "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=after-init" {
		t.Errorf("expected deltaLink checkpoint, got %q", val)
	}
}

func TestIdentityFieldsInMetadata(t *testing.T) {
	deltaResp := map[string]interface{}{
		"value": []map[string]interface{}{
			{
				"id":                   "item-99",
				"name":                 "test.txt",
				"lastModifiedDateTime": "2026-03-29T10:00:00Z",
				"file":                 map[string]interface{}{},
			},
		},
		"@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=500",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(deltaResp)
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := cp.Save(cpKey, srv.URL+"/v1.0/me/drive/root/delta?token=499"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found = true
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		identity, ok := meta["identity"].(map[string]interface{})
		if !ok {
			t.Fatal("expected identity object in metadata")
		}
		if identity["provider"] != "microsoft" {
			t.Errorf("expected identity provider 'microsoft', got %v", identity["provider"])
		}
		if identity["auth_method"] != "oauth" {
			t.Errorf("expected identity auth_method 'oauth', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	deltaResp := map[string]interface{}{
		"value": []map[string]interface{}{
			{
				"id":                   "item-tag-1",
				"name":                 "tagged.txt",
				"lastModifiedDateTime": "2026-03-29T10:00:00Z",
				"file":                 map[string]interface{}{},
			},
		},
		"@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=600",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(deltaResp)
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{
			Match:       "drive:changes",
			Destination: "test-agent",
			Tags:        map[string]string{"source_type": "cloud_storage", "priority": "normal"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := cp.Save(cpKey, srv.URL+"/v1.0/me/drive/root/delta?token=599"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found = true
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		tags, ok := meta["tags"].(map[string]interface{})
		if !ok {
			t.Fatal("expected tags object in metadata")
		}
		if tags["source_type"] != "cloud_storage" {
			t.Errorf("expected tag source_type 'cloud_storage', got %v", tags["source_type"])
		}
		if tags["priority"] != "normal" {
			t.Errorf("expected tag priority 'normal', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
