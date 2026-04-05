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

// sampleAtomResponse returns an Arxiv-style Atom XML response with two entries.
func sampleAtomResponse() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"
      xmlns:arxiv="http://arxiv.org/schemas/atom">
  <title>ArXiv Query: cat:cs.AI</title>
  <entry>
    <id>http://arxiv.org/abs/2401.00001v1</id>
    <title>Transformers Are All You Need, Again</title>
    <summary>We revisit the transformer architecture and show improvements.</summary>
    <published>2024-01-02T00:00:00Z</published>
    <updated>2024-01-02T00:00:00Z</updated>
    <author><name>Alice Smith</name></author>
    <author><name>Bob Jones</name></author>
    <link href="http://arxiv.org/abs/2401.00001v1" rel="alternate" type="text/html"/>
    <arxiv:primary_category term="cs.AI"/>
    <category term="cs.AI"/>
    <category term="cs.LG"/>
  </entry>
  <entry>
    <id>http://arxiv.org/abs/2401.00002v1</id>
    <title>On the Limits of Language Models</title>
    <summary>An analysis of current language model limitations.</summary>
    <published>2024-01-01T00:00:00Z</published>
    <updated>2024-01-01T00:00:00Z</updated>
    <author><name>Charlie Brown</name></author>
    <link href="http://arxiv.org/abs/2401.00002v1" rel="alternate" type="text/html"/>
    <arxiv:primary_category term="cs.CL"/>
    <category term="cs.CL"/>
    <category term="cs.AI"/>
  </entry>
</feed>`
}

func newTestArxivConnector(t *testing.T, queries []QueryConfig) (*ArxivConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "arxiv")
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

	c := &ArxivConnector{
		config: Config{
			Queries: queries,
		},
		writer:       writer,
		matcher:      matcher,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
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

func TestPollFetchesPapers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the query parameters are set correctly.
		q := r.URL.Query()
		if q.Get("search_query") != "cat:cs.AI" {
			t.Errorf("expected search_query=cat:cs.AI, got %q", q.Get("search_query"))
		}
		if q.Get("sortBy") != "submittedDate" {
			t.Errorf("expected sortBy=submittedDate, got %q", q.Get("sortBy"))
		}
		if q.Get("sortOrder") != "descending" {
			t.Errorf("expected sortOrder=descending, got %q", q.Get("sortOrder"))
		}
		if q.Get("max_results") != "25" {
			t.Errorf("expected max_results=25, got %q", q.Get("max_results"))
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(sampleAtomResponse()))
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "ai-papers", Query: "cat:cs.AI", MaxResults: 25}}
	c, stagingDir, stateDir := newTestArxivConnector(t, queries)
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

	// Check content of a staged item includes expected JSON fields.
	entries, _ := os.ReadDir(stagingDir)
	foundTransformer := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		var paper map[string]interface{}
		if err := json.Unmarshal(data, &paper); err != nil {
			t.Fatalf("content should be valid JSON: %v\ncontent: %s", err, string(data))
		}
		if title, ok := paper["title"].(string); ok && strings.Contains(title, "Transformers") {
			foundTransformer = true
			if paper["abstract"] == nil || paper["abstract"] == "" {
				t.Error("expected non-empty abstract")
			}
			authors, ok := paper["authors"].([]interface{})
			if !ok || len(authors) != 2 {
				t.Errorf("expected 2 authors, got %v", paper["authors"])
			}
			categories, ok := paper["categories"].([]interface{})
			if !ok || len(categories) != 2 {
				t.Errorf("expected 2 categories, got %v", paper["categories"])
			}
			if paper["link"] == nil || paper["link"] == "" {
				t.Error("expected non-empty link")
			}
		}
	}
	if !foundTransformer {
		t.Error("expected to find the transformer paper in staged items")
	}
}

func TestCheckpointDedup(t *testing.T) {
	atomXML := sampleAtomResponse()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(atomXML))
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "dedup-test", Query: "cat:cs.AI", MaxResults: 25}}
	c, stagingDir, stateDir := newTestArxivConnector(t, queries)
	c.baseURL = srv.URL
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll with same data: should stage 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}

	// Verify checkpoint was saved.
	lastID, ok := cp.Load("last:dedup-test")
	if !ok {
		t.Fatal("checkpoint not saved for query 'dedup-test'")
	}
	// The newest entry (first in descending feed, last processed after reverse)
	// should be the checkpoint.
	if lastID != "http://arxiv.org/abs/2401.00001v1" {
		t.Errorf("expected checkpoint for newest paper, got %q", lastID)
	}
}

func TestIdentityInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(sampleAtomResponse()))
	}))
	defer srv.Close()

	queries := []QueryConfig{{Name: "id-test", Query: "cat:cs.AI", MaxResults: 10}}
	c, stagingDir, stateDir := newTestArxivConnector(t, queries)
	c.baseURL = srv.URL
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

		if meta["source"] != "arxiv" {
			t.Errorf("expected source 'arxiv', got %v", meta["source"])
		}

		identity, ok := meta["identity"].(map[string]interface{})
		if !ok {
			t.Fatal("expected identity object in metadata")
		}
		if identity["provider"] != "arxiv" {
			t.Errorf("expected identity provider 'arxiv', got %v", identity["provider"])
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(sampleAtomResponse()))
	}))
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "arxiv")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "query:tagged-query",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "research", "priority": "high"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	queries := []QueryConfig{{Name: "tagged-query", Query: "cat:cs.AI", MaxResults: 10}}
	c := &ArxivConnector{
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
		if tags["category"] != "research" {
			t.Errorf("expected tag category 'research', got %v", tags["category"])
		}
		if tags["priority"] != "high" {
			t.Errorf("expected tag priority 'high', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestDefaultMaxResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("max_results") != "25" {
			t.Errorf("expected default max_results=25, got %q", q.Get("max_results"))
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(sampleAtomResponse()))
	}))
	defer srv.Close()

	// MaxResults=0 should default to 25.
	queries := []QueryConfig{{Name: "default-max", Query: "all:transformer", MaxResults: 0}}
	c, _, stateDir := newTestArxivConnector(t, queries)
	c.baseURL = srv.URL
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}
