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
	"time"

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

// --- mock IMAP client ---

type mockMessage struct {
	UID     uint32
	Raw     []byte
	Sender  string
	Subject string
	Date    time.Time
}

type mockIMAPClient struct {
	folders   map[string][]mockMessage
	selected  string
	connected bool
	closed    bool
	idleCh    chan struct{}
}

func newMockIMAPClient(folders map[string][]mockMessage) *mockIMAPClient {
	return &mockIMAPClient{
		folders: folders,
		idleCh:  make(chan struct{}),
	}
}

func (m *mockIMAPClient) Connect(ctx context.Context) error {
	m.connected = true
	return nil
}

func (m *mockIMAPClient) SelectFolder(ctx context.Context, folder string) error {
	if _, ok := m.folders[folder]; !ok {
		return fmt.Errorf("folder %q not found", folder)
	}
	m.selected = folder
	return nil
}

func (m *mockIMAPClient) SearchSinceUID(ctx context.Context, uid uint32) ([]uint32, error) {
	msgs, ok := m.folders[m.selected]
	if !ok {
		return nil, fmt.Errorf("no folder selected")
	}
	var result []uint32
	for _, msg := range msgs {
		if msg.UID > uid {
			result = append(result, msg.UID)
		}
	}
	return result, nil
}

func (m *mockIMAPClient) FetchMessage(ctx context.Context, uid uint32) ([]byte, string, string, time.Time, error) {
	msgs, ok := m.folders[m.selected]
	if !ok {
		return nil, "", "", time.Time{}, fmt.Errorf("no folder selected")
	}
	for _, msg := range msgs {
		if msg.UID == uid {
			return msg.Raw, msg.Sender, msg.Subject, msg.Date, nil
		}
	}
	return nil, "", "", time.Time{}, fmt.Errorf("uid %d not found", uid)
}

func (m *mockIMAPClient) Idle(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.idleCh:
		return nil
	}
}

func (m *mockIMAPClient) Close() error {
	m.closed = true
	return nil
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

// setupTestConnector creates an IMAPConnector wired to a mock client and real
// staging writer in a temp directory. Returns the connector, checkpoint, and
// staging dir for assertions.
func setupTestConnector(t *testing.T, mock *mockIMAPClient, rules []connector.Rule, username string) (*IMAPConnector, *mockCheckpoint, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "imap")
	if err != nil {
		t.Fatalf("create staging writer: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)
	cp := newMockCheckpoint()

	var folders []FolderConfig
	for name := range mock.folders {
		folders = append(folders, FolderConfig{Name: name})
	}

	c := &IMAPConnector{
		config: Config{
			Folders: folders,
		},
		writer:       writer,
		matcher:      matcher,
		imapUsername: username,
		newClient: func() IMAPClient {
			return mock
		},
	}

	_ = stateDir // checkpoint is in-memory for tests
	return c, cp, stagingDir
}

// countStagingItems returns the number of item directories written to the staging dir.
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

// readStagingMetadata reads and returns parsed metadata from a staging item directory.
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

// readStagingContent reads and returns the content from a staging item directory.
func readStagingContent(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "content.raw"))
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	return string(data)
}

// stagingItemDirs returns the list of item directory paths in the staging dir.
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
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 1, Raw: makeRawEmail("alice@example.com", "Hello", "Body one"), Sender: "alice@example.com", Subject: "Hello", Date: now},
			{UID: 2, Raw: makeRawEmail("bob@example.com", "Meeting", "Body two"), Sender: "bob@example.com", Subject: "Meeting", Date: now},
			{UID: 3, Raw: makeRawEmail("carol@example.com", "Update", "Body three"), Sender: "carol@example.com", Subject: "Update", Date: now},
		},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, rules, "testuser@example.com")

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staging items, got %d", count)
	}

	// Verify metadata on each item.
	dirs := stagingItemDirs(t, stagingDir)
	if len(dirs) != 3 {
		t.Fatalf("expected 3 item dirs, got %d", len(dirs))
	}

	for _, dir := range dirs {
		meta := readStagingMetadata(t, dir)
		if meta["source"] != "imap" {
			t.Errorf("expected source=imap, got %v", meta["source"])
		}
		if meta["destination_agent"] != "messaging" {
			t.Errorf("expected destination_agent=messaging, got %v", meta["destination_agent"])
		}
		if meta["content_type"] != "text/plain" {
			t.Errorf("expected content_type=text/plain, got %v", meta["content_type"])
		}
	}

	// Verify client was connected and closed.
	if !mock.connected {
		t.Error("expected client to be connected")
	}
	if !mock.closed {
		t.Error("expected client to be closed")
	}
}

func TestCheckpointAdvances(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 10, Raw: makeRawEmail("a@example.com", "A", "body a"), Sender: "a@example.com", Subject: "A", Date: now},
			{UID: 20, Raw: makeRawEmail("b@example.com", "B", "body b"), Sender: "b@example.com", Subject: "B", Date: now},
			{UID: 30, Raw: makeRawEmail("c@example.com", "C", "body c"), Sender: "c@example.com", Subject: "C", Date: now},
		},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	}
	c, cp, _ := setupTestConnector(t, mock, rules, "testuser@example.com")

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	val, ok := cp.Load("uid:INBOX")
	if !ok {
		t.Fatal("expected checkpoint uid:INBOX to be set")
	}
	if val != "30" {
		t.Errorf("expected checkpoint uid:INBOX=30, got %s", val)
	}
}

func TestPollSkipsProcessed(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 1, Raw: makeRawEmail("a@example.com", "Old", "old body"), Sender: "a@example.com", Subject: "Old", Date: now},
			{UID: 2, Raw: makeRawEmail("b@example.com", "Also old", "also old"), Sender: "b@example.com", Subject: "Also old", Date: now},
			{UID: 3, Raw: makeRawEmail("c@example.com", "New", "new body"), Sender: "c@example.com", Subject: "New", Date: now},
		},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, rules, "testuser@example.com")

	// Pre-set checkpoint so UIDs 1 and 2 are already processed.
	cp.Save("uid:INBOX", "2")

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staging item (only UID 3), got %d", count)
	}

	// Verify the single item has the correct sender.
	dirs := stagingItemDirs(t, stagingDir)
	if len(dirs) == 1 {
		meta := readStagingMetadata(t, dirs[0])
		if meta["sender"] != "c@example.com" {
			t.Errorf("expected sender=c@example.com, got %v", meta["sender"])
		}
	}
}

func TestFolderRouting(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 1, Raw: makeRawEmail("a@example.com", "Inbox msg", "inbox body"), Sender: "a@example.com", Subject: "Inbox msg", Date: now},
		},
		"Sent": {
			{UID: 1, Raw: makeRawEmail("me@example.com", "Sent msg", "sent body"), Sender: "me@example.com", Subject: "Sent msg", Date: now},
		},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
		{Match: "folder:Sent", Destination: "archive"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, rules, "testuser@example.com")

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	count := countStagingItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staging items, got %d", count)
	}

	// Check that we have one messaging and one archive destination.
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

func TestMIMEDecoding(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	raw := makeMultipartEmail("sender@example.com", "Multipart", "Plain text content", "<p>HTML content</p>")

	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 1, Raw: raw, Sender: "sender@example.com", Subject: "Multipart", Date: now},
		},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, rules, "testuser@example.com")

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

func TestWatchReturnsOnCancel(t *testing.T) {
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	}
	c, cp, _ := setupTestConnector(t, mock, rules, "testuser@example.com")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- c.Watch(ctx, cp)
	}()

	// Cancel after a brief moment.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("expected nil or context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}

	if !mock.connected {
		t.Error("expected client to be connected")
	}
	if !mock.closed {
		t.Error("expected client to be closed")
	}
}

func TestIdentityInStagedMetadata(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 1, Raw: makeRawEmail("alice@example.com", "Hello", "Body"), Sender: "alice@example.com", Subject: "Hello", Date: now},
		},
	})

	rules := []connector.Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, rules, "steve@homelab.local")

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
	if identity["provider"] != "imap" {
		t.Errorf("expected provider=imap, got %v", identity["provider"])
	}
	if identity["auth_method"] != "app_password" {
		t.Errorf("expected auth_method=app_password, got %v", identity["auth_method"])
	}
	if identity["account_id"] != "steve@homelab.local" {
		t.Errorf("expected account_id=steve@homelab.local, got %v", identity["account_id"])
	}
}

func TestRuleTagsInStagedMetadata(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mock := newMockIMAPClient(map[string][]mockMessage{
		"INBOX": {
			{UID: 1, Raw: makeRawEmail("alice@example.com", "Hello", "Body"), Sender: "alice@example.com", Subject: "Hello", Date: now},
		},
	})

	rules := []connector.Rule{
		{
			Match:       "folder:INBOX",
			Destination: "messaging",
			Tags:        map[string]string{"priority": "high", "category": "personal"},
		},
	}
	c, cp, stagingDir := setupTestConnector(t, mock, rules, "testuser@example.com")

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
