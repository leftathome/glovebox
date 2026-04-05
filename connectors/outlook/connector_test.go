package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// --- mock checkpoint ---

type mockCheckpoint struct {
	data map[string]string
}

func newMockCheckpoint() *mockCheckpoint {
	return &mockCheckpoint{data: make(map[string]string)}
}

func (m *mockCheckpoint) Load(key string) (string, bool) {
	v, ok := m.data[key]
	return v, ok
}

func (m *mockCheckpoint) Save(key string, value string) error {
	m.data[key] = value
	return nil
}

func (m *mockCheckpoint) Delete(key string) error {
	delete(m.data, key)
	return nil
}

// --- mock Outlook client ---

type mockOutlookClient struct {
	// messages maps folder -> list of messages available under that folder.
	messages map[string][]OutlookMessage
	// listCalls tracks which folders were queried and with which checkpoint.
	listCalls []mockListCall
}

type mockListCall struct {
	FolderID   string
	Checkpoint string
	MaxResults int
}

func newMockOutlookClient(messages map[string][]OutlookMessage) *mockOutlookClient {
	return &mockOutlookClient{
		messages: messages,
	}
}

func (m *mockOutlookClient) ListMessages(ctx context.Context, folderID string, checkpoint string, maxResults int) ([]OutlookMessage, error) {
	m.listCalls = append(m.listCalls, mockListCall{
		FolderID:   folderID,
		Checkpoint: checkpoint,
		MaxResults: maxResults,
	})

	msgs, ok := m.messages[folderID]
	if !ok {
		return nil, nil
	}

	var result []OutlookMessage
	for _, msg := range msgs {
		// Filter by checkpoint: only return messages with receivedDateTime > checkpoint.
		if checkpoint != "" && msg.ReceivedDateTime <= checkpoint {
			continue
		}
		result = append(result, msg)
		if maxResults > 0 && len(result) >= maxResults {
			break
		}
	}
	return result, nil
}

// --- helpers ---

func setupTestConnector(t *testing.T, mock OutlookClient, cfg Config, rules []connector.Rule) (*OutlookConnector, *mockCheckpoint, string) {
	t.Helper()

	stagingDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "outlook")
	if err != nil {
		t.Fatalf("create staging writer: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)
	cp := newMockCheckpoint()

	c := &OutlookConnector{
		config:       cfg,
		writer:       writer,
		matcher:      matcher,
		client:       mock,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
	}

	return c, cp, stagingDir
}

func countStagingItems(t *testing.T, stagingDir string) int {
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

func readStagingMetadata(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	return meta
}

func readStagingContent(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "content.raw"))
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	return string(data)
}

func stagingItemDirs(t *testing.T, stagingDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, filepath.Join(stagingDir, e.Name()))
		}
	}
	return dirs
}

// --- tests ---

func TestPollFetchesMessages(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{ID: "msg1", Subject: "Hello", From: "alice@example.com", ReceivedDateTime: "2024-01-01T12:00:00Z", BodyContent: "Body one", BodyContentType: "text"},
			{ID: "msg2", Subject: "Meeting", From: "bob@example.com", ReceivedDateTime: "2024-01-01T12:01:00Z", BodyContent: "Body two", BodyContentType: "text"},
			{ID: "msg3", Subject: "Update", From: "carol@example.com", ReceivedDateTime: "2024-01-01T12:02:00Z", BodyContent: "Body three", BodyContentType: "text"},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "folder:inbox", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staging items, got %d", count)
	}

	dirs := stagingItemDirs(t, stagingDir)
	for _, dir := range dirs {
		meta := readStagingMetadata(t, dir)
		if meta["source"] != "outlook" {
			t.Errorf("expected source=outlook, got %v", meta["source"])
		}
		if meta["destination_agent"] != "messaging" {
			t.Errorf("expected destination_agent=messaging, got %v", meta["destination_agent"])
		}
		if meta["content_type"] != "text/plain" {
			t.Errorf("expected content_type=text/plain, got %v", meta["content_type"])
		}
	}

	// Verify that the mock client was called for the inbox folder.
	if len(mock.listCalls) != 1 {
		t.Errorf("expected 1 ListMessages call, got %d", len(mock.listCalls))
	}
}

func TestHTMLBodyDecodedToText(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{
				ID:               "msg1",
				Subject:          "HTML email",
				From:             "sender@example.com",
				ReceivedDateTime: "2024-01-01T12:00:00Z",
				BodyContent:      "<html><body><p>Hello world</p><br><b>Bold text</b></body></html>",
				BodyContentType:  "html",
			},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "folder:inbox", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	dirs := stagingItemDirs(t, stagingDir)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 staging item, got %d", len(dirs))
	}

	body := readStagingContent(t, dirs[0])
	if !strings.Contains(body, "Hello world") {
		t.Errorf("expected body to contain 'Hello world', got: %s", body)
	}
	if !strings.Contains(body, "Bold text") {
		t.Errorf("expected body to contain 'Bold text', got: %s", body)
	}
	// HTML tags should be stripped.
	if strings.Contains(body, "<p>") {
		t.Errorf("expected HTML tags to be stripped, got: %s", body)
	}
}

func TestCheckpointDedup(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{ID: "msg1", Subject: "Old", From: "a@example.com", ReceivedDateTime: "2024-01-01T12:00:00Z", BodyContent: "old body", BodyContentType: "text"},
			{ID: "msg2", Subject: "Also old", From: "b@example.com", ReceivedDateTime: "2024-01-01T12:01:00Z", BodyContent: "also old", BodyContentType: "text"},
			{ID: "msg3", Subject: "New", From: "c@example.com", ReceivedDateTime: "2024-01-01T13:00:00Z", BodyContent: "new body", BodyContentType: "text"},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "folder:inbox", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	// Pre-set checkpoint: messages at or before this time are already processed.
	cp.Save("receiveddatetime:inbox", "2024-01-01T12:01:00Z")

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staging item (only msg3), got %d", count)
	}

	// Verify checkpoint advanced to latest receivedDateTime.
	val, ok := cp.Load("receiveddatetime:inbox")
	if !ok {
		t.Fatal("expected checkpoint receiveddatetime:inbox to be set")
	}
	if val != "2024-01-01T13:00:00Z" {
		t.Errorf("expected checkpoint=2024-01-01T13:00:00Z, got %s", val)
	}
}

func TestIdentityInStagedMetadata(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{ID: "msg1", Subject: "Hello", From: "alice@example.com", ReceivedDateTime: "2024-01-01T12:00:00Z", BodyContent: "Body", BodyContentType: "text"},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "folder:inbox", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	dirs := stagingItemDirs(t, stagingDir)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 staging item, got %d", len(dirs))
	}

	meta := readStagingMetadata(t, dirs[0])

	identity, ok := meta["identity"].(map[string]interface{})
	if !ok {
		t.Fatal("expected identity in metadata")
	}
	if identity["provider"] != "microsoft" {
		t.Errorf("expected provider=microsoft, got %v", identity["provider"])
	}
	if identity["auth_method"] != "oauth" {
		t.Errorf("expected auth_method=oauth, got %v", identity["auth_method"])
	}
}

func TestRuleTagsInStagedMetadata(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{ID: "msg1", Subject: "Hello", From: "alice@example.com", ReceivedDateTime: "2024-01-01T12:00:00Z", BodyContent: "Body", BodyContentType: "text"},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{
			Match:       "folder:inbox",
			Destination: "messaging",
			Tags:        map[string]string{"priority": "high", "category": "personal"},
		},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	dirs := stagingItemDirs(t, stagingDir)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 staging item, got %d", len(dirs))
	}

	meta := readStagingMetadata(t, dirs[0])

	tags, ok := meta["tags"].(map[string]interface{})
	if !ok {
		t.Fatal("expected tags in metadata")
	}
	if tags["priority"] != "high" {
		t.Errorf("expected tag priority=high, got %v", tags["priority"])
	}
	if tags["category"] != "personal" {
		t.Errorf("expected tag category=personal, got %v", tags["category"])
	}
}

func TestFolderRouting(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{ID: "msg1", Subject: "Inbox msg", From: "a@example.com", ReceivedDateTime: "2024-01-01T12:00:00Z", BodyContent: "inbox body", BodyContentType: "text"},
		},
		"sentitems": {
			{ID: "msg2", Subject: "Sent msg", From: "b@example.com", ReceivedDateTime: "2024-01-01T12:01:00Z", BodyContent: "sent body", BodyContentType: "text"},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox", "sentitems"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "folder:inbox", Destination: "messaging"},
		{Match: "folder:sentitems", Destination: "archive"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staging items, got %d", count)
	}

	dirs := stagingItemDirs(t, stagingDir)
	destinations := make(map[string]bool)
	for _, dir := range dirs {
		meta := readStagingMetadata(t, dir)
		dest, ok := meta["destination_agent"].(string)
		if !ok {
			t.Errorf("destination_agent missing or not string in %s", dir)
			continue
		}
		destinations[dest] = true
	}

	if !destinations["messaging"] {
		t.Error("expected a staging item routed to messaging")
	}
	if !destinations["archive"] {
		t.Error("expected a staging item routed to archive")
	}
}

func TestNoFolderRuleSkipsFolder(t *testing.T) {
	mock := newMockOutlookClient(map[string][]OutlookMessage{
		"inbox": {
			{ID: "msg1", Subject: "A", From: "a@example.com", ReceivedDateTime: "2024-01-01T12:00:00Z", BodyContent: "body", BodyContentType: "text"},
		},
		"junkemail": {
			{ID: "msg2", Subject: "B", From: "b@example.com", ReceivedDateTime: "2024-01-01T12:01:00Z", BodyContent: "body", BodyContentType: "text"},
		},
	})

	cfg := Config{
		FolderIDs:  []string{"inbox", "junkemail"},
		MaxResults: 25,
	}
	// Only inbox has a rule; junkemail should be skipped.
	rules := []connector.Rule{
		{Match: "folder:inbox", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staging item (inbox only), got %d", count)
	}
}
