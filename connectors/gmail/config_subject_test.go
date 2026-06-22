package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// TestConfigJSON_NeutralPublicDefaults guards the committed connectors/gmail
// config.json (glovebox-0nzk): the PUBLIC connector config must NOT bake a
// specific person's entity_id. data_subject_default ships empty so an
// unconfigured install falls to the safe household audience; the real owner's
// entity_id is supplied per deployment via operator values. A regression that
// re-introduces a baked subject (or a subject-relative audience without one)
// would both leak identity and route a stranger's mail to it.
func TestConfigJSON_NeutralPublicDefaults(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}

	if cfg.DataSubjectDefault != "" {
		t.Errorf("data_subject_default = %q, want \"\" (public default must not bake an entity_id)", cfg.DataSubjectDefault)
	}
	// With no subject, the audience must stay at the household default and must
	// not use subject-relative tokens (which require a data_subject).
	if len(cfg.AudienceDefault) != 1 || cfg.AudienceDefault[0] != "household" {
		t.Errorf("audience_default = %v, want [household]", cfg.AudienceDefault)
	}

	// The defaults must pass the same startup validation the runner applies.
	if err := connector.ValidateBaseConfig(&cfg.BaseConfig); err != nil {
		t.Errorf("ValidateBaseConfig: %v", err)
	}
}
