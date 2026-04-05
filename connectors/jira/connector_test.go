package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestConnector creates a JiraConnector wired to temp directories and a
// test HTTP server URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, baseURL string, projects []string, rules []connector.Rule) (*JiraConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "jira")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &JiraConnector{
		config: Config{
			BaseURL:  baseURL,
			Projects: projects,
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		email:        "test@example.com",
		apiToken:     "test-token",
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}

	return c, stagingDir, stateDir
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

func newCheckpoint(t *testing.T, stateDir string) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return cp
}

// buildSearchResponse is a helper to build mock Jira search API responses.
func buildSearchResponse(issues []map[string]interface{}) []byte {
	resp := map[string]interface{}{
		"startAt":    0,
		"maxResults": 50,
		"total":      len(issues),
		"issues":     issues,
	}
	data, _ := json.Marshal(resp)
	return data
}

func makeIssue(key, summary, status, description, updated string) map[string]interface{} {
	return map[string]interface{}{
		"key": key,
		"fields": map[string]interface{}{
			"summary": summary,
			"status": map[string]interface{}{
				"name": status,
			},
			"description": description,
			"updated":     updated,
		},
	}
}

// TestPollFetchesIssues verifies that Poll fetches issues via JQL and writes
// them to the staging directory.
func TestPollFetchesIssues(t *testing.T) {
	issues := []map[string]interface{}{
		makeIssue("PROJ-1", "First issue", "Open", "Description one", "2024-01-15T10:00:00.000+0000"),
		makeIssue("PROJ-2", "Second issue", "In Progress", "Description two", "2024-01-15T11:00:00.000+0000"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request path and auth header.
		if r.URL.Path != "/rest/api/3/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		jql := r.URL.Query().Get("jql")
		if !strings.Contains(jql, "project = PROJ") {
			t.Errorf("expected JQL to contain project = PROJ, got: %s", jql)
		}

		// Verify basic auth header is present.
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Basic ") {
			t.Errorf("expected Basic auth header, got: %s", authHeader)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSearchResponse(issues))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "project:PROJ", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, []string{"PROJ"}, rules)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}

	// Verify checkpoint was saved.
	cpVal, ok := cp.Load("updated:PROJ")
	if !ok {
		t.Fatal("checkpoint not saved for project PROJ")
	}
	if cpVal != "2024-01-15T11:00:00.000+0000" {
		t.Errorf("expected checkpoint '2024-01-15T11:00:00.000+0000', got %q", cpVal)
	}
}

// TestCheckpointPreventsRefetch verifies that on a second poll with the same
// data and a checkpoint, no new items are staged.
func TestCheckpointPreventsRefetch(t *testing.T) {
	issues := []map[string]interface{}{
		makeIssue("PROJ-1", "First issue", "Open", "Desc one", "2024-01-15T10:00:00.000+0000"),
		makeIssue("PROJ-2", "Second issue", "Done", "Desc two", "2024-01-15T11:00:00.000+0000"),
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			// First poll returns issues.
			w.Write(buildSearchResponse(issues))
		} else {
			// Second poll: the JQL should contain the checkpoint timestamp.
			jql := r.URL.Query().Get("jql")
			if !strings.Contains(jql, "2024-01-15T11:00:00.000+0000") {
				t.Errorf("expected JQL to contain checkpoint timestamp, got: %s", jql)
			}
			// Return empty results (no new updates since checkpoint).
			w.Write(buildSearchResponse(nil))
		}
		callCount++
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "project:PROJ", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, []string{"PROJ"}, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: stages 2 items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll: should produce 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}
}

// TestIdentityInMetadata verifies that the staged items contain the correct
// identity fields in their metadata.
func TestIdentityInMetadata(t *testing.T) {
	issues := []map[string]interface{}{
		makeIssue("OPS-1", "Identity test", "Open", "Test desc", "2024-01-15T10:00:00.000+0000"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSearchResponse(issues))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "project:OPS", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, []string{"OPS"}, rules)
	cp := newCheckpoint(t, stateDir)

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
		if identity["provider"] != "jira" {
			t.Errorf("expected identity provider 'jira', got %v", identity["provider"])
		}
		if identity["auth_method"] != "api_key" {
			t.Errorf("expected identity auth_method 'api_key', got %v", identity["auth_method"])
		}
		if identity["account_id"] != "test@example.com" {
			t.Errorf("expected identity account_id 'test@example.com', got %v", identity["account_id"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

// TestRuleTagsInMetadata verifies that rule tags are included in the staged
// item metadata.
func TestRuleTagsInMetadata(t *testing.T) {
	issues := []map[string]interface{}{
		makeIssue("DEV-1", "Tags test", "Open", "Test desc", "2024-01-15T10:00:00.000+0000"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSearchResponse(issues))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{
			Match:       "project:DEV",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "devops", "priority": "high"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, srv.URL, []string{"DEV"}, rules)
	cp := newCheckpoint(t, stateDir)

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
		if tags["category"] != "devops" {
			t.Errorf("expected tag category 'devops', got %v", tags["category"])
		}
		if tags["priority"] != "high" {
			t.Errorf("expected tag priority 'high', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
