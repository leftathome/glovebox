package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// --- mock Gmail client ---

type mockGmailClient struct {
	// messages maps label -> list of messages available under that label
	messages map[string][]GmailMessage
	// getCalls tracks which message IDs were fetched
	getCalls []string
}

func newMockGmailClient(messages map[string][]GmailMessage) *mockGmailClient {
	return &mockGmailClient{
		messages: messages,
	}
}

func (m *mockGmailClient) ListMessages(ctx context.Context, labelID string, query string, maxResults int) ([]string, error) {
	msgs, ok := m.messages[labelID]
	if !ok {
		return nil, nil
	}

	// Parse "after:" epoch from query to simulate checkpoint filtering.
	var afterEpoch int64
	if strings.Contains(query, "after:") {
		fmt.Sscanf(query, "after:%d", &afterEpoch)
	}

	var ids []string
	for _, msg := range msgs {
		// Gmail after: filter uses epoch seconds; internalDate is epoch ms.
		if afterEpoch > 0 && msg.InternalDate/1000 <= afterEpoch {
			continue
		}
		ids = append(ids, msg.ID)
		if maxResults > 0 && len(ids) >= maxResults {
			break
		}
	}
	return ids, nil
}

func (m *mockGmailClient) GetMessage(ctx context.Context, messageID string) (*GmailMessage, error) {
	m.getCalls = append(m.getCalls, messageID)
	for _, msgs := range m.messages {
		for i := range msgs {
			if msgs[i].ID == messageID {
				return &msgs[i], nil
			}
		}
	}
	return nil, fmt.Errorf("message %s not found", messageID)
}

// --- helpers ---

// makeRawEmail builds a minimal RFC 2822 message for testing.
func makeRawEmail(from, subject, body string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	fmt.Fprintf(&buf, "Date: Mon, 01 Jan 2024 12:00:00 +0000\r\n")
	fmt.Fprintf(&buf, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&buf, "\r\n")
	fmt.Fprintf(&buf, "%s", body)
	return buf.Bytes()
}

// makeMultipartEmail builds a multipart/alternative message with text and HTML parts.
func makeMultipartEmail(from, subject, textBody, htmlBody string) []byte {
	boundary := "test-boundary-123"
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	fmt.Fprintf(&buf, "Date: Mon, 01 Jan 2024 12:00:00 +0000\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%s\r\n", boundary)
	fmt.Fprintf(&buf, "\r\n")
	fmt.Fprintf(&buf, "--%s\r\n", boundary)
	fmt.Fprintf(&buf, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&buf, "\r\n")
	fmt.Fprintf(&buf, "%s\r\n", textBody)
	fmt.Fprintf(&buf, "--%s\r\n", boundary)
	fmt.Fprintf(&buf, "Content-Type: text/html; charset=utf-8\r\n")
	fmt.Fprintf(&buf, "\r\n")
	fmt.Fprintf(&buf, "%s\r\n", htmlBody)
	fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	return buf.Bytes()
}

func setupTestConnector(t *testing.T, mock GmailClient, cfg Config, rules []connector.Rule) (*GmailConnector, *mockCheckpoint, string) {
	t.Helper()

	stagingDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "gmail")
	if err != nil {
		t.Fatalf("create staging writer: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)
	cp := newMockCheckpoint()

	c := &GmailConnector{
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
		if e.IsDir() {
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
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(stagingDir, e.Name()))
		}
	}
	return dirs
}

// --- tests ---

func TestPollFetchesMessages(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("alice@example.com", "Hello", "Body one"), Sender: "alice@example.com", Subject: "Hello", InternalDate: 1704110400000},
			{ID: "msg2", Raw: makeRawEmail("bob@example.com", "Meeting", "Body two"), Sender: "bob@example.com", Subject: "Meeting", InternalDate: 1704110401000},
			{ID: "msg3", Raw: makeRawEmail("carol@example.com", "Update", "Body three"), Sender: "carol@example.com", Subject: "Update", InternalDate: 1704110402000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
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
		if meta["source"] != "gmail" {
			t.Errorf("expected source=gmail, got %v", meta["source"])
		}
		if meta["destination_agent"] != "messaging" {
			t.Errorf("expected destination_agent=messaging, got %v", meta["destination_agent"])
		}
		if meta["content_type"] != "text/plain" {
			t.Errorf("expected content_type=text/plain, got %v", meta["content_type"])
		}
	}

	// Verify all three messages were fetched individually.
	if len(mock.getCalls) != 3 {
		t.Errorf("expected 3 GetMessage calls, got %d", len(mock.getCalls))
	}
}

func TestMIMEDecoding(t *testing.T) {
	raw := makeMultipartEmail("sender@example.com", "Multipart", "Plain text content", "<p>HTML content</p>")

	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: raw, Sender: "sender@example.com", Subject: "Multipart", InternalDate: 1704110400000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
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

	content := readStagingContent(t, dirs[0])
	if !strings.Contains(content, "Plain text content") {
		t.Errorf("expected content to contain plain text, got: %s", content)
	}
	if !strings.Contains(content, "HTML content") {
		t.Errorf("expected content to contain HTML text body, got: %s", content)
	}
}

func TestCheckpointDedup(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("a@example.com", "Old", "old body"), Sender: "a@example.com", Subject: "Old", InternalDate: 1704110400000},
			{ID: "msg2", Raw: makeRawEmail("b@example.com", "Also old", "also old"), Sender: "b@example.com", Subject: "Also old", InternalDate: 1704110401000},
			{ID: "msg3", Raw: makeRawEmail("c@example.com", "New", "new body"), Sender: "c@example.com", Subject: "New", InternalDate: 1704110500000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	// Pre-set checkpoint: messages at or before epoch second 1704110401 are already processed.
	// InternalDate 1704110401000 ms -> epoch second 1704110401
	cp.Save("internaldate:INBOX", "1704110401000")

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staging item (only msg3), got %d", count)
	}

	// Verify checkpoint advanced to latest internalDate.
	val, ok := cp.Load("internaldate:INBOX")
	if !ok {
		t.Fatal("expected checkpoint internaldate:INBOX to be set")
	}
	if val != "1704110500000" {
		t.Errorf("expected checkpoint=1704110500000, got %s", val)
	}
}

func TestCheckpointAdvancesToLatest(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("a@example.com", "A", "body a"), Sender: "a@example.com", Subject: "A", InternalDate: 1704110400000},
			{ID: "msg2", Raw: makeRawEmail("b@example.com", "B", "body b"), Sender: "b@example.com", Subject: "B", InternalDate: 1704110500000},
			{ID: "msg3", Raw: makeRawEmail("c@example.com", "C", "body c"), Sender: "c@example.com", Subject: "C", InternalDate: 1704110600000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
	}
	c, cp, _ := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	val, ok := cp.Load("internaldate:INBOX")
	if !ok {
		t.Fatal("expected checkpoint internaldate:INBOX to be set")
	}
	if val != "1704110600000" {
		t.Errorf("expected checkpoint=1704110600000, got %s", val)
	}
}

func TestIdentityInStagedMetadata(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("alice@example.com", "Hello", "Body"), Sender: "alice@example.com", Subject: "Hello", InternalDate: 1704110400000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
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
	if identity["provider"] != "google" {
		t.Errorf("expected provider=google, got %v", identity["provider"])
	}
	if identity["auth_method"] != "oauth" {
		t.Errorf("expected auth_method=oauth, got %v", identity["auth_method"])
	}
}

func TestRuleTagsInStagedMetadata(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("alice@example.com", "Hello", "Body"), Sender: "alice@example.com", Subject: "Hello", InternalDate: 1704110400000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{
			Match:       "label:INBOX",
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

func TestLabelRouting(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("a@example.com", "Inbox msg", "inbox body"), Sender: "a@example.com", Subject: "Inbox msg", InternalDate: 1704110400000},
		},
		"IMPORTANT": {
			{ID: "msg2", Raw: makeRawEmail("b@example.com", "Important msg", "important body"), Sender: "b@example.com", Subject: "Important msg", InternalDate: 1704110401000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX", "IMPORTANT"},
		MaxResults: 25,
	}
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
		{Match: "label:IMPORTANT", Destination: "priority"},
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
	if !destinations["priority"] {
		t.Error("expected a staging item routed to priority")
	}
}

func TestNoLabelRuleSkipsLabel(t *testing.T) {
	mock := newMockGmailClient(map[string][]GmailMessage{
		"INBOX": {
			{ID: "msg1", Raw: makeRawEmail("a@example.com", "A", "body"), Sender: "a@example.com", Subject: "A", InternalDate: 1704110400000},
		},
		"SPAM": {
			{ID: "msg2", Raw: makeRawEmail("b@example.com", "B", "body"), Sender: "b@example.com", Subject: "B", InternalDate: 1704110401000},
		},
	})

	cfg := Config{
		LabelIDs:   []string{"INBOX", "SPAM"},
		MaxResults: 25,
	}
	// Only INBOX has a rule; SPAM should be skipped.
	rules := []connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, cfg, rules)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staging item (INBOX only), got %d", count)
	}
}
