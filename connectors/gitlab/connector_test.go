package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// gitlabEvent is a minimal representation of a GitLab event for test fixtures.
type gitlabEvent struct {
	ID         int    `json:"id"`
	ActionName string `json:"action_name"`
	TargetType string `json:"target_type"`
	TargetID   int    `json:"target_id"`
	CreatedAt  string `json:"created_at"`
}

func makeEvents(ids ...int) []gitlabEvent {
	events := make([]gitlabEvent, len(ids))
	for i, id := range ids {
		events[i] = gitlabEvent{
			ID:         id,
			ActionName: "pushed to",
			TargetType: "Project",
			TargetID:   1,
			CreatedAt:  fmt.Sprintf("2024-01-0%dT12:00:00.000Z", i+1),
		}
	}
	return events
}

func newTestGitLabConnector(t *testing.T, projects []ProjectConfig, baseURL string) (*GitLabConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "gitlab")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := make([]connector.Rule, 0, len(projects))
	for _, p := range projects {
		rules = append(rules, connector.Rule{
			Match:       "project:" + p.Path,
			Destination: "test-agent",
		})
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &GitLabConnector{
		config: Config{
			Projects: projects,
			BaseURL:  baseURL,
		},
		writer:      writer,
		matcher:     matcher,
		tokenSource: connector.NewStaticTokenSource("test-token"),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
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
		if e.IsDir() {
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

func TestPollFetchesEvents(t *testing.T) {
	events := makeEvents(101, 102, 103)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header
		if r.Header.Get("PRIVATE-TOKEN") != "test-token" {
			t.Errorf("expected PRIVATE-TOKEN header 'test-token', got %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(events)
		w.Write(data)
	}))
	defer srv.Close()

	projects := []ProjectConfig{{Path: "mygroup/myproject"}}
	c, stagingDir, stateDir := newTestGitLabConnector(t, projects, srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staged items, got %d", count)
	}

	// Checkpoint should be saved with the highest event ID.
	lastID, ok := cp.Load("event:mygroup/myproject")
	if !ok {
		t.Fatal("checkpoint not saved for project")
	}
	if lastID != "103" {
		t.Errorf("expected checkpoint '103', got %q", lastID)
	}
}

func TestCheckpointSkipsDuplicates(t *testing.T) {
	events := makeEvents(201, 202)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(events)
		w.Write(data)
	}))
	defer srv.Close()

	projects := []ProjectConfig{{Path: "group/proj"}}
	c, stagingDir, stateDir := newTestGitLabConnector(t, projects, srv.URL)
	cp := newCheckpoint(t, stateDir)

	// First poll: process all items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll with same events: should produce 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}
}

func TestIdentityInMetadata(t *testing.T) {
	events := makeEvents(301)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(events)
		w.Write(data)
	}))
	defer srv.Close()

	projects := []ProjectConfig{{Path: "org/repo"}}
	c, stagingDir, stateDir := newTestGitLabConnector(t, projects, srv.URL)
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
		if identity["provider"] != "gitlab" {
			t.Errorf("expected identity provider 'gitlab', got %v", identity["provider"])
		}
		if identity["auth_method"] != "pat" {
			t.Errorf("expected identity auth_method 'pat', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	events := makeEvents(401)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(events)
		w.Write(data)
	}))
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "gitlab")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "project:tagged/proj",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "devops", "priority": "high"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	projects := []ProjectConfig{{Path: "tagged/proj"}}
	c := &GitLabConnector{
		config: Config{
			Projects: projects,
			BaseURL:  srv.URL,
		},
		writer:      writer,
		matcher:     matcher,
		tokenSource: connector.NewStaticTokenSource("test-token"),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
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

func TestPagination(t *testing.T) {
	page1Events := makeEvents(501, 502)
	page2Events := makeEvents(503)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		if page == "" || page == "1" {
			w.Header().Set("X-Next-Page", "2")
			data, _ := json.Marshal(page1Events)
			w.Write(data)
		} else if page == "2" {
			// No X-Next-Page header means last page.
			data, _ := json.Marshal(page2Events)
			w.Write(data)
		}
	}))
	defer srv.Close()

	projects := []ProjectConfig{{Path: "paged/proj"}}
	c, stagingDir, stateDir := newTestGitLabConnector(t, projects, srv.URL)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staged items from 2 pages, got %d", count)
	}
}

func TestContentIsJSONEventBody(t *testing.T) {
	events := makeEvents(601)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(events)
		w.Write(data)
	}))
	defer srv.Close()

	projects := []ProjectConfig{{Path: "content/proj"}}
	c, stagingDir, stateDir := newTestGitLabConnector(t, projects, srv.URL)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}

		// Verify content is valid JSON.
		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("content is not valid JSON: %v", err)
		}

		// Verify it has the event ID.
		idVal, ok := parsed["id"]
		if !ok {
			t.Fatal("expected 'id' field in event JSON")
		}
		idNum, ok := idVal.(float64)
		if !ok {
			t.Fatalf("expected numeric id, got %T", idVal)
		}
		if int(idNum) != 601 {
			t.Errorf("expected event id 601, got %d", int(idNum))
		}

		// Verify metadata has content_type=application/json.
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		metaData, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(metaData, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}
		if meta["content_type"] != "application/json" {
			t.Errorf("expected content_type 'application/json', got %v", meta["content_type"])
		}
	}
}

func TestURLEncodesProjectPath(t *testing.T) {
	var requestedURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedURI = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	projects := []ProjectConfig{{Path: "my-group/my-project"}}
	c, _, stateDir := newTestGitLabConnector(t, projects, srv.URL)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	expected := "/api/v4/projects/my-group%2Fmy-project/events"
	if requestedURI != expected {
		t.Errorf("expected request URI %q, got %q", expected, requestedURI)
	}
}

