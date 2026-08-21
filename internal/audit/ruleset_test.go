package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/engine"
)

func TestLogRuleset_WritesProvenance(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()

	entry := RulesetEntry{
		Timestamp: "2026-08-21T00:00:00Z",
		Event:     "ruleset_loaded",
		RulesFile: "/etc/glovebox/rules.json",
		Pinned:    true,
		Rules: engine.RulesProvenance{
			SHA256:              strings.Repeat("a", 64),
			RuleCount:           7,
			QuarantineThreshold: 0.8,
			MaxAchievableScore:  5.4,
			ThresholdReachable:  true,
		},
	}
	if err := l.LogRuleset(entry); err != nil {
		t.Fatalf("LogRuleset: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "ruleset.jsonl"))
	if err != nil {
		t.Fatalf("read ruleset.jsonl: %v", err)
	}
	var got RulesetEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal entry: %v (raw %q)", err, data)
	}
	if got.Rules.SHA256 != entry.Rules.SHA256 {
		t.Errorf("sha256 = %q, want %q", got.Rules.SHA256, entry.Rules.SHA256)
	}
	if got.Rules.RuleCount != 7 || got.Rules.QuarantineThreshold != 0.8 {
		t.Errorf("provenance did not round-trip: %+v", got.Rules)
	}
	if !got.Pinned {
		t.Error("pinned flag did not round-trip")
	}
}

// A provenance write failure must not take the scanner down or trip
// degraded mode: nothing has been scanned yet, and refusing to run over
// bookkeeping would be a self-inflicted outage.
func TestLogRuleset_DoesNotTripDegradedMode(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()

	if err := l.LogRuleset(RulesetEntry{Event: "ruleset_loaded"}); err != nil {
		t.Fatalf("LogRuleset: %v", err)
	}
	if l.InDegradedMode() {
		t.Error("logging a ruleset entry put the logger in degraded mode")
	}
}

// Verdict logging must be unaffected by the new file.
func TestLogRuleset_CoexistsWithVerdictLogs(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()

	if err := l.LogRuleset(RulesetEntry{Event: "ruleset_loaded"}); err != nil {
		t.Fatalf("LogRuleset: %v", err)
	}
	if err := l.LogPass(PassEntry{AuditEntry: AuditEntry{Verdict: "pass"}}); err != nil {
		t.Fatalf("LogPass: %v", err)
	}
	if err := l.LogReject(RejectEntry{AuditEntry: AuditEntry{Verdict: "quarantine"}, Reason: "threshold_exceeded"}); err != nil {
		t.Fatalf("LogReject: %v", err)
	}
	for _, name := range []string{"ruleset.jsonl", "pass.jsonl", "rejected.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}
