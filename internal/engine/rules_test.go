package engine

import (
	"strings"
	"testing"
)

func TestLoadRules_Valid(t *testing.T) {
	input := `{
		"rules": [
			{"name": "test_rule", "patterns": ["bad"], "weight": 0.5, "match_type": "substring"}
		],
		"quarantine_threshold": 0.8
	}`
	rc, err := LoadRules(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.Rules) != 1 {
		t.Fatalf("rules len = %d, want 1", len(rc.Rules))
	}
	if rc.Rules[0].Name != "test_rule" {
		t.Errorf("name = %q, want test_rule", rc.Rules[0].Name)
	}
	if rc.QuarantineThreshold != 0.8 {
		t.Errorf("threshold = %f, want 0.8", rc.QuarantineThreshold)
	}
}

func TestLoadRules_InvalidWeight(t *testing.T) {
	tests := []struct {
		name   string
		weight string
	}{
		{"negative", "-0.1"},
		{"over_one", "1.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"rules":[{"name":"r","patterns":["x"],"weight":` + tt.weight + `,"match_type":"substring"}],"quarantine_threshold":0.8}`
			_, err := LoadRules(strings.NewReader(input))
			if err == nil {
				t.Fatal("expected error for invalid weight")
			}
		})
	}
}

func TestLoadRules_UnknownMatchType(t *testing.T) {
	input := `{"rules":[{"name":"r","patterns":["x"],"weight":0.5,"match_type":"magic"}],"quarantine_threshold":0.8}`
	_, err := LoadRules(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for unknown match_type")
	}
}

func TestLoadRules_CustomDetectorMissingDetectorField(t *testing.T) {
	input := `{"rules":[{"name":"r","patterns":[],"weight":0.5,"match_type":"custom_detector"}],"quarantine_threshold":0.8}`
	_, err := LoadRules(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for custom_detector without detector field")
	}
}

func TestLoadRules_InvalidThreshold(t *testing.T) {
	tests := []struct {
		name      string
		threshold string
	}{
		{"zero", "0.0"},
		{"negative", "-1.0"},
		{"too_high", "3.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"rules":[{"name":"r","patterns":["x"],"weight":0.5,"match_type":"substring"}],"quarantine_threshold":` + tt.threshold + `}`
			_, err := LoadRules(strings.NewReader(input))
			if err == nil {
				t.Fatalf("expected error for threshold %s", tt.threshold)
			}
		})
	}
}

func TestLoadRules_EmptyRulesRejected(t *testing.T) {
	input := `{"rules":[],"quarantine_threshold":0.8}`
	_, err := LoadRules(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for empty rules")
	}
}

func TestLoadRules_InvalidBoostFactor(t *testing.T) {
	input := `{"rules":[{"name":"r","patterns":[],"weight":0.0,"match_type":"custom_detector","detector":"test","behavior":"weight_booster","boost_factor":5.0}],"quarantine_threshold":0.8}`
	_, err := LoadRules(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for boost_factor > 3.0")
	}
}

func TestLoadRules_InvalidRegexRejected(t *testing.T) {
	input := `{"rules":[{"name":"r","patterns":["[invalid"],"weight":0.5,"match_type":"regex"}],"quarantine_threshold":0.8}`
	_, err := LoadRules(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}
