package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
)

// Camera events say who was where and when, and may carry a face-recognition
// label (see the detectedName handling in event.go). Spec 11 section 3.6 makes
// an item with no declared audience household-visible -- readable by every
// resident's agent -- so shipping this connector without a default is a
// disclosure by omission, not a missing nicety.
//
// This asserts the shipped sample config declares one, and that it is narrower
// than household.
func TestShippedConfigDeclaresANarrowAudience(t *testing.T) {
	raw, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg struct {
		AudienceDefault []string `json:"audience_default"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}

	if len(cfg.AudienceDefault) == 0 {
		t.Fatal("config.json declares no audience_default, so every camera event is household-visible by omission (spec 11 section 3.6)")
	}
	for _, tok := range cfg.AudienceDefault {
		if tok == staging.AudienceHousehold {
			t.Errorf("audience_default contains %q, which is the disclosure this default exists to prevent", tok)
		}
		if tok == staging.AudiencePublic {
			t.Errorf("audience_default contains %q; camera events are never public", tok)
		}
		if tok == staging.AudienceGuardians {
			t.Errorf("audience_default contains %q: that is the whole class of responsible adults, "+
				"which in a household with a live dispute includes the person someone may need "+
				"protection from", tok)
		}
	}

	// It must also be a legal audience for items that carry no data_subject,
	// which is every item this connector stages today.
	if err := staging.ValidateAudience(cfg.AudienceDefault, false); err != nil {
		t.Errorf("audience_default is not valid without a data_subject: %v", err)
	}
}

// The declaration has to reach metadata.json, since that file is the entire
// contract with anything downstream that reads audience.
func TestStagedEventCarriesTheConfiguredAudience(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	// What connector.Run does from BaseConfig.AudienceDefault at boot.
	c.writer.SetConfigAudience([]string{staging.AudienceOperator})

	w := newProtectWatcher(c)
	if err := w.handleFrame([]byte(frameEnd), nil, quietLogger()); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}

	got := staging.EffectiveAudience(items[0].Meta)
	if len(got) != 1 || got[0] != staging.AudienceOperator {
		t.Errorf("effective audience = %v, want [operator]; household by omission is the bug this guards", got)
	}
}

// A rule-level audience reaches metadata.json only if the connector passes
// RuleAudience in ItemOptions. This connector originally did not, so an
// operator who set `audience` on a rule would have had it silently discarded
// and the item would have fallen back to the config default -- or, with none,
// to household. Every other rule-driven connector passes it.
func TestRuleAudienceIsNotSilentlyDiscarded(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, []connector.Rule{{
		Match:       "*",
		Destination: "messaging",
		Audience:    []string{staging.AudienceCaregivers},
	}})
	// A config-wide default the rule must win over.
	c.writer.SetConfigAudience([]string{staging.AudienceOperator})

	w := newProtectWatcher(c)
	if err := w.handleFrame([]byte(frameEnd), nil, quietLogger()); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}
	got := staging.EffectiveAudience(items[0].Meta)
	if len(got) != 1 || got[0] != staging.AudienceCaregivers {
		t.Errorf("effective audience = %v, want [caregivers]: the rule-level audience was discarded", got)
	}
}
