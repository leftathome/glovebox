package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalRules = `{
  "rules": [
    {"name": "instruction_override", "patterns": ["ignore previous"], "weight": 1.0, "match_type": "substring"},
    {"name": "tool_syntax", "patterns": ["<tool>"], "weight": 0.8, "match_type": "substring"}
  ],
  "quarantine_threshold": 0.8
}`

func writeRules(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	return p
}

func TestLoadRulesFile_Digest(t *testing.T) {
	p := writeRules(t, minimalRules)

	rc, prov, err := LoadRulesFile(p)
	if err != nil {
		t.Fatalf("LoadRulesFile: %v", err)
	}
	if len(rc.Rules) != 2 {
		t.Errorf("rule count = %d, want 2", len(rc.Rules))
	}
	if want := RulesDigest([]byte(minimalRules)); prov.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q", prov.SHA256, want)
	}
	if len(prov.SHA256) != 64 {
		t.Errorf("digest is not a hex sha256: %q", prov.SHA256)
	}
	if !prov.ThresholdReachable {
		t.Error("threshold 0.8 must be reachable from weights 1.0 + 0.8")
	}
}

// The digest must change when the file does, or pinning it is pointless.
func TestLoadRulesFile_DigestTracksContent(t *testing.T) {
	_, a, err := LoadRulesFile(writeRules(t, minimalRules))
	if err != nil {
		t.Fatalf("LoadRulesFile: %v", err)
	}
	weakened := strings.Replace(minimalRules, `"quarantine_threshold": 0.8`, `"quarantine_threshold": 1.9`, 1)
	_, b, err := LoadRulesFile(writeRules(t, weakened))
	if err != nil {
		t.Fatalf("LoadRulesFile: %v", err)
	}
	if a.SHA256 == b.SHA256 {
		t.Error("digest did not change when the threshold was raised")
	}
}

// The case the review named: raise the threshold past anything the rules can
// score and the scanner silently stops quarantining. It must be detectable.
func TestLoadRulesFile_UnreachableThresholdIsFlagged(t *testing.T) {
	weakened := strings.Replace(minimalRules, `"quarantine_threshold": 0.8`, `"quarantine_threshold": 1.9`, 1)
	_, prov, err := LoadRulesFile(writeRules(t, weakened))
	if err != nil {
		t.Fatalf("LoadRulesFile: %v", err)
	}
	if prov.ThresholdReachable {
		t.Errorf("threshold %.2f reported reachable, but the maximum achievable score is %.2f",
			prov.QuarantineThreshold, prov.MaxAchievableScore)
	}
}

func TestMaxAchievableScore_AccountsForBoosters(t *testing.T) {
	rc := RuleConfig{
		Rules: []Rule{
			{Name: "a", Weight: 1.0},
			{Name: "b", Weight: 0.5},
			{Name: "boost", Weight: 0.0, Behavior: "weight_booster", BoostFactor: 1.5},
		},
		QuarantineThreshold: 2.0,
	}
	// (1.0 + 0.5) * 1.5 = 2.25
	if got := MaxAchievableScore(rc); got < 2.24 || got > 2.26 {
		t.Errorf("MaxAchievableScore = %.2f, want 2.25", got)
	}
	if !(MaxAchievableScore(rc) >= rc.QuarantineThreshold) {
		t.Error("threshold 2.0 should be reachable with the booster applied")
	}
}

func TestLoadRulesFile_MissingFile(t *testing.T) {
	if _, _, err := LoadRulesFile("/nonexistent/rules.json"); err == nil {
		t.Error("expected an error for a missing rules file")
	}
}

func TestLoadRulesFile_InvalidJSON(t *testing.T) {
	if _, _, err := LoadRulesFile(writeRules(t, "{not json")); err == nil {
		t.Error("expected an error for malformed rules JSON")
	}
}
