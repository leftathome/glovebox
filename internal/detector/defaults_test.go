package detector

import (
	"os"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/engine"
)

func TestDefaultRulesJSON_LoadsSuccessfully(t *testing.T) {
	f, err := os.Open("../../configs/default-rules.json")
	if err != nil {
		t.Fatalf("open default-rules.json: %v", err)
	}
	defer f.Close()

	rc, err := engine.LoadRules(f)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rc.Rules) != 6 {
		t.Errorf("rules count = %d, want 6", len(rc.Rules))
	}
	if rc.QuarantineThreshold != 0.8 {
		t.Errorf("threshold = %f, want 0.8", rc.QuarantineThreshold)
	}
}

func TestDefaultRulesJSON_RuleNames(t *testing.T) {
	f, _ := os.Open("../../configs/default-rules.json")
	defer f.Close()
	rc, _ := engine.LoadRules(f)

	expectedNames := []string{
		"instruction_override",
		"role_reassignment",
		"tool_invocation_syntax",
		"suspicious_encoding",
		"prompt_template_structure",
		"non_english_content",
	}
	for i, name := range expectedNames {
		if rc.Rules[i].Name != name {
			t.Errorf("rule[%d] name = %q, want %q", i, rc.Rules[i].Name, name)
		}
	}
}

func TestDefaultRegistry_AllDetectorsResolvable(t *testing.T) {
	registry := NewDefaultRegistry()

	detectorNames := []string{"encoding_anomaly", "template_structure", "language_detection"}
	for _, name := range detectorNames {
		d, ok := registry.Get(name)
		if !ok {
			t.Errorf("detector %q not registered", name)
			continue
		}
		if d == nil {
			t.Errorf("detector %q is nil", name)
		}
	}
}

func TestDefaultRegistry_RulesMatchDetectors(t *testing.T) {
	f, _ := os.Open("../../configs/default-rules.json")
	defer f.Close()
	rc, _ := engine.LoadRules(f)
	registry := NewDefaultRegistry()

	for _, rule := range rc.Rules {
		if rule.MatchType != engine.MatchCustomDetector {
			continue
		}
		if _, ok := registry.Get(rule.Detector); !ok {
			t.Errorf("rule %q references detector %q which is not registered", rule.Name, rule.Detector)
		}
	}
}

func TestDefaultRegistry_DetectorsWork(t *testing.T) {
	registry := NewDefaultRegistry()

	d, _ := registry.Get("encoding_anomaly")
	signals, err := d.Detect([]byte("normal text"))
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Errorf("encoding_anomaly should not fire on normal text")
	}

	d, _ = registry.Get("template_structure")
	signals, _ = d.Detect([]byte("You are a helpful assistant"))
	if len(signals) == 0 {
		t.Error("template_structure should fire on 'You are a helpful assistant'")
	}

	d, _ = registry.Get("language_detection")
	signals, _ = d.Detect([]byte("Bonjour, je voudrais vous informer que la reunion de demain est annulee."))
	found := false
	for _, s := range signals {
		if strings.Contains(s.Matched, "French") {
			found = true
		}
	}
	if !found {
		t.Error("language_detection should detect French")
	}
}
