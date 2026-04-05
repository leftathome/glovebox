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

func sampleSearchResponse() searchResponse {
	return searchResponse{
		Data: []paper{
			{
				PaperID:  "abc123",
				Title:    "Attention Is All You Need",
				Abstract: "We propose a new architecture based on attention mechanisms.",
				TLDR:     &tldr{Text: "Transformers use attention instead of recurrence."},
				Authors:  []author{{Name: "Vaswani"}, {Name: "Shazeer"}},
				Year:     2017,
			},
			{
				PaperID:  "def456",
				Title:    "BERT: Pre-training of Deep Bidirectional Transformers",
				Abstract: "We introduce a new language representation model.",
				TLDR:     nil,
				Authors:  []author{{Name: "Devlin"}},
				Year:     2019,
			},
		},
	}
}

func newTestConnector(t *testing.T, queries []QueryConfig) (*SemanticScholarConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "semantic-scholar")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := make([]connector.Rule, 0, len(queries))
	for _, q := range queries {
		rules = append(rules, connector.Rule{
			Match:       "query:" + q.Name,
			Destination: "test-agent",
		})
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &SemanticScholarConnector{
		config:       Config{Queries: queries},
		writer:       writer,
		matcher:      matcher,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
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
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

func TestPollFetchesPapers(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "transformers", Query: "transformers"}}
	c, stagingDir, stateDir := newTestConnector(t, queries)
	c.baseURL = srv.URL
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}

	// Verify content of first staged item includes paper data.
	entries, _ := os.ReadDir(stagingDir)
	foundTitle := false
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
		if strings.Contains(text, "Attention Is All You Need") {
			foundTitle = true
			if !strings.Contains(text, "Vaswani") {
				t.Error("expected author name in content")
			}
			if !strings.Contains(text, "attention mechanisms") {
				t.Error("expected abstract text in content")
			}
		}
	}
	if !foundTitle {
		t.Error("expected to find paper title in staged content")
	}
}

func TestCheckpointDedup(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "transformers", Query: "transformers"}}
	c, stagingDir, stateDir := newTestConnector(t, queries)
	c.baseURL = srv.URL
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

func TestAPIKeyHeader(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	var receivedAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "test", Query: "test"}}
	c, _, stateDir := newTestConnector(t, queries)
	c.baseURL = srv.URL
	c.apiKey = "test-secret-key"
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if receivedAPIKey != "test-secret-key" {
		t.Errorf("expected x-api-key header 'test-secret-key', got %q", receivedAPIKey)
	}
}

func TestAPIKeyHeaderAbsentWhenNotSet(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	var receivedAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "test", Query: "test"}}
	c, _, stateDir := newTestConnector(t, queries)
	c.baseURL = srv.URL
	// apiKey is empty -- no header should be sent.
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if receivedAPIKey != "" {
		t.Errorf("expected no x-api-key header, got %q", receivedAPIKey)
	}
}

func TestIdentityWithAPIKey(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "test", Query: "test"}}
	c, stagingDir, stateDir := newTestConnector(t, queries)
	c.baseURL = srv.URL
	c.apiKey = "some-key"
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
		if identity["provider"] != "semantic-scholar" {
			t.Errorf("expected identity provider 'semantic-scholar', got %v", identity["provider"])
		}
		if identity["auth_method"] != "api_key" {
			t.Errorf("expected identity auth_method 'api_key', got %v", identity["auth_method"])
		}
		break
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestIdentityWithoutAPIKey(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "test", Query: "test"}}
	c, stagingDir, stateDir := newTestConnector(t, queries)
	c.baseURL = srv.URL
	// No API key.
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
		if identity["provider"] != "semantic-scholar" {
			t.Errorf("expected identity provider 'semantic-scholar', got %v", identity["provider"])
		}
		if identity["auth_method"] != "none" {
			t.Errorf("expected identity auth_method 'none', got %v", identity["auth_method"])
		}
		break
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTags(t *testing.T) {
	resp := sampleSearchResponse()
	respJSON, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	}))
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "semantic-scholar")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "query:tagged",
			Destination: "research-agent",
			Tags:        map[string]string{"domain": "ml", "priority": "high"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	queries := []QueryConfig{{Name: "tagged", Query: "machine learning"}}
	c := &SemanticScholarConnector{
		config:       Config{Queries: queries},
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
		if tags["domain"] != "ml" {
			t.Errorf("expected tag domain 'ml', got %v", tags["domain"])
		}
		if tags["priority"] != "high" {
			t.Errorf("expected tag priority 'high', got %v", tags["priority"])
		}
		break
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
