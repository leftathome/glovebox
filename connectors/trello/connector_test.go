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

// fakeTrelloActions returns a JSON array of Trello action objects.
func fakeTrelloActions(actions []map[string]interface{}) string {
	data, _ := json.Marshal(actions)
	return string(data)
}

func newTestConnector(t *testing.T, boards []BoardConfig, baseURL string) (*TrelloConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "trello")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := make([]connector.Rule, 0, len(boards))
	for _, b := range boards {
		rules = append(rules, connector.Rule{
			Match:       "board:" + b.Name,
			Destination: "test-agent",
		})
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &TrelloConnector{
		config: Config{
			Boards: boards,
		},
		apiKey:     "test-key",
		token:      "test-token",
		writer:     writer,
		matcher:    matcher,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
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

func TestPollFetchesBoardActions(t *testing.T) {
	actions := []map[string]interface{}{
		{
			"id":   "action-001",
			"type": "createCard",
			"date": "2024-06-15T10:00:00.000Z",
			"data": map[string]interface{}{
				"card": map[string]interface{}{
					"name": "New feature request",
				},
			},
			"memberCreator": map[string]interface{}{
				"fullName": "Alice",
			},
		},
		{
			"id":   "action-002",
			"type": "commentCard",
			"date": "2024-06-15T11:00:00.000Z",
			"data": map[string]interface{}{
				"card": map[string]interface{}{
					"name": "Bug fix",
				},
			},
			"memberCreator": map[string]interface{}{
				"fullName": "Bob",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth params are in query string, not headers
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("expected query param key=test-key, got %q", r.URL.Query().Get("key"))
		}
		if r.URL.Query().Get("token") != "test-token" {
			t.Errorf("expected query param token=test-token, got %q", r.URL.Query().Get("token"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeTrelloActions(actions)))
	}))
	defer srv.Close()

	boards := []BoardConfig{{ID: "board-123", Name: "dev"}}
	c, stagingDir, stateDir := newTestConnector(t, boards, srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}
}

func TestCheckpointSkipsProcessedActions(t *testing.T) {
	actions := []map[string]interface{}{
		{
			"id":   "action-001",
			"type": "createCard",
			"date": "2024-06-15T10:00:00.000Z",
			"data": map[string]interface{}{
				"card": map[string]interface{}{
					"name": "Card A",
				},
			},
			"memberCreator": map[string]interface{}{
				"fullName": "Alice",
			},
		},
		{
			"id":   "action-002",
			"type": "updateCard",
			"date": "2024-06-15T11:00:00.000Z",
			"data": map[string]interface{}{
				"card": map[string]interface{}{
					"name": "Card B",
				},
			},
			"memberCreator": map[string]interface{}{
				"fullName": "Bob",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeTrelloActions(actions)))
	}))
	defer srv.Close()

	boards := []BoardConfig{{ID: "board-456", Name: "ops"}}
	c, stagingDir, stateDir := newTestConnector(t, boards, srv.URL)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll: same actions, checkpoint should prevent duplicates.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}

	// Verify checkpoint key format
	cpVal, ok := cp.Load("action:board-456")
	if !ok {
		t.Fatal("checkpoint not saved for board")
	}
	// Trello returns newest first; after reversal, oldest first, so
	// last processed should be the newest action ID.
	if cpVal != "action-001" {
		t.Errorf("expected checkpoint 'action-001', got %q", cpVal)
	}
}

func TestIdentityInMetadata(t *testing.T) {
	actions := []map[string]interface{}{
		{
			"id":   "action-010",
			"type": "createCard",
			"date": "2024-06-15T10:00:00.000Z",
			"data": map[string]interface{}{
				"card": map[string]interface{}{
					"name": "Identity card",
				},
			},
			"memberCreator": map[string]interface{}{
				"fullName": "Charlie",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeTrelloActions(actions)))
	}))
	defer srv.Close()

	boards := []BoardConfig{{ID: "board-id-1", Name: "identity-board"}}
	c, stagingDir, stateDir := newTestConnector(t, boards, srv.URL)
	cp := newCheckpoint(t, stateDir)

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
		if identity["provider"] != "trello" {
			t.Errorf("expected identity provider 'trello', got %v", identity["provider"])
		}
		if identity["auth_method"] != "api_key" {
			t.Errorf("expected identity auth_method 'api_key', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	actions := []map[string]interface{}{
		{
			"id":   "action-020",
			"type": "moveCard",
			"date": "2024-06-15T12:00:00.000Z",
			"data": map[string]interface{}{
				"card": map[string]interface{}{
					"name": "Tagged card",
				},
			},
			"memberCreator": map[string]interface{}{
				"fullName": "Diana",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeTrelloActions(actions)))
	}))
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "trello")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "board:tag-board",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "project", "priority": "medium"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	boards := []BoardConfig{{ID: "board-tags", Name: "tag-board"}}
	c := &TrelloConnector{
		config: Config{
			Boards: boards,
		},
		apiKey:     "test-key",
		token:      "test-token",
		writer:     writer,
		matcher:    matcher,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    srv.URL,
	}
	cp := newCheckpoint(t, stateDir)

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
		if tags["category"] != "project" {
			t.Errorf("expected tag category 'project', got %v", tags["category"])
		}
		if tags["priority"] != "medium" {
			t.Errorf("expected tag priority 'medium', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
