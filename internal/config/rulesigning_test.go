package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/engine"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// The compatibility guarantee: a config that says nothing about rule
// signing must produce exactly the pre-existing behaviour -- verification
// off, nothing to read, nothing that can fail.
func TestRulesSigning_DefaultsOff(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"rules_file": "/etc/rules.json"}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RulesSigning.Mode != "" {
		t.Errorf("rules_signing.mode = %q, want empty (disabled)", cfg.RulesSigning.Mode)
	}
	if cfg.RulesSigning.Active() {
		t.Error("rules_signing.Active() = true for an unconfigured install")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a config with no rules_signing block: %v", err)
	}
	if p := cfg.RulesSigning.Policy(); p.Active() {
		t.Errorf("Policy().Active() = true for an unconfigured install: %+v", p)
	}
}

func TestRulesSigning_ParsesAndValidates(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{
		"rules_file": "/etc/glovebox/rules.json",
		"rules_signing": {
			"mode": "required",
			"public_key_file": "/etc/glovebox-rules-key/rules.pub",
			"signature_file": "/etc/glovebox/rules.json.sig"
		}
	}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.RulesSigning.Active() {
		t.Error("Active() = false for mode=required")
	}
	p := cfg.RulesSigning.Policy()
	if p.Mode != engine.SigModeRequired {
		t.Errorf("Policy().Mode = %q", p.Mode)
	}
	if p.PublicKeyFile != "/etc/glovebox-rules-key/rules.pub" {
		t.Errorf("Policy().PublicKeyFile = %q", p.PublicKeyFile)
	}
	if got := p.EffectiveSignatureFile(cfg.RulesFile); got != "/etc/glovebox/rules.json.sig" {
		t.Errorf("EffectiveSignatureFile = %q", got)
	}
}

func TestRulesSigning_DefaultSignatureFileIsSidecar(t *testing.T) {
	p := engine.SignaturePolicy{Mode: engine.SigModeRequired, PublicKeyFile: "/k/rules.pub"}
	if got, want := p.EffectiveSignatureFile("/etc/glovebox/rules.json"), "/etc/glovebox/rules.json.sig"; got != want {
		t.Errorf("EffectiveSignatureFile = %q, want %q", got, want)
	}
}

func TestRulesSigning_Validation(t *testing.T) {
	cases := []struct {
		name    string
		signing RulesSigningConfig
		wantErr string
	}{
		{"disabled explicitly", RulesSigningConfig{Mode: engine.SigModeDisabled}, ""},
		{"permissive with key", RulesSigningConfig{Mode: engine.SigModePermissive, PublicKeyFile: "/k"}, ""},
		{"required with key", RulesSigningConfig{Mode: engine.SigModeRequired, PublicKeyFile: "/k"}, ""},
		{"unknown mode", RulesSigningConfig{Mode: "enforce"}, "must be one of"},
		// Refusing here, at config load, gives a message that names the
		// missing setting; letting it through would refuse every ruleset
		// at boot with an error about signatures instead.
		{"required without key", RulesSigningConfig{Mode: engine.SigModeRequired}, "public_key_file is required"},
		{"permissive without key", RulesSigningConfig{Mode: engine.SigModePermissive}, "public_key_file is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.signing.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// The chart and the Go schema must agree. This fixture is the config.json
// key of the ConfigMap `helm template` renders with signing on:
//
//	helm template gb charts/glovebox \
//	  --set rules.signing.mode=required \
//	  --set rules.signing.publicKeySecret=glovebox-rules-signing-key \
//	  --set-file rules.json=… --set-file rules.signature=…
//
// A camelCase key or a renamed field would leave the Go side silently
// unsigned, which is precisely the failure this feature must not have.
// subjects_file/sources_file are stripped from the fixture, as they are
// from testdata/helm-rendered-ingest-enabled.json: Validate loads those
// registries from disk and the container paths do not exist here.
func TestLoadConfig_HelmRenderedRuleSigning(t *testing.T) {
	cfg, err := LoadConfig("testdata/helm-rendered-rule-signing.json")
	if err != nil {
		t.Fatalf("LoadConfig on helm-rendered fixture: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on helm-rendered fixture: %v", err)
	}
	if cfg.RulesSigning.Mode != engine.SigModeRequired {
		t.Errorf("rules_signing.mode = %q; chart -> Go schema mismatch", cfg.RulesSigning.Mode)
	}
	if !cfg.RulesSigning.Active() {
		t.Error("rules_signing.Active() = false on a chart render with signing on")
	}
	// The mount path is owned by the chart (deployment.yaml) and echoed
	// into config.json by the same chart; if these two ever drift the
	// pod fails closed at boot, so pin the value here.
	if cfg.RulesSigning.PublicKeyFile != "/etc/glovebox-rules-key/rules.pub" {
		t.Errorf("public_key_file = %q, want the chart's mount path /etc/glovebox-rules-key/rules.pub", cfg.RulesSigning.PublicKeyFile)
	}
	// The chart leaves signature_file to the binary's sidecar default.
	if got := cfg.RulesSigning.Policy().EffectiveSignatureFile(cfg.RulesFile); got != "/etc/glovebox/rules.json.sig" {
		t.Errorf("effective signature file = %q, want /etc/glovebox/rules.json.sig (the chart's ConfigMap key)", got)
	}
}

func TestRulesSigning_EnvOverrides(t *testing.T) {
	t.Setenv("GLOVEBOX_RULES_SIGNING_MODE", "permissive")
	t.Setenv("GLOVEBOX_RULES_PUBLIC_KEY_FILE", "/env/rules.pub")
	t.Setenv("GLOVEBOX_RULES_SIGNATURE_FILE", "/env/rules.sig")

	cfg, err := LoadConfig(writeConfig(t, `{"rules_file": "/etc/rules.json"}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RulesSigning.Mode != engine.SigModePermissive {
		t.Errorf("mode = %q", cfg.RulesSigning.Mode)
	}
	if cfg.RulesSigning.PublicKeyFile != "/env/rules.pub" {
		t.Errorf("public_key_file = %q", cfg.RulesSigning.PublicKeyFile)
	}
	if cfg.RulesSigning.SignatureFile != "/env/rules.sig" {
		t.Errorf("signature_file = %q", cfg.RulesSigning.SignatureFile)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
