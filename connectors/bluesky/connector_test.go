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

// mockBlueskyServer creates an httptest server that handles both the
// createSession and getAuthorFeed XRPC endpoints. It returns the server
// and counters for how many times each endpoint was called.
func mockBlueskyServer(t *testing.T, posts []map[string]interface{}) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body struct {
			Identifier string `json:"identifier"`
			Password   string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp := map[string]interface{}{
			"did":         "did:plc:testuser123",
			"handle":      body.Identifier,
			"accessJwt":   "test-access-jwt",
			"refreshJwt":  "test-refresh-jwt",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/xrpc/app.bsky.feed.getAuthorFeed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-access-jwt" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		feed := make([]map[string]interface{}, 0, len(posts))
		for _, p := range posts {
			feed = append(feed, map[string]interface{}{"post": p})
		}
		resp := map[string]interface{}{
			"feed": feed,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	return httptest.NewServer(mux)
}

func newTestBlueskyConnector(t *testing.T, serviceURL string) (*BlueskyConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "bluesky")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "feed:timeline",
			Destination: "test-agent",
			Tags:        map[string]string{"source_type": "social", "platform": "bluesky"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	cfg := Config{}
	cfg.Service = serviceURL
	cfg.FeedURIs = []string{"at://did:plc:testuser123/app.bsky.feed.getAuthorFeed"}

	c := &BlueskyConnector{
		config:       cfg,
		identifier:   "testuser.bsky.social",
		appPassword:  "test-app-password",
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
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

func readMetadata(t *testing.T, stagingDir string) []map[string]interface{} {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var metas []map[string]interface{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}
		metas = append(metas, meta)
	}
	return metas
}

// Test 1: Poll creates session, fetches feed, and stages posts.
func TestPollCreatesSessionAndStagesPosts(t *testing.T) {
	posts := []map[string]interface{}{
		{
			"uri":       "at://did:plc:testuser123/app.bsky.feed.post/post2",
			"cid":       "cid-post-2",
			"author":    map[string]interface{}{"did": "did:plc:testuser123", "handle": "testuser.bsky.social"},
			"record":    map[string]interface{}{"text": "Second post", "createdAt": "2024-01-02T12:00:00Z"},
			"indexedAt": "2024-01-02T12:00:00Z",
		},
		{
			"uri":       "at://did:plc:testuser123/app.bsky.feed.post/post1",
			"cid":       "cid-post-1",
			"author":    map[string]interface{}{"did": "did:plc:testuser123", "handle": "testuser.bsky.social"},
			"record":    map[string]interface{}{"text": "First post", "createdAt": "2024-01-01T12:00:00Z"},
			"indexedAt": "2024-01-01T12:00:00Z",
		},
	}

	srv := mockBlueskyServer(t, posts)
	defer srv.Close()

	c, stagingDir, stateDir := newTestBlueskyConnector(t, srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}

	// Checkpoint should be the last (newest) post CID after processing.
	lastCID, ok := cp.Load("post:latest")
	if !ok {
		t.Fatal("checkpoint not saved")
	}
	if lastCID != "cid-post-2" {
		t.Errorf("expected checkpoint 'cid-post-2', got %q", lastCID)
	}
}

// Test 2: Checkpoint skips duplicates on second poll.
func TestCheckpointSkipsDuplicates(t *testing.T) {
	posts := []map[string]interface{}{
		{
			"uri":       "at://did:plc:testuser123/app.bsky.feed.post/post2",
			"cid":       "cid-post-2",
			"author":    map[string]interface{}{"did": "did:plc:testuser123", "handle": "testuser.bsky.social"},
			"record":    map[string]interface{}{"text": "Second post", "createdAt": "2024-01-02T12:00:00Z"},
			"indexedAt": "2024-01-02T12:00:00Z",
		},
		{
			"uri":       "at://did:plc:testuser123/app.bsky.feed.post/post1",
			"cid":       "cid-post-1",
			"author":    map[string]interface{}{"did": "did:plc:testuser123", "handle": "testuser.bsky.social"},
			"record":    map[string]interface{}{"text": "First post", "createdAt": "2024-01-01T12:00:00Z"},
			"indexedAt": "2024-01-01T12:00:00Z",
		},
	}

	srv := mockBlueskyServer(t, posts)
	defer srv.Close()

	c, stagingDir, stateDir := newTestBlueskyConnector(t, srv.URL)
	cp := newCheckpoint(t, stateDir)

	// First poll: stages all posts.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll with same feed: should produce 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll, got %d", secondCount)
	}
}

// Test 3: Identity fields appear correctly in staged metadata.
func TestIdentityInMetadata(t *testing.T) {
	posts := []map[string]interface{}{
		{
			"uri":       "at://did:plc:testuser123/app.bsky.feed.post/post1",
			"cid":       "cid-post-1",
			"author":    map[string]interface{}{"did": "did:plc:testuser123", "handle": "testuser.bsky.social"},
			"record":    map[string]interface{}{"text": "Identity test post", "createdAt": "2024-01-01T12:00:00Z"},
			"indexedAt": "2024-01-01T12:00:00Z",
		},
	}

	srv := mockBlueskyServer(t, posts)
	defer srv.Close()

	c, stagingDir, stateDir := newTestBlueskyConnector(t, srv.URL)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	metas := readMetadata(t, stagingDir)
	if len(metas) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(metas))
	}

	meta := metas[0]
	identity, ok := meta["identity"].(map[string]interface{})
	if !ok {
		t.Fatal("expected identity object in metadata")
	}
	if identity["provider"] != "bluesky" {
		t.Errorf("expected identity provider 'bluesky', got %v", identity["provider"])
	}
	if identity["auth_method"] != "app_password" {
		t.Errorf("expected identity auth_method 'app_password', got %v", identity["auth_method"])
	}
	if identity["account_id"] != "testuser.bsky.social" {
		t.Errorf("expected identity account_id 'testuser.bsky.social', got %v", identity["account_id"])
	}
}

// Test 4: Rule tags appear in staged metadata.
func TestRuleTagsInMetadata(t *testing.T) {
	posts := []map[string]interface{}{
		{
			"uri":       "at://did:plc:testuser123/app.bsky.feed.post/post1",
			"cid":       "cid-post-1",
			"author":    map[string]interface{}{"did": "did:plc:testuser123", "handle": "testuser.bsky.social"},
			"record":    map[string]interface{}{"text": "Tags test post", "createdAt": "2024-01-01T12:00:00Z"},
			"indexedAt": "2024-01-01T12:00:00Z",
		},
	}

	srv := mockBlueskyServer(t, posts)
	defer srv.Close()

	c, stagingDir, stateDir := newTestBlueskyConnector(t, srv.URL)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	metas := readMetadata(t, stagingDir)
	if len(metas) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(metas))
	}

	meta := metas[0]
	tags, ok := meta["tags"].(map[string]interface{})
	if !ok {
		t.Fatal("expected tags object in metadata")
	}
	if tags["source_type"] != "social" {
		t.Errorf("expected tag source_type 'social', got %v", tags["source_type"])
	}
	if tags["platform"] != "bluesky" {
		t.Errorf("expected tag platform 'bluesky', got %v", tags["platform"])
	}
}
