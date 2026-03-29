package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLogPass_AppendsValidJSONL(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	entry := PassEntry{
		Timestamp:      "2026-03-28T12:00:00Z",
		Source:         "email",
		Sender:         "alice@example.com",
		ContentHash:    "abc123",
		ContentLength:  100,
		Signals:        []SignalEntry{},
		TotalScore:     0.0,
		Verdict:        "pass",
		Destination:    "messaging",
		ScanDurationMs: 42,
	}
	if err := logger.LogPass(entry); err != nil {
		t.Fatalf("LogPass: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pass.jsonl"))
	var decoded PassEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSONL: %v", err)
	}
	if decoded.Source != "email" {
		t.Errorf("source = %q, want email", decoded.Source)
	}
}

func TestLogReject_AppendsValidJSONL(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(dir)
	entry := RejectEntry{
		Timestamp:      "2026-03-28T12:00:00Z",
		Source:         "email",
		Sender:         "attacker@evil.com",
		ContentHash:    "def456",
		ContentLength:  500,
		Signals:        []SignalEntry{{Name: "instruction_override", Weight: 1.0, Matched: "ignore previous"}},
		TotalScore:     1.0,
		Verdict:        "quarantine",
		Reason:         "threshold_exceeded",
		Destination:    "messaging",
		ScanDurationMs: 15,
	}
	if err := logger.LogReject(entry); err != nil {
		t.Fatalf("LogReject: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "rejected.jsonl"))
	var decoded RejectEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSONL: %v", err)
	}
	if decoded.Verdict != "quarantine" {
		t.Errorf("verdict = %q, want quarantine", decoded.Verdict)
	}
	if len(decoded.Signals) != 1 {
		t.Errorf("signals len = %d, want 1", len(decoded.Signals))
	}
}

func TestLogPass_MultipleWrites(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(dir)
	for i := 0; i < 3; i++ {
		logger.LogPass(PassEntry{
			Timestamp: "2026-03-28T12:00:00Z",
			Source:    "email",
			Sender:    "a@b.com",
			Verdict:   "pass",
		})
	}
	f, _ := os.Open(filepath.Join(dir, "pass.jsonl"))
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count++
		var entry PassEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Errorf("line %d: invalid JSON: %v", count, err)
		}
	}
	if count != 3 {
		t.Errorf("line count = %d, want 3", count)
	}
}

func TestLogPass_SingleLinePerEntry(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(dir)
	logger.LogPass(PassEntry{
		Timestamp: "2026-03-28T12:00:00Z",
		Source:    "email",
		Sender:    "has\nnewline@test.com",
		Verdict:   "pass",
	})
	f, _ := os.Open(filepath.Join(dir, "pass.jsonl"))
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}
	if lineCount != 1 {
		t.Errorf("expected 1 line, got %d (newline in field leaked)", lineCount)
	}
}

func TestLogPass_WriteFailureReturnsError(t *testing.T) {
	logger, _ := NewLogger("/nonexistent/path/that/does/not/exist")
	err := logger.LogPass(PassEntry{Verdict: "pass"})
	if err == nil {
		t.Fatal("expected error for write to nonexistent path")
	}
}
