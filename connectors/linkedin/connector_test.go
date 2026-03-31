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

// newTestConnector creates a LinkedInConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, feedTypes []string, apiBase string, rules []connector.Rule) (*LinkedInConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "linkedin")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &LinkedInConnector{
		config: Config{
			FeedTypes: feedTypes,
		},
		writer:       writer,
		matcher:      matcher,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-token"),
		apiBase:      apiBase,
		personID:     "abc123",
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
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

// makeShares builds a JSON response body matching the LinkedIn shares API format.
func makeShares(ids ...string) []byte {
	type share struct {
		ID      string `json:"id"`
		Text    string `json:"text"`
		Created struct {
			Time int64 `json:"time"`
		} `json:"created"`
	}
	type response struct {
		Elements []share `json:"elements"`
	}

	resp := response{}
	for _, id := range ids {
		s := share{ID: id, Text: "Post content for " + id}
		s.Created.Time = time.Now().UnixMilli()
		resp.Elements = append(resp.Elements, s)
	}
	data, _ := json.Marshal(resp)
	return data
}

func TestPollFetchesSharesAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header is present.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeShares("s100", "s101", "s102"))
	}))
	defer srv.Close()

	feedTypes := []string{"shares"}
	rules := []connector.Rule{
		{Match: "feed:shares", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, feedTypes, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staged items, got %d", count)
	}
}

func TestCheckpointPreventsDuplicates(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			// First call: return shares s100, s101 (newest first)
			w.Write(makeShares("s101", "s100"))
		} else {
			// Second call: same shares plus one new one
			w.Write(makeShares("s102", "s101", "s100"))
		}
		callCount++
	}))
	defer srv.Close()

	feedTypes := []string{"shares"}
	rules := []connector.Rule{
		{Match: "feed:shares", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, feedTypes, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new item.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 3 {
		t.Errorf("expected 3 items total after second poll, got %d", got)
	}
}

func TestIdentityFieldsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeShares("s200"))
	}))
	defer srv.Close()

	feedTypes := []string{"shares"}
	rules := []connector.Rule{
		{Match: "feed:shares", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, feedTypes, srv.URL, rules)
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
		if identity["provider"] != "linkedin" {
			t.Errorf("expected identity provider 'linkedin', got %v", identity["provider"])
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeShares("s300"))
	}))
	defer srv.Close()

	feedTypes := []string{"shares"}
	rules := []connector.Rule{
		{
			Match:       "feed:shares",
			Destination: "test-agent",
			Tags:        map[string]string{"source_type": "social", "priority": "low"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, feedTypes, srv.URL, rules)
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
		if tags["source_type"] != "social" {
			t.Errorf("expected tag source_type 'social', got %v", tags["source_type"])
		}
		if tags["priority"] != "low" {
			t.Errorf("expected tag priority 'low', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
