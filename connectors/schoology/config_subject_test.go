package schoology

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// TestConfigJSON_PerKidEntityIDs guards the committed connectors/schoology
// config.json (glovebox-qlkv): per-kid rules must stamp each child's opaque
// entity_id in data_subject (not a raw name/label), so glovebox's subject gate
// accepts them via the registered-entity_id pass-through (spec 15 §5.2 step 3)
// and downstream items never carry a human-identifying subject.
func TestConfigJSON_PerKidEntityIDs(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}

	// Expected data_subject + audience per match key (the connector's actual
	// match keys; k1=e_333333, k2=e_444444 -- synthetic example entity_ids).
	wantSubject := map[string]string{
		"schoology:k1:assignment": "e_333333",
		"schoology:k1:feed":       "e_333333",
		"schoology:k1:attachment": "e_333333",
		"schoology:k2:assignment": "e_444444",
		"schoology:k2:feed":       "e_444444",
		"schoology:k2:attachment": "e_444444",
	}
	wantAudience := map[string][]string{
		"schoology:k1:assignment": {"household"},
		"schoology:k1:feed":       {"subject", "guardians"},
		"schoology:k1:attachment": {"subject", "guardians"},
		"schoology:k2:assignment": {"household"},
		"schoology:k2:feed":       {"subject", "guardians"},
		"schoology:k2:attachment": {"subject", "guardians"},
	}

	seen := make(map[string]bool)
	for _, r := range cfg.Rules {
		if wantDS, ok := wantSubject[r.Match]; ok {
			seen[r.Match] = true
			if r.DataSubject != wantDS {
				t.Errorf("rule %q: data_subject = %q, want %q", r.Match, r.DataSubject, wantDS)
			}
			if !equalStrings(r.Audience, wantAudience[r.Match]) {
				t.Errorf("rule %q: audience = %v, want %v", r.Match, r.Audience, wantAudience[r.Match])
			}
		}
		// Every rule's audience must be individually valid: subject/siblings
		// tokens are only legal when the rule sets a data_subject.
		if err := staging.ValidateAudience(r.Audience, r.DataSubject != ""); err != nil {
			t.Errorf("rule %q: invalid audience %v: %v", r.Match, r.Audience, err)
		}
	}

	for match := range wantSubject {
		if !seen[match] {
			t.Errorf("config.json missing expected per-kid rule %q", match)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
