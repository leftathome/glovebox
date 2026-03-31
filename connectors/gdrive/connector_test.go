package main

import (
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

// newTestConnector creates a GDriveConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, apiBase string, rules []connector.Rule) (*GDriveConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "gdrive")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &GDriveConnector{
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
		if e.IsDir() {
			count++
		}
	}
	return count
}

func TestPollFetchesChangesAndStages(t *testing.T) {
	changesResp := map[string]interface{}{
		"changes": []map[string]interface{}{
			{
				"fileId":  "file-1",
				"removed": false,
				"file": map[string]interface{}{
					"name":         "document.txt",
					"mimeType":     "text/plain",
					"modifiedTime": "2026-03-29T10:00:00Z",
				},
			},
			{
				"fileId":  "file-2",
				"removed": false,
				"file": map[string]interface{}{
					"name":         "spreadsheet.xlsx",
					"mimeType":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
					"modifiedTime": "2026-03-29T11:00:00Z",
				},
			},
		},
		"newStartPageToken": "456",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(changesResp)
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// Pre-seed a checkpoint so we skip the startPageToken fetch.
	if err := cp.Save("drive:changes", "123"); err != nil {
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

func TestCheckpointUsesNewStartPageToken(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"changes": []map[string]interface{}{
					{
						"fileId":  "file-1",
						"removed": false,
						"file": map[string]interface{}{
							"name":         "doc.txt",
							"mimeType":     "text/plain",
							"modifiedTime": "2026-03-29T10:00:00Z",
						},
					},
				},
				"newStartPageToken": "page-token-2",
			})
		} else {
			// Second call: verify the pageToken parameter.
			pt := r.URL.Query().Get("pageToken")
			if pt != "page-token-2" {
				t.Errorf("expected pageToken=page-token-2, got %q", pt)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"changes":           []map[string]interface{}{},
				"newStartPageToken": "page-token-3",
			})
		}
		callCount++
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, _, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// Pre-seed checkpoint for the first poll.
	if err := cp.Save("drive:changes", "page-token-1"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// First poll: gets changes and updates checkpoint to page-token-2.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// Second poll: should use page-token-2 as the pageToken.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls, got %d", callCount)
	}
}

func TestInitialTokenFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/drive/v3/changes/startPageToken" {
			json.NewEncoder(w).Encode(map[string]string{
				"startPageToken": "initial-token-99",
			})
			return
		}
		// Changes endpoint: verify it uses the initial token.
		pt := r.URL.Query().Get("pageToken")
		if pt != "initial-token-99" {
			t.Errorf("expected pageToken=initial-token-99, got %q", pt)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"changes":           []map[string]interface{}{},
			"newStartPageToken": "initial-token-100",
		})
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, _, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// No checkpoint saved -- should fetch startPageToken first.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Verify checkpoint was updated.
	val, ok := cp.Load("drive:changes")
	if !ok {
		t.Fatal("expected checkpoint to be set after initial poll")
	}
	if val != "initial-token-100" {
		t.Errorf("expected checkpoint initial-token-100, got %q", val)
	}
}

func TestIdentityFieldsInMetadata(t *testing.T) {
	changesResp := map[string]interface{}{
		"changes": []map[string]interface{}{
			{
				"fileId":  "file-99",
				"removed": false,
				"file": map[string]interface{}{
					"name":         "test.txt",
					"mimeType":     "text/plain",
					"modifiedTime": "2026-03-29T10:00:00Z",
				},
			},
		},
		"newStartPageToken": "500",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(changesResp)
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "drive:changes", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := cp.Save("drive:changes", "499"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
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
		if identity["provider"] != "google" {
			t.Errorf("expected identity provider 'google', got %v", identity["provider"])
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
	changesResp := map[string]interface{}{
		"changes": []map[string]interface{}{
			{
				"fileId":  "file-tag-1",
				"removed": false,
				"file": map[string]interface{}{
					"name":         "tagged.txt",
					"mimeType":     "text/plain",
					"modifiedTime": "2026-03-29T10:00:00Z",
				},
			},
		},
		"newStartPageToken": "600",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(changesResp)
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

	if err := cp.Save("drive:changes", "599"); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
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
