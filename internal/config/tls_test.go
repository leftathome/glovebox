package config

import "testing"

func TestIngestTLSConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		tls     IngestTLSConfig
		wantErr bool
	}{
		{name: "disabled by default", tls: IngestTLSConfig{}},
		{name: "explicit disabled", tls: IngestTLSConfig{Mode: TLSModeDisabled}},
		{name: "unknown mode", tls: IngestTLSConfig{Mode: "sorta"}, wantErr: true},
		{
			name:    "permissive without cert",
			tls:     IngestTLSConfig{Mode: TLSModePermissive, ClientCAFile: "/ca.crt"},
			wantErr: true,
		},
		{
			// Without a client CA there is no client verification at all:
			// the endpoint would look secured and authenticate nobody.
			name:    "required without client CA",
			tls:     IngestTLSConfig{Mode: TLSModeRequired, CertFile: "/tls.crt", KeyFile: "/tls.key"},
			wantErr: true,
		},
		{
			name: "permissive complete",
			tls:  IngestTLSConfig{Mode: TLSModePermissive, CertFile: "/tls.crt", KeyFile: "/tls.key", ClientCAFile: "/ca.crt"},
		},
		{
			name: "permissive sharing the plaintext port",
			tls: IngestTLSConfig{
				Mode: TLSModePermissive, Port: 9091,
				CertFile: "/tls.crt", KeyFile: "/tls.key", ClientCAFile: "/ca.crt",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.tls
			err := cfg.validate(9091)
			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestIngestTLSConfig_PortDefaultsBesidePlaintext(t *testing.T) {
	cfg := IngestTLSConfig{Mode: TLSModeRequired, CertFile: "/c", KeyFile: "/k", ClientCAFile: "/ca"}
	if err := cfg.validate(9091); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Port != 9092 {
		t.Errorf("Port = %d, want 9092 (ingest.port+1)", cfg.Port)
	}
}

func TestIngestTLSConfig_Modes(t *testing.T) {
	disabled := IngestTLSConfig{Mode: TLSModeDisabled}
	if disabled.Active() || !disabled.PlaintextActive() {
		t.Error("disabled must serve plaintext only")
	}
	permissive := IngestTLSConfig{Mode: TLSModePermissive}
	if !permissive.Active() || !permissive.PlaintextActive() {
		t.Error("permissive must serve both listeners")
	}
	required := IngestTLSConfig{Mode: TLSModeRequired}
	if !required.Active() || required.PlaintextActive() {
		t.Error("required must serve mTLS only")
	}
}

// Enforcement is the reason mTLS is worth doing: it must default ON
// whenever mTLS is active, so nobody ends up with an encrypted endpoint
// that still trusts whatever source the caller claims.
func TestIngestTLSConfig_SourceMatchDefaultsOn(t *testing.T) {
	on := IngestTLSConfig{Mode: TLSModeRequired}
	if !on.SourceMatchEnforced() {
		t.Error("source matching must default on when mTLS is active")
	}

	off := false
	explicit := IngestTLSConfig{Mode: TLSModeRequired, EnforceSourceMatch: &off}
	if explicit.SourceMatchEnforced() {
		t.Error("explicit false must be honoured")
	}

	disabled := IngestTLSConfig{Mode: TLSModeDisabled}
	if disabled.SourceMatchEnforced() {
		t.Error("no peer exists when TLS is disabled, so nothing can be enforced")
	}
}

func TestIngestTLSConfig_TrustDomainDefault(t *testing.T) {
	if got := (IngestTLSConfig{}).EffectiveTrustDomain(); got != "glovebox" {
		t.Errorf("EffectiveTrustDomain() = %q, want glovebox", got)
	}
	if got := (IngestTLSConfig{TrustDomain: "example.org"}).EffectiveTrustDomain(); got != "example.org" {
		t.Errorf("EffectiveTrustDomain() = %q, want example.org", got)
	}
}
