package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestConnector creates an HNConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, cfg Config, baseURL string) (*HNConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "hackernews")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := make([]connector.Rule, 0, len(cfg.Feeds))
	for _, f := range cfg.Feeds {
		rules = append(rules, connector.Rule{
			Match:       "feed:" + f,
			Destination: "test-agent",
		})
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &HNConnector{
		config:       cfg,
		writer:       writer,
		matcher:      matcher,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		baseURL:      baseURL,
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

// mockHNServer creates a test HTTP server that serves HN API responses.
// storyIDs is the list returned for the feed endpoint.
// stories maps item ID to the JSON response body.
func mockHNServer(storyIDs []int, stories map[int]string) *httptest.Server {
	mux := http.NewServeMux()

	// Serve any feed endpoint (topstories.json, newstories.json, etc.)
	for _, feed := range []string{"top", "new", "best", "ask", "show"} {
		path := fmt.Sprintf("/v0/%sstories.json", feed)
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			data, _ := json.Marshal(storyIDs)
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
		})
	}

	// Serve individual item endpoints.
	mux.HandleFunc("/v0/item/", func(w http.ResponseWriter, r *http.Request) {
		// Extract ID from path: /v0/item/12345.json
		path := r.URL.Path
		path = strings.TrimPrefix(path, "/v0/item/")
		path = strings.TrimSuffix(path, ".json")
		id, err := strconv.Atoi(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body, ok := stories[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	return httptest.NewServer(mux)
}

func storyJSON(id int, title, url, text string, score int, kids []int) string {
	kidsJSON, _ := json.Marshal(kids)
	return fmt.Sprintf(
		`{"id":%d,"title":%q,"url":%q,"text":%q,"score":%d,"by":"testuser","time":1700000000,"descendants":%d,"kids":%s,"type":"story"}`,
		id, title, url, text, score, len(kids), string(kidsJSON),
	)
}

func commentJSON(id int, text, by string, parent int) string {
	return fmt.Sprintf(
		`{"id":%d,"text":%q,"by":%q,"time":1700000100,"kids":[],"type":"comment","parent":%d}`,
		id, text, by, parent,
	)
}

func TestPollFetchesStories(t *testing.T) {
	stories := map[int]string{
		101: storyJSON(101, "Story One", "https://example.com/1", "", 42, nil),
		102: storyJSON(102, "Story Two", "https://example.com/2", "Some text", 99, nil),
	}

	srv := mockHNServer([]int{102, 101}, stories)
	defer srv.Close()

	cfg := Config{
		Feeds:          []string{"top"},
		FollowComments: false,
		MaxComments:    10,
	}

	c, stagingDir, stateDir := newTestConnector(t, cfg, srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}

	// Checkpoint should be the highest ID seen.
	cpVal, ok := cp.Load("last:top")
	if !ok {
		t.Fatal("checkpoint not saved for feed 'top'")
	}
	if cpVal != "102" {
		t.Errorf("expected checkpoint '102', got %q", cpVal)
	}
}

func TestCommentFollowing(t *testing.T) {
	stories := map[int]string{
		201: storyJSON(201, "Story With Comments", "https://example.com/c", "", 10, []int{301, 302}),
		301: commentJSON(301, "First comment text", "commenter1", 201),
		302: commentJSON(302, "Second comment text", "commenter2", 201),
	}

	srv := mockHNServer([]int{201}, stories)
	defer srv.Close()

	cfg := Config{
		Feeds:          []string{"top"},
		FollowComments: true,
		MaxComments:    10,
	}

	c, stagingDir, stateDir := newTestConnector(t, cfg, srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 1 {
		t.Fatalf("expected 1 staged item, got %d", count)
	}

	// Check that comment text is included in the staged content.
	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		text := string(data)
		if !strings.Contains(text, "First comment text") {
			t.Errorf("expected 'First comment text' in content, got:\n%s", text)
		}
		if !strings.Contains(text, "Second comment text") {
			t.Errorf("expected 'Second comment text' in content, got:\n%s", text)
		}
	}
}

func TestCommentMaxLimit(t *testing.T) {
	kids := []int{401, 402, 403, 404, 405}
	stories := map[int]string{
		200: storyJSON(200, "Many Comments", "https://example.com/m", "", 5, kids),
		401: commentJSON(401, "Comment 1", "u1", 200),
		402: commentJSON(402, "Comment 2", "u2", 200),
		403: commentJSON(403, "Comment 3", "u3", 200),
		404: commentJSON(404, "Comment 4", "u4", 200),
		405: commentJSON(405, "Comment 5", "u5", 200),
	}

	srv := mockHNServer([]int{200}, stories)
	defer srv.Close()

	cfg := Config{
		Feeds:          []string{"top"},
		FollowComments: true,
		MaxComments:    2, // Only fetch 2 of 5 comments.
	}

	c, stagingDir, stateDir := newTestConnector(t, cfg, srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		text := string(data)
		// Should have comments 1 and 2 but not 3, 4, 5.
		if !strings.Contains(text, "Comment 1") {
			t.Errorf("expected 'Comment 1' in content")
		}
		if !strings.Contains(text, "Comment 2") {
			t.Errorf("expected 'Comment 2' in content")
		}
		if strings.Contains(text, "Comment 3") {
			t.Errorf("did not expect 'Comment 3' in content (max_comments=2)")
		}
	}
}

func TestCheckpointDedup(t *testing.T) {
	stories := map[int]string{
		501: storyJSON(501, "Story A", "https://example.com/a", "", 10, nil),
		502: storyJSON(502, "Story B", "https://example.com/b", "", 20, nil),
	}

	srv := mockHNServer([]int{502, 501}, stories)
	defer srv.Close()

	cfg := Config{
		Feeds:          []string{"top"},
		FollowComments: false,
		MaxComments:    10,
	}

	c, stagingDir, stateDir := newTestConnector(t, cfg, srv.URL)
	cp := newCheckpoint(t, stateDir)

	// First poll: both stories are new.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll: same stories, should produce 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}
}

func TestIdentityInMetadata(t *testing.T) {
	stories := map[int]string{
		601: storyJSON(601, "Identity Test", "https://example.com/id", "", 5, nil),
	}

	srv := mockHNServer([]int{601}, stories)
	defer srv.Close()

	cfg := Config{
		Feeds:          []string{"top"},
		FollowComments: false,
		MaxComments:    10,
	}

	c, stagingDir, stateDir := newTestConnector(t, cfg, srv.URL)
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
		if identity["provider"] != "hackernews" {
			t.Errorf("expected identity provider 'hackernews', got %v", identity["provider"])
		}
		if identity["auth_method"] != "none" {
			t.Errorf("expected identity auth_method 'none', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	stories := map[int]string{
		701: storyJSON(701, "Tags Test", "https://example.com/tags", "", 5, nil),
	}

	srv := mockHNServer([]int{701}, stories)
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "hackernews")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "feed:top",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "tech", "priority": "high"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	cfg := Config{
		Feeds:          []string{"top"},
		FollowComments: false,
		MaxComments:    10,
	}

	c := &HNConnector{
		config:       cfg,
		writer:       writer,
		matcher:      matcher,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		baseURL:      srv.URL,
	}
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
		if tags["category"] != "tech" {
			t.Errorf("expected tag category 'tech', got %v", tags["category"])
		}
		if tags["priority"] != "high" {
			t.Errorf("expected tag priority 'high', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
