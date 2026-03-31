package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestNotionConnector creates a NotionConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestNotionConnector(t *testing.T, cfg Config, apiBase string, rules []connector.Rule) (*NotionConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "notion")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &NotionConnector{
		config:       cfg,
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-notion-token"),
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

// makeDatabaseQueryResponse builds a Notion database query response with the
// given page entries. Each entry has an id and last_edited_time.
func makeDatabaseQueryResponse(pages []map[string]string) []byte {
	type richText struct {
		Type      string `json:"type"`
		PlainText string `json:"plain_text"`
	}
	type titleProp struct {
		Title []richText `json:"title"`
	}
	type properties struct {
		Name titleProp `json:"Name"`
	}
	type page struct {
		Object         string     `json:"object"`
		ID             string     `json:"id"`
		LastEditedTime string     `json:"last_edited_time"`
		Properties     properties `json:"properties"`
	}
	type response struct {
		Results []page `json:"results"`
		HasMore bool   `json:"has_more"`
	}

	results := make([]page, 0, len(pages))
	for _, p := range pages {
		results = append(results, page{
			Object:         "page",
			ID:             p["id"],
			LastEditedTime: p["last_edited_time"],
			Properties: properties{
				Name: titleProp{
					Title: []richText{{Type: "text", PlainText: "Test Page " + p["id"]}},
				},
			},
		})
	}

	data, _ := json.Marshal(response{Results: results, HasMore: false})
	return data
}

// makeBlocksResponse builds a Notion blocks children response.
func makeBlocksResponse(blocks []map[string]interface{}) []byte {
	type response struct {
		Results []map[string]interface{} `json:"results"`
		HasMore bool                     `json:"has_more"`
	}
	data, _ := json.Marshal(response{Results: blocks, HasMore: false})
	return data
}

func makeParagraphBlock(text string) map[string]interface{} {
	return map[string]interface{}{
		"object": "block",
		"type":   "paragraph",
		"paragraph": map[string]interface{}{
			"rich_text": []map[string]interface{}{
				{"type": "text", "plain_text": text},
			},
		},
	}
}

func makeHeadingBlock(level int, text string) map[string]interface{} {
	typeName := "heading_1"
	if level == 2 {
		typeName = "heading_2"
	} else if level == 3 {
		typeName = "heading_3"
	}
	return map[string]interface{}{
		"object": "block",
		"type":   typeName,
		typeName: map[string]interface{}{
			"rich_text": []map[string]interface{}{
				{"type": "text", "plain_text": text},
			},
		},
	}
}

func makeBulletedListBlock(text string) map[string]interface{} {
	return map[string]interface{}{
		"object": "block",
		"type":   "bulleted_list_item",
		"bulleted_list_item": map[string]interface{}{
			"rich_text": []map[string]interface{}{
				{"type": "text", "plain_text": text},
			},
		},
	}
}

func makeNumberedListBlock(text string) map[string]interface{} {
	return map[string]interface{}{
		"object": "block",
		"type":   "numbered_list_item",
		"numbered_list_item": map[string]interface{}{
			"rich_text": []map[string]interface{}{
				{"type": "text", "plain_text": text},
			},
		},
	}
}

func TestPollDatabaseFetchesAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pages := []map[string]string{
			{"id": "page-1", "last_edited_time": "2026-03-28T10:00:00.000Z"},
			{"id": "page-2", "last_edited_time": "2026-03-28T11:00:00.000Z"},
		}
		w.Write(makeDatabaseQueryResponse(pages))
	}))
	defer srv.Close()

	cfg := Config{DatabaseIDs: []string{"db-abc123"}}
	rules := []connector.Rule{
		{Match: "database:db-abc123", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestNotionConnector(t, cfg, srv.URL, rules)
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

func TestPollPageBlocksExtractsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		blocks := []map[string]interface{}{
			makeParagraphBlock("Hello world."),
			makeHeadingBlock(1, "Section Title"),
			makeBulletedListBlock("Bullet point one"),
			makeNumberedListBlock("Numbered item one"),
		}
		w.Write(makeBlocksResponse(blocks))
	}))
	defer srv.Close()

	cfg := Config{PageIDs: []string{"page-xyz789"}}
	rules := []connector.Rule{
		{Match: "page:page-xyz789", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestNotionConnector(t, cfg, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staged item, got %d", count)
	}

	// Read the content and verify text was extracted from all block types.
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
		content := string(data)
		for _, expected := range []string{"Hello world.", "Section Title", "Bullet point one", "Numbered item one"} {
			if !containsString(content, expected) {
				t.Errorf("expected content to contain %q, got %q", expected, content)
			}
		}
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestNotionVersionHeaderSent(t *testing.T) {
	var mu sync.Mutex
	var capturedHeaders []http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedHeaders = append(capturedHeaders, r.Header.Clone())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// Return empty results for database query.
		w.Write([]byte(`{"results":[],"has_more":false}`))
	}))
	defer srv.Close()

	cfg := Config{DatabaseIDs: []string{"db-test"}}
	rules := []connector.Rule{
		{Match: "database:db-test", Destination: "test-agent"},
	}
	c, _, stateDir := newTestNotionConnector(t, cfg, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(capturedHeaders) == 0 {
		t.Fatal("expected at least one request to be made")
	}

	for i, h := range capturedHeaders {
		got := h.Get("Notion-Version")
		if got != "2022-06-28" {
			t.Errorf("request %d: expected Notion-Version '2022-06-28', got %q", i, got)
		}
		auth := h.Get("Authorization")
		if auth != "Bearer test-notion-token" {
			t.Errorf("request %d: expected Bearer test-notion-token, got %q", i, auth)
		}
	}
}

func TestCheckpointPreventsDuplicates(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			pages := []map[string]string{
				{"id": "page-1", "last_edited_time": "2026-03-28T10:00:00.000Z"},
				{"id": "page-2", "last_edited_time": "2026-03-28T11:00:00.000Z"},
			}
			w.Write(makeDatabaseQueryResponse(pages))
		} else {
			// Second poll returns same pages plus one new one with later timestamp.
			pages := []map[string]string{
				{"id": "page-1", "last_edited_time": "2026-03-28T10:00:00.000Z"},
				{"id": "page-2", "last_edited_time": "2026-03-28T11:00:00.000Z"},
				{"id": "page-3", "last_edited_time": "2026-03-28T12:00:00.000Z"},
			}
			w.Write(makeDatabaseQueryResponse(pages))
		}
		callCount++
	}))
	defer srv.Close()

	cfg := Config{DatabaseIDs: []string{"db-dedup"}}
	rules := []connector.Rule{
		{Match: "database:db-dedup", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestNotionConnector(t, cfg, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new item (page-3).
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
		pages := []map[string]string{
			{"id": "page-id-1", "last_edited_time": "2026-03-28T10:00:00.000Z"},
		}
		w.Write(makeDatabaseQueryResponse(pages))
	}))
	defer srv.Close()

	cfg := Config{DatabaseIDs: []string{"db-identity"}}
	rules := []connector.Rule{
		{Match: "database:db-identity", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestNotionConnector(t, cfg, srv.URL, rules)
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
		if identity["provider"] != "notion" {
			t.Errorf("expected identity provider 'notion', got %v", identity["provider"])
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pages := []map[string]string{
			{"id": "page-tag-1", "last_edited_time": "2026-03-28T10:00:00.000Z"},
		}
		w.Write(makeDatabaseQueryResponse(pages))
	}))
	defer srv.Close()

	cfg := Config{DatabaseIDs: []string{"db-tags"}}
	rules := []connector.Rule{
		{
			Match:       "database:db-tags",
			Destination: "test-agent",
			Tags:        map[string]string{"source_type": "knowledge_base", "priority": "normal"},
		},
	}
	c, stagingDir, stateDir := newTestNotionConnector(t, cfg, srv.URL, rules)
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
		if tags["source_type"] != "knowledge_base" {
			t.Errorf("expected tag source_type 'knowledge_base', got %v", tags["source_type"])
		}
		if tags["priority"] != "normal" {
			t.Errorf("expected tag priority 'normal', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
