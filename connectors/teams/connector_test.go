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

// newTestConnector creates a TeamsConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, channels []ChannelConfig, apiBase string, rules []connector.Rule) (*TeamsConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "teams")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &TeamsConnector{
		config: Config{
			Channels: channels,
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-token"),
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
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

// makeGraphMessages builds a Microsoft Graph API response with chat messages.
func makeGraphMessages(messages ...graphMessage) []byte {
	resp := graphMessagesResponse{
		Value: messages,
	}
	data, _ := json.Marshal(resp)
	return data
}

func TestPollFetchesMessagesAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header is present.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeGraphMessages(
			graphMessage{
				ID:              "msg-1",
				CreatedDateTime: "2026-03-28T10:00:00Z",
				From: graphFrom{
					User: &graphUser{DisplayName: "Alice"},
				},
				Body: graphBody{ContentType: "html", Content: "<p>Hello world</p>"},
			},
			graphMessage{
				ID:              "msg-2",
				CreatedDateTime: "2026-03-28T10:01:00Z",
				From: graphFrom{
					User: &graphUser{DisplayName: "Bob"},
				},
				Body: graphBody{ContentType: "html", Content: "<b>Important</b> update"},
			},
		))
	}))
	defer srv.Close()

	channels := []ChannelConfig{
		{TeamID: "team-1", ChannelID: "chan-1", Name: "general"},
	}
	rules := []connector.Rule{
		{Match: "channel:general", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channels, srv.URL, rules)
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

func TestHTMLBodyDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeGraphMessages(
			graphMessage{
				ID:              "msg-html",
				CreatedDateTime: "2026-03-28T12:00:00Z",
				From: graphFrom{
					User: &graphUser{DisplayName: "Carol"},
				},
				Body: graphBody{ContentType: "html", Content: "<div><p>Hello</p> <b>world</b></div>"},
			},
		))
	}))
	defer srv.Close()

	channels := []ChannelConfig{
		{TeamID: "team-1", ChannelID: "chan-1", Name: "general"},
	}
	rules := []connector.Rule{
		{Match: "channel:general", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channels, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Read the staged content and verify HTML was converted to text.
	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found = true
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("parse content JSON: %v", err)
		}

		body, ok := msg["body"].(string)
		if !ok {
			t.Fatal("expected body string in content")
		}
		// Should not contain HTML tags after conversion.
		if body != "Hello world" {
			t.Errorf("expected body 'Hello world', got %q", body)
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestCheckpointPreventsDuplicates(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			// First call: return 2 messages.
			w.Write(makeGraphMessages(
				graphMessage{
					ID:              "msg-1",
					CreatedDateTime: "2026-03-28T10:00:00Z",
					From:            graphFrom{User: &graphUser{DisplayName: "Alice"}},
					Body:            graphBody{ContentType: "text", Content: "First"},
				},
				graphMessage{
					ID:              "msg-2",
					CreatedDateTime: "2026-03-28T10:01:00Z",
					From:            graphFrom{User: &graphUser{DisplayName: "Bob"}},
					Body:            graphBody{ContentType: "text", Content: "Second"},
				},
			))
		} else {
			// Second call: same messages plus one new one.
			w.Write(makeGraphMessages(
				graphMessage{
					ID:              "msg-1",
					CreatedDateTime: "2026-03-28T10:00:00Z",
					From:            graphFrom{User: &graphUser{DisplayName: "Alice"}},
					Body:            graphBody{ContentType: "text", Content: "First"},
				},
				graphMessage{
					ID:              "msg-2",
					CreatedDateTime: "2026-03-28T10:01:00Z",
					From:            graphFrom{User: &graphUser{DisplayName: "Bob"}},
					Body:            graphBody{ContentType: "text", Content: "Second"},
				},
				graphMessage{
					ID:              "msg-3",
					CreatedDateTime: "2026-03-28T10:02:00Z",
					From:            graphFrom{User: &graphUser{DisplayName: "Carol"}},
					Body:            graphBody{ContentType: "text", Content: "Third"},
				},
			))
		}
		callCount++
	}))
	defer srv.Close()

	channels := []ChannelConfig{
		{TeamID: "team-1", ChannelID: "chan-1", Name: "general"},
	}
	rules := []connector.Rule{
		{Match: "channel:general", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channels, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 messages.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new message.
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
		w.Write(makeGraphMessages(
			graphMessage{
				ID:              "msg-id-1",
				CreatedDateTime: "2026-03-28T14:00:00Z",
				From:            graphFrom{User: &graphUser{DisplayName: "Dave"}},
				Body:            graphBody{ContentType: "text", Content: "Test message"},
			},
		))
	}))
	defer srv.Close()

	channels := []ChannelConfig{
		{TeamID: "team-1", ChannelID: "chan-1", Name: "general"},
	}
	rules := []connector.Rule{
		{Match: "channel:general", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channels, srv.URL, rules)
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
		if identity["provider"] != "microsoft" {
			t.Errorf("expected identity provider 'microsoft', got %v", identity["provider"])
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
		w.Write(makeGraphMessages(
			graphMessage{
				ID:              "msg-tag-1",
				CreatedDateTime: "2026-03-28T15:00:00Z",
				From:            graphFrom{User: &graphUser{DisplayName: "Eve"}},
				Body:            graphBody{ContentType: "text", Content: "Tag test"},
			},
		))
	}))
	defer srv.Close()

	channels := []ChannelConfig{
		{TeamID: "team-1", ChannelID: "chan-1", Name: "general"},
	}
	rules := []connector.Rule{
		{
			Match:       "channel:general",
			Destination: "test-agent",
			Tags:        map[string]string{"source_type": "chat", "priority": "normal"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, channels, srv.URL, rules)
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
		if tags["source_type"] != "chat" {
			t.Errorf("expected tag source_type 'chat', got %v", tags["source_type"])
		}
		if tags["priority"] != "normal" {
			t.Errorf("expected tag priority 'normal', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
