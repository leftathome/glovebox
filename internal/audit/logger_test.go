package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

func TestLogPass_AppendsValidJSONL(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()
	entry := PassEntry{AuditEntry: AuditEntry{
		Timestamp:      "2026-03-28T12:00:00Z",
		Source:         "email",
		Sender:         "alice@example.com",
		ContentHash:    "abc123",
		ContentLength:  100,
		Signals:        []engine.Signal{},
		TotalScore:     0.0,
		Verdict:        "pass",
		Destination:    "messaging",
		ScanDurationMs: 42,
	}}
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
	defer logger.Close()
	entry := RejectEntry{
		AuditEntry: AuditEntry{
			Timestamp:      "2026-03-28T12:00:00Z",
			Source:         "email",
			Sender:         "attacker@evil.com",
			ContentHash:    "def456",
			ContentLength:  500,
			Signals:        []engine.Signal{{Name: "instruction_override", Weight: 1.0, Matched: "ignore previous"}},
			TotalScore:     1.0,
			Verdict:        "quarantine",
			Destination:    "messaging",
			ScanDurationMs: 15,
		},
		Reason: "threshold_exceeded",
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
	defer logger.Close()
	for i := 0; i < 3; i++ {
		logger.LogPass(PassEntry{AuditEntry: AuditEntry{
			Timestamp: "2026-03-28T12:00:00Z",
			Source:    "email",
			Sender:    "a@b.com",
			Verdict:   "pass",
		}})
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
	defer logger.Close()
	logger.LogPass(PassEntry{AuditEntry: AuditEntry{
		Timestamp: "2026-03-28T12:00:00Z",
		Source:    "email",
		Sender:    "has\nnewline@test.com",
		Verdict:   "pass",
	}})
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

func TestNewLogger_FailsOnBadDir(t *testing.T) {
	_, err := NewLogger("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestLogger_NotDegradedInitially(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(dir)
	defer logger.Close()
	if logger.InDegradedMode() {
		t.Error("should not be degraded initially")
	}
}

func TestLogger_DegradedAfterWriteFailure(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(dir)
	// Close the file to force write failure
	logger.passFile.Close()
	logger.LogPass(PassEntry{AuditEntry: AuditEntry{Verdict: "pass"}})
	if !logger.InDegradedMode() {
		t.Error("should be degraded after write failure")
	}
}

func TestAuditEntry_IncludesIdentityAndTags(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	identity := &staging.ItemIdentity{
		AccountID:  "steve@github",
		Provider:   "github",
		AuthMethod: "oauth",
		Scopes:     []string{"repo", "read:org"},
		Tenant:     "steve",
	}
	tags := map[string]string{"team": "platform", "env": "production"}

	entry := PassEntry{AuditEntry: AuditEntry{
		Timestamp:      "2026-03-28T12:00:00Z",
		Source:         "github",
		Sender:         "octocat",
		ContentHash:    "abc123",
		ContentLength:  200,
		Signals:        []engine.Signal{},
		TotalScore:     0.0,
		Verdict:        "pass",
		Destination:    "messaging",
		ScanDurationMs: 10,
		Identity:       identity,
		Tags:           tags,
	}}
	if err := logger.LogPass(entry); err != nil {
		t.Fatalf("LogPass: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "pass.jsonl"))
	var decoded PassEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSONL: %v", err)
	}
	if decoded.Identity == nil {
		t.Fatal("expected identity in audit entry, got nil")
	}
	if decoded.Identity.Provider != "github" {
		t.Errorf("identity.provider = %q, want github", decoded.Identity.Provider)
	}
	if decoded.Identity.AccountID != "steve@github" {
		t.Errorf("identity.account_id = %q, want steve@github", decoded.Identity.AccountID)
	}
	if len(decoded.Identity.Scopes) != 2 {
		t.Errorf("identity.scopes len = %d, want 2", len(decoded.Identity.Scopes))
	}
	if decoded.Tags == nil {
		t.Fatal("expected tags in audit entry, got nil")
	}
	if decoded.Tags["team"] != "platform" {
		t.Errorf("tags[team] = %q, want platform", decoded.Tags["team"])
	}
	if decoded.Tags["env"] != "production" {
		t.Errorf("tags[env] = %q, want production", decoded.Tags["env"])
	}
}

func TestAuditEntry_OmitsIdentityAndTagsWhenNil(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	entry := PassEntry{AuditEntry: AuditEntry{
		Timestamp:      "2026-03-28T12:00:00Z",
		Source:         "rss",
		Sender:         "feed",
		ContentHash:    "abc",
		ContentLength:  50,
		Signals:        []engine.Signal{},
		TotalScore:     0.0,
		Verdict:        "pass",
		Destination:    "media",
		ScanDurationMs: 5,
	}}
	if err := logger.LogPass(entry); err != nil {
		t.Fatalf("LogPass: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "pass.jsonl"))
	// Verify that identity and tags keys are not present in the JSON
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, exists := raw["identity"]; exists {
		t.Error("expected identity to be omitted from JSON when nil")
	}
	if _, exists := raw["tags"]; exists {
		t.Error("expected tags to be omitted from JSON when nil")
	}
}

func TestLogger_DegradedClearsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(dir)
	defer logger.Close()

	// Force degraded by closing and reopening pass file
	logger.passFile.Close()
	logger.LogPass(PassEntry{AuditEntry: AuditEntry{Verdict: "pass"}})
	if !logger.InDegradedMode() {
		t.Fatal("should be degraded")
	}

	// Reopen pass file and write successfully via reject (which uses a different file)
	logger.LogReject(RejectEntry{
		AuditEntry: AuditEntry{Verdict: "reject"},
		Reason:     "test",
	})
	if logger.InDegradedMode() {
		t.Error("should clear degraded after successful write")
	}
}

func TestAuditEntry_DataSubjectAndAudienceRoundtrip(t *testing.T) {
	e := AuditEntry{
		Timestamp:     "2026-04-22T00:00:00Z",
		Source:        "schoology",
		Sender:        "Mr. Rodriguez",
		ContentHash:   "abc",
		ContentLength: 10,
		Verdict:       "pass",
		Destination:   "school",
		DataSubject:   "bee",
		Audience:      []string{"subject", "parents"},
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"data_subject":"bee"`) {
		t.Errorf("missing data_subject in JSON: %s", data)
	}
	if !strings.Contains(string(data), `"audience":["subject","parents"]`) {
		t.Errorf("missing audience in JSON: %s", data)
	}
}

func TestAuditEntry_OmitEmptyForNewFields(t *testing.T) {
	e := AuditEntry{Timestamp: "t", Source: "s", Verdict: "pass", Destination: "d"}
	data, _ := json.Marshal(e)
	if strings.Contains(string(data), "data_subject") {
		t.Errorf("expected data_subject omitted: %s", data)
	}
	if strings.Contains(string(data), "audience") {
		t.Errorf("expected audience omitted: %s", data)
	}
}
