package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// TestConfigJSON_StevePrivateDefaults guards the committed connectors/gmail
// config.json (glovebox-qlkv): the Gmail account is Steve's, so every item
// must default to Steve's opaque entity_id and a Steve-private audience. A
// regression here would silently broaden the audience of Steve's mail.
func TestConfigJSON_StevePrivateDefaults(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}

	if cfg.DataSubjectDefault != "e_111111" {
		t.Errorf("data_subject_default = %q, want e_111111 (Steve)", cfg.DataSubjectDefault)
	}
	if len(cfg.AudienceDefault) != 1 || cfg.AudienceDefault[0] != "subject" {
		t.Errorf("audience_default = %v, want [subject]", cfg.AudienceDefault)
	}

	// The defaults must pass the same startup validation the runner applies.
	if err := connector.ValidateBaseConfig(&cfg.BaseConfig); err != nil {
		t.Errorf("ValidateBaseConfig: %v", err)
	}
}
