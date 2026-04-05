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

// mockReviewsJSON returns a JSON response mimicking the Steam reviews API.
func mockReviewsJSON(reviews []review) string {
	resp := reviewsResponse{
		Success: 1,
		Reviews: reviews,
	}
	data, _ := json.Marshal(resp)
	return string(data)
}

// mockNewsJSON returns a JSON response mimicking the Steam news API.
func mockNewsJSON(items []newsItem) string {
	resp := newsResponse{}
	resp.AppNews.AppID = 440
	resp.AppNews.NewsItems = items
	data, _ := json.Marshal(resp)
	return string(data)
}

func newTestSteamConnector(t *testing.T, cfg Config, apiKey string, baseReviewURL string, baseNewsURL string) (*SteamConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "steam")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := make([]connector.Rule, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		rules = append(rules, connector.Rule{
			Match:       "app:" + app.Name,
			Destination: "test-agent",
		})
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &SteamConnector{
		config:        cfg,
		apiKey:        apiKey,
		writer:        writer,
		matcher:       matcher,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		fetchCounter:  connector.NewFetchCounter(connector.FetchLimits{}),
		baseReviewURL: baseReviewURL,
		baseNewsURL:   baseNewsURL,
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

func TestPollReviews(t *testing.T) {
	reviews := []review{
		{
			RecommendationID: "rev-3",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user3"},
			Review:           "Great game, highly recommend",
			TimestampCreated: 1704326400, // 2024-01-04
			VotedUp:          true,
		},
		{
			RecommendationID: "rev-2",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user2"},
			Review:           "Decent but has bugs",
			TimestampCreated: 1704240000, // 2024-01-03
			VotedUp:          true,
		},
		{
			RecommendationID: "rev-1",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user1"},
			Review:           "Not worth the money",
			TimestampCreated: 1704153600, // 2024-01-02
			VotedUp:          false,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockReviewsJSON(reviews)))
	}))
	defer srv.Close()

	apps := []AppConfig{{ID: "440", Name: "tf2"}}
	cfg := Config{
		Apps:         apps,
		FetchReviews: true,
		FetchNews:    false,
	}
	c, stagingDir, stateDir := newTestSteamConnector(t, cfg, "", srv.URL, "")
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staged items, got %d", count)
	}

	// Checkpoint should be the newest timestamp.
	lastTS, ok := cp.Load("review:440")
	if !ok {
		t.Fatal("checkpoint not saved for reviews")
	}
	if lastTS != "1704326400" {
		t.Errorf("expected checkpoint '1704326400', got %q", lastTS)
	}
}

func TestPollNews(t *testing.T) {
	items := []newsItem{
		{
			GID:       "news-2",
			Title:     "Big Update Coming",
			URL:       "https://store.steampowered.com/news/2",
			Contents:  "We are excited to announce a big update.",
			Author:    "Valve",
			Date:      1704240000, // 2024-01-03
			FeedLabel: "Community Announcements",
			FeedName:  "steam_community_announcements",
		},
		{
			GID:       "news-1",
			Title:     "Holiday Event",
			URL:       "https://store.steampowered.com/news/1",
			Contents:  "Happy holidays from the team!",
			Author:    "Valve",
			Date:      1704153600, // 2024-01-02
			FeedLabel: "Community Announcements",
			FeedName:  "steam_community_announcements",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockNewsJSON(items)))
	}))
	defer srv.Close()

	apps := []AppConfig{{ID: "440", Name: "tf2"}}
	cfg := Config{
		Apps:         apps,
		FetchReviews: false,
		FetchNews:    true,
	}
	c, stagingDir, stateDir := newTestSteamConnector(t, cfg, "test-key", "", srv.URL)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}

	lastTS, ok := cp.Load("news:440")
	if !ok {
		t.Fatal("checkpoint not saved for news")
	}
	if lastTS != "1704240000" {
		t.Errorf("expected checkpoint '1704240000', got %q", lastTS)
	}
}

func TestCheckpointDedup(t *testing.T) {
	reviews := []review{
		{
			RecommendationID: "rev-2",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user2"},
			Review:           "Good game",
			TimestampCreated: 1704240000,
			VotedUp:          true,
		},
		{
			RecommendationID: "rev-1",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user1"},
			Review:           "Bad game",
			TimestampCreated: 1704153600,
			VotedUp:          false,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockReviewsJSON(reviews)))
	}))
	defer srv.Close()

	apps := []AppConfig{{ID: "440", Name: "tf2"}}
	cfg := Config{
		Apps:         apps,
		FetchReviews: true,
		FetchNews:    false,
	}
	c, stagingDir, stateDir := newTestSteamConnector(t, cfg, "", srv.URL, "")
	cp := newCheckpoint(t, stateDir)

	// First poll: process all items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll with same data: should produce 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}
}

func TestReviewIdentity(t *testing.T) {
	reviews := []review{
		{
			RecommendationID: "rev-1",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user1"},
			Review:           "A review",
			TimestampCreated: 1704153600,
			VotedUp:          true,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockReviewsJSON(reviews)))
	}))
	defer srv.Close()

	apps := []AppConfig{{ID: "440", Name: "tf2"}}
	cfg := Config{
		Apps:         apps,
		FetchReviews: true,
		FetchNews:    false,
	}
	c, stagingDir, stateDir := newTestSteamConnector(t, cfg, "", srv.URL, "")
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
		if identity["provider"] != "steam" {
			t.Errorf("expected identity provider 'steam', got %v", identity["provider"])
		}
		if identity["auth_method"] != "none" {
			t.Errorf("expected identity auth_method 'none' for reviews, got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestNewsIdentity(t *testing.T) {
	items := []newsItem{
		{
			GID:      "news-1",
			Title:    "Update",
			URL:      "https://example.com/news/1",
			Contents: "News body",
			Author:   "Dev",
			Date:     1704153600,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockNewsJSON(items)))
	}))
	defer srv.Close()

	apps := []AppConfig{{ID: "440", Name: "tf2"}}
	cfg := Config{
		Apps:         apps,
		FetchReviews: false,
		FetchNews:    true,
	}
	c, stagingDir, stateDir := newTestSteamConnector(t, cfg, "test-key", "", srv.URL)
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
		if identity["provider"] != "steam" {
			t.Errorf("expected identity provider 'steam', got %v", identity["provider"])
		}
		if identity["auth_method"] != "api_key" {
			t.Errorf("expected identity auth_method 'api_key' for news, got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTags(t *testing.T) {
	reviews := []review{
		{
			RecommendationID: "rev-1",
			Author:           struct{ SteamID string `json:"steamid"` }{SteamID: "user1"},
			Review:           "Tagged review",
			TimestampCreated: 1704153600,
			VotedUp:          true,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mockReviewsJSON(reviews)))
	}))
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "steam")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "app:tf2",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "gaming", "priority": "low"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	apps := []AppConfig{{ID: "440", Name: "tf2"}}
	c := &SteamConnector{
		config: Config{
			Apps:         apps,
			FetchReviews: true,
			FetchNews:    false,
		},
		writer:        writer,
		matcher:       matcher,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		fetchCounter:  connector.NewFetchCounter(connector.FetchLimits{}),
		baseReviewURL: srv.URL,
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
		if tags["category"] != "gaming" {
			t.Errorf("expected tag category 'gaming', got %v", tags["category"])
		}
		if tags["priority"] != "low" {
			t.Errorf("expected tag priority 'low', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
