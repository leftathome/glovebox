package schoology

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	schoologylib "github.com/leftathome/schoology-go"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
)

// messageMatcher is a small ergonomic helper that builds a RuleMatcher
// with two rules: the messages routing rule and a parse-failure
// catch-all so receipts have somewhere to land in tests that exercise
// the error path.
func messageMatcher() *connector.RuleMatcher {
	return connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:message", Destination: "school", Audience: []string{"guardians"}},
		{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
	})
}

// readMessageItems walks stagingDir and returns the parsed metadata for
// every committed item directory. Hidden directories (.tmp etc.) are
// skipped. Test-local helper -- intentionally distinct from any
// readStagedItems helper that may exist in sibling test files.
func readMessageItems(t *testing.T, stagingDir string) []staging.ItemMetadata {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var out []staging.ItemMetadata
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		raw, rErr := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if rErr != nil {
			t.Fatalf("read metadata.json for %s: %v", e.Name(), rErr)
		}
		var meta staging.ItemMetadata
		if jErr := json.Unmarshal(raw, &meta); jErr != nil {
			t.Fatalf("parse metadata.json for %s: %v", e.Name(), jErr)
		}
		out = append(out, meta)
	}
	return out
}

// sampleThread returns a fully-populated MessageThread with the given id
// and subject. LastActivity is a fixed date so timestamp comparisons in
// tests stay deterministic.
func sampleThread(id int64, subject string) *schoologylib.MessageThread {
	return &schoologylib.MessageThread{
		ID:           id,
		Subject:      subject,
		SenderName:   "Ms. Teacher",
		SenderUID:    7777,
		LastActivity: time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC),
		Unread:       true,
		MessageCount: 3,
		URL:          "/messages/thread/" + subject,
	}
}

func TestProcessMessages_StagesNewThreads(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := messageMatcher()
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	threads := []*schoologylib.MessageThread{
		sampleThread(101, "Field trip permission"),
		sampleThread(202, "Reading group update"),
	}
	client := &fakeClient{
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return threads, nil, nil
		},
	}

	staged, errsLogged := ProcessMessages(context.Background(), client, writer, matcher, cp, dedup, "v0.1.0")

	if staged != 2 {
		t.Errorf("staged = %d, want 2", staged)
	}
	if errsLogged {
		t.Errorf("errsLogged = true, want false")
	}
	if got := stagingDirCount(t, stagingDir); got != 2 {
		t.Errorf("staging dir has %d items, want 2", got)
	}
	last, err := LastSeenID(cp, "message", "")
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if last != 202 {
		t.Errorf("checkpoint = %d, want 202", last)
	}

	items := readMessageItems(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("readMessageItems len = %d, want 2", len(items))
	}
	threadTags := make(map[string]bool)
	for _, m := range items {
		if m.DataSubject != "" {
			t.Errorf("DataSubject = %q, want empty", m.DataSubject)
		}
		if id, ok := m.Tags["thread_id"]; ok {
			threadTags[id] = true
		} else {
			t.Errorf("thread_id tag missing on %+v", m.Tags)
		}
	}
	if !threadTags["101"] || !threadTags["202"] {
		t.Errorf("expected thread_id tags 101 and 202, got %v", threadTags)
	}
}

func TestProcessMessages_DedupesAcrossPolls(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := messageMatcher()
	cp := newTestCheckpoint(t)

	threads := []*schoologylib.MessageThread{sampleThread(500, "Half-day reminder")}
	client := &fakeClient{
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return threads, nil, nil
		},
	}

	first, _ := ProcessMessages(context.Background(), client, writer, matcher, cp, NewReceiptDedup(), "v0.1.0")
	if first != 1 {
		t.Fatalf("first call staged %d, want 1", first)
	}

	second, errsLogged := ProcessMessages(context.Background(), client, writer, matcher, cp, NewReceiptDedup(), "v0.1.0")
	if second != 0 {
		t.Errorf("second call staged %d, want 0 (dedup)", second)
	}
	if errsLogged {
		t.Errorf("errsLogged = true on duplicate poll, want false")
	}
	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Errorf("staging dir has %d items, want 1", got)
	}
}

func TestProcessMessages_LibraryErrorEmitsReceipt(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := messageMatcher()
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	client := &fakeClient{
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, &schoologylib.Error{
				Code:    schoologylib.ErrCodeAuth,
				Op:      "GetInbox",
				Message: "session expired",
			}
		},
	}

	staged, errsLogged := ProcessMessages(context.Background(), client, writer, matcher, cp, dedup, "v0.1.0")
	if staged != 0 {
		t.Errorf("staged = %d, want 0", staged)
	}
	if !errsLogged {
		t.Errorf("errsLogged = false, want true on library error")
	}

	items := readMessageItems(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("staging items = %d, want 1 (receipt)", len(items))
	}
	receipt := items[0]
	if receipt.Source != "schoology-parse-failure" {
		t.Errorf("receipt Source = %q, want %q", receipt.Source, "schoology-parse-failure")
	}
	if receipt.Tags["parser"] != "messages" {
		t.Errorf("receipt parser tag = %q, want %q", receipt.Tags["parser"], "messages")
	}
	if receipt.Tags["error_class"] != "auth" {
		t.Errorf("receipt error_class tag = %q, want %q", receipt.Tags["error_class"], "auth")
	}
}

func TestProcessMessages_ZeroIDSkipped(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := messageMatcher()
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	zero := sampleThread(0, "missing id row")
	threads := []*schoologylib.MessageThread{zero}
	client := &fakeClient{
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return threads, nil, nil
		},
	}

	staged, errsLogged := ProcessMessages(context.Background(), client, writer, matcher, cp, dedup, "v0.1.0")
	if staged != 0 {
		t.Errorf("staged = %d, want 0", staged)
	}
	if errsLogged {
		t.Errorf("errsLogged = true, want false (zero-id is a quiet skip)")
	}
	if got := stagingDirCount(t, stagingDir); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
	last, _ := LastSeenID(cp, "message", "")
	if last != 0 {
		t.Errorf("checkpoint advanced to %d on zero-id skip, want 0", last)
	}
}

func TestProcessMessages_ParseErrorsEmitReceipt(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := messageMatcher()
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	good := sampleThread(900, "Good row")
	threads := []*schoologylib.MessageThread{good}
	perrs := schoologylib.ParseErrors{
		schoologylib.NewParseError("parseInbox: tr", "missing subject"),
		schoologylib.NewParseError("parseInbox: tr", "missing subject"),
	}
	client := &fakeClient{
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return threads, perrs, nil
		},
	}

	staged, errsLogged := ProcessMessages(context.Background(), client, writer, matcher, cp, dedup, "v0.1.0")
	if staged != 1 {
		t.Errorf("staged = %d, want 1", staged)
	}
	if !errsLogged {
		t.Errorf("errsLogged = false, want true (parse errors should log)")
	}

	items := readMessageItems(t, stagingDir)
	var receipts, threadItems int
	for _, m := range items {
		switch m.Source {
		case "schoology-parse-failure":
			receipts++
		case "schoology":
			threadItems++
		}
	}
	if receipts != 1 {
		t.Errorf("got %d receipt items, want exactly 1 (dedup by parser/errorClass)", receipts)
	}
	if threadItems != 1 {
		t.Errorf("got %d thread items, want 1", threadItems)
	}
}

func TestProcessMessages_AudienceFromRule(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:message", Destination: "school", Audience: []string{"guardians"}},
	})
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	threads := []*schoologylib.MessageThread{sampleThread(1234, "Parent night")}
	client := &fakeClient{
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return threads, nil, nil
		},
	}

	staged, _ := ProcessMessages(context.Background(), client, writer, matcher, cp, dedup, "v0.1.0")
	if staged != 1 {
		t.Fatalf("staged = %d, want 1", staged)
	}

	items := readMessageItems(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("readMessageItems len = %d, want 1", len(items))
	}
	m := items[0]
	if m.DataSubject != "" {
		t.Errorf("DataSubject = %q, want empty (spec §7.3)", m.DataSubject)
	}
	if len(m.Audience) != 1 || m.Audience[0] != "guardians" {
		t.Errorf("Audience = %v, want [guardians]", m.Audience)
	}
	if m.DestinationAgent != "school" {
		t.Errorf("DestinationAgent = %q, want %q", m.DestinationAgent, "school")
	}
}

// Sanity guard: confirm sampleThread covers fields the processor reads.
// If the schoology-go type drifts, this test fails loudly rather than
// the harder-to-diagnose downstream check.
func TestProcessMessages_SampleThreadFieldsCovered(t *testing.T) {
	tr := sampleThread(1, "x")
	if tr.SenderName == "" || tr.Subject == "" || tr.LastActivity.IsZero() {
		t.Fatal("sampleThread must populate SenderName, Subject, LastActivity for the processor")
	}
}
