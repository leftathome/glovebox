package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/engine"
)

// The whole operator flow, against the ruleset this repository actually
// ships. An unusable signing tool is not a security control, and the way
// it becomes unusable is by drifting from the loader it feeds.
func TestOperatorFlow_KeygenSignVerifyShippedRules(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "ruleset-signing.key.pem")
	pubPath := filepath.Join(dir, "rules.pub")

	if err := keygen([]string{"-private", privPath, "-public", pubPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	// The private key is the entire trust anchor; a world-readable one
	// on a shared operator box is a finding in itself.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode %04o, want 0600", perm)
	}

	// Refusing to clobber matters: overwriting a signing key silently
	// invalidates every signature made with it.
	if err := keygen([]string{"-private", privPath, "-public", pubPath}); err == nil {
		t.Error("keygen overwrote an existing private key without -force")
	}

	rulesPath := filepath.Join(dir, "rules.json")
	shipped, err := os.ReadFile("../../configs/default-rules.json")
	if err != nil {
		t.Fatalf("read shipped rules: %v", err)
	}
	if err := os.WriteFile(rulesPath, shipped, 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}

	if err := sign([]string{"-rules", rulesPath, "-private", privPath}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verify([]string{"-rules", rulesPath, "-public", pubPath}); err != nil {
		t.Fatalf("verify rejected what sign just produced: %v", err)
	}
	if err := fingerprint([]string{"-public", pubPath}); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	// The signature the tool writes is the one the daemon reads.
	if _, prov, err := engine.LoadRulesFileWithPolicy(rulesPath, engine.SignaturePolicy{
		Mode:          engine.SigModeRequired,
		PublicKeyFile: pubPath,
	}); err != nil {
		t.Fatalf("the daemon's loader rejected a freshly signed ruleset: %v", err)
	} else if !prov.Signature.Verified {
		t.Fatal("loader reported the ruleset unverified")
	}

	// And one flipped byte breaks it.
	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	raw[len(raw)/2] ^= 0x01
	if err := os.WriteFile(rulesPath, raw, 0o600); err != nil {
		t.Fatalf("write tampered rules: %v", err)
	}
	if err := verify([]string{"-rules", rulesPath, "-public", pubPath}); err == nil {
		t.Fatal("verify accepted a tampered ruleset")
	}
}

// Signing a ruleset the daemon would refuse to parse only moves the
// failure to whenever the pod next restarts.
func TestSign_RefusesInvalidRuleset(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "k.pem")
	pubPath := filepath.Join(dir, "k.pub")
	if err := keygen([]string{"-private", privPath, "-public", pubPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	bad := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(bad, []byte(`{"rules": [], "quarantine_threshold": 0.8}`), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	err := sign([]string{"-rules", bad, "-private", privPath})
	if err == nil {
		t.Fatal("signed an empty ruleset")
	}
	if !strings.Contains(err.Error(), "refusing to sign") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(bad + ".sig"); statErr == nil {
		t.Error("a signature was written for a ruleset that was refused")
	}
}

func TestSubcommands_RequireTheirFlags(t *testing.T) {
	for name, fn := range map[string]func([]string) error{
		"keygen":      keygen,
		"sign":        sign,
		"verify":      verify,
		"fingerprint": fingerprint,
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(nil); err == nil {
				t.Fatalf("%s ran with no flags", name)
			}
		})
	}
}
