package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/engine"
)

func readRulesetLine(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "ruleset.jsonl"))
	if err != nil {
		t.Fatalf("read ruleset.jsonl: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &raw); err != nil {
		t.Fatalf("unmarshal entry: %v (raw %q)", err, data)
	}
	return raw
}

// "Which rules was this process enforcing, and were they signed" must be
// answerable from audit/ruleset.jsonl alone -- not from stderr, and not
// by going back to the ConfigMap, which is the thing under suspicion.
func TestLogRuleset_RecordsSignatureVerification(t *testing.T) {
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
		Rules: engine.RulesProvenance{
			SHA256:              strings.Repeat("b", 64),
			RuleCount:           7,
			QuarantineThreshold: 0.8,
			Signature: engine.RulesSignature{
				Mode:           engine.SigModeRequired,
				Verified:       true,
				KeyFingerprint: "0123456789abcdef",
				SignatureFile:  "/etc/glovebox/rules.json.sig",
				PublicKeyFile:  "/etc/glovebox-rules-key/rules.pub",
				TrustedKeys:    1,
			},
		},
	}
	if err := l.LogRuleset(entry); err != nil {
		t.Fatalf("LogRuleset: %v", err)
	}

	raw := readRulesetLine(t, dir)
	rules, ok := raw["rules"].(map[string]any)
	if !ok {
		t.Fatalf("no rules object in entry: %v", raw)
	}
	sig, ok := rules["signature"].(map[string]any)
	if !ok {
		t.Fatalf("no signature object in the ruleset entry: %v", rules)
	}
	if sig["verified"] != true {
		t.Errorf("signature.verified = %v, want true", sig["verified"])
	}
	if sig["mode"] != engine.SigModeRequired {
		t.Errorf("signature.mode = %v, want %q", sig["mode"], engine.SigModeRequired)
	}
	if sig["key_fingerprint"] != "0123456789abcdef" {
		t.Errorf("signature.key_fingerprint = %v", sig["key_fingerprint"])
	}
	if sig["public_key_file"] != "/etc/glovebox-rules-key/rules.pub" {
		t.Errorf("signature.public_key_file = %v", sig["public_key_file"])
	}
}

// A refused ruleset must also leave a record. If the only trace of a
// rejected boot were stderr, an operator investigating later would find
// the log silent on the one event that matters most.
func TestLogRuleset_RecordsRejection(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()

	if err := l.LogRuleset(RulesetEntry{
		Timestamp: "2026-08-21T00:00:00Z",
		Event:     "ruleset_rejected",
		RulesFile: "/etc/glovebox/rules.json",
		Warning:   "ruleset signature does not verify",
		Rules: engine.RulesProvenance{
			SHA256: strings.Repeat("c", 64),
			Signature: engine.RulesSignature{
				Mode:  engine.SigModeRequired,
				Error: "ruleset signature covers sha256 aaa... but the rules file is ccc...",
			},
		},
	}); err != nil {
		t.Fatalf("LogRuleset: %v", err)
	}

	raw := readRulesetLine(t, dir)
	if raw["event"] != "ruleset_rejected" {
		t.Errorf("event = %v, want ruleset_rejected", raw["event"])
	}
	sig := raw["rules"].(map[string]any)["signature"].(map[string]any)
	if sig["verified"] != false {
		t.Errorf("signature.verified = %v, want false", sig["verified"])
	}
	if s, _ := sig["error"].(string); s == "" {
		t.Error("signature.error empty: the refusal is not explainable from the log")
	}
}

// With verification off the signature object is still emitted, saying so.
// "Never checked" and "checked and unverified" must not look alike to
// someone reading the log a year from now.
func TestLogRuleset_DisabledIsStillRecorded(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()

	if err := l.LogRuleset(RulesetEntry{
		Event: "ruleset_loaded",
		Rules: engine.RulesProvenance{
			SHA256:    strings.Repeat("d", 64),
			Signature: engine.RulesSignature{Mode: engine.SigModeDisabled},
		},
	}); err != nil {
		t.Fatalf("LogRuleset: %v", err)
	}

	sig := readRulesetLine(t, dir)["rules"].(map[string]any)["signature"].(map[string]any)
	if sig["mode"] != engine.SigModeDisabled {
		t.Errorf("signature.mode = %v, want %q", sig["mode"], engine.SigModeDisabled)
	}
	if sig["verified"] != false {
		t.Errorf("signature.verified = %v, want false", sig["verified"])
	}
	// Fields describing material that was never read must be absent
	// rather than empty-but-present.
	for _, k := range []string{"key_fingerprint", "public_key_file", "signature_file", "error"} {
		if _, present := sig[k]; present {
			t.Errorf("signature.%s present under mode=disabled", k)
		}
	}
}
