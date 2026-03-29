package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

func testItem() staging.StagingItem {
	return staging.StagingItem{
		DirPath:     "/staging/20260328-test",
		ContentPath: "/staging/20260328-test/content.raw",
		Metadata: staging.ItemMetadata{
			Source:           "email",
			Sender:           "alice@example.com",
			Subject:          "Re: meeting notes",
			Timestamp:        time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC),
			DestinationAgent: "messaging",
			ContentType:      "text/plain",
		},
	}
}

func testScanResult() engine.ScanResult {
	return engine.ScanResult{
		Signals: []engine.Signal{
			{Name: "instruction_override", Weight: 1.0, Matched: "ignore previous"},
			{Name: "suspicious_encoding", Weight: 0.7, Matched: "base64 block"},
		},
		TotalScore: 1.7,
		Verdict:    engine.VerdictQuarantine,
	}
}

func TestWriteQuarantineNotification_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	err := WriteQuarantineNotification("20260328-150405-abc123", testItem(), testScanResult(), 1234, dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 notification file, got %d", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Errorf("notification file should end with .json: %s", entries[0].Name())
	}
}

func TestWriteQuarantineNotification_CorrectSchema(t *testing.T) {
	dir := t.TempDir()
	WriteQuarantineNotification("20260328-150405-abc123", testItem(), testScanResult(), 1234, dir)

	entries, _ := os.ReadDir(dir)
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))

	var n QuarantineNotification
	if err := json.Unmarshal(data, &n); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if n.Source != "email" {
		t.Errorf("source = %q, want email", n.Source)
	}
	if n.Sender != "alice@example.com" {
		t.Errorf("sender = %q, want alice@example.com", n.Sender)
	}
	if len(n.Signals) != 2 {
		t.Errorf("signals len = %d, want 2", len(n.Signals))
	}
	if n.TotalScore != 1.7 {
		t.Errorf("total_score = %f, want 1.7", n.TotalScore)
	}
	if n.ContentLength != 1234 {
		t.Errorf("content_length = %d, want 1234", n.ContentLength)
	}
}

func TestWriteQuarantineNotification_NoRawContent(t *testing.T) {
	dir := t.TempDir()
	WriteQuarantineNotification("20260328-150405-abc123", testItem(), testScanResult(), 1234, dir)

	entries, _ := os.ReadDir(dir)
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	s := string(data)
	if strings.Contains(s, "content.raw") {
		t.Error("notification should not contain raw content references")
	}
	if strings.Contains(s, "ignore previous") {
		t.Error("notification should contain signal names only, not matched content")
	}
}

func TestWriteQuarantineNotification_MultipleCreateSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	WriteQuarantineNotification("20260328-150405-aaa111", testItem(), testScanResult(), 100, dir)
	WriteQuarantineNotification("20260328-150406-bbb222", testItem(), testScanResult(), 200, dir)

	entries, _ := os.ReadDir(dir)
	if len(entries) < 2 {
		t.Errorf("expected 2 separate notification files, got %d", len(entries))
	}
}
