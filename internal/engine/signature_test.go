package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signedRuleset writes a rules file and a detached signature for it and
// returns (rulesPath, publicKeyPath).
func signedRuleset(t *testing.T, body string, priv ed25519.PrivateKey, trust ed25519.PublicKey) (string, string) {
	t.Helper()
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(rulesPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	env, err := SignRuleset([]byte(body), priv)
	if err != nil {
		t.Fatalf("SignRuleset: %v", err)
	}
	sigBytes, err := MarshalSignature(env)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}
	if err := os.WriteFile(rulesPath+".sig", sigBytes, 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	pubPEM, err := MarshalPublicKey(trust)
	if err != nil {
		t.Fatalf("MarshalPublicKey: %v", err)
	}
	pubPath := filepath.Join(dir, "rules.pub")
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return rulesPath, pubPath
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func requiredPolicy(pubPath string) SignaturePolicy {
	return SignaturePolicy{Mode: SigModeRequired, PublicKeyFile: pubPath}
}

func TestVerify_ValidSignatureAccepted(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)

	rc, prov, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath))
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if len(rc.Rules) != 2 {
		t.Fatalf("rules not loaded: got %d rules", len(rc.Rules))
	}
	if !prov.Signature.Verified {
		t.Fatalf("Signature.Verified = false, want true (%+v)", prov.Signature)
	}
	if got, want := prov.Signature.KeyFingerprint, KeyFingerprint(pub); got != want {
		t.Errorf("KeyFingerprint = %q, want %q", got, want)
	}
	if prov.Signature.Mode != SigModeRequired {
		t.Errorf("Mode = %q, want %q", prov.Signature.Mode, SigModeRequired)
	}
	if prov.Signature.TrustedKeys != 1 {
		t.Errorf("TrustedKeys = %d, want 1", prov.Signature.TrustedKeys)
	}
	if prov.Signature.Error != "" {
		t.Errorf("Error = %q, want empty", prov.Signature.Error)
	}
}

// TestVerify_TamperedRulesetRejected is the attack this whole feature
// exists for: someone with ConfigMap-edit rights rewrites rules.json to
// weaken a boundary. One flipped byte must be fatal.
func TestVerify_TamperedRulesetRejected(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)

	original, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	// Flip one byte in the middle of the file. It stays valid JSON and a
	// valid ruleset -- the point is that the daemon must reject it on the
	// signature alone, not because it happened to become unparseable.
	tampered := append([]byte(nil), original...)
	idx := strings.Index(string(tampered), "\"weight\": 1.0")
	if idx < 0 {
		t.Fatalf("fixture changed: no weight to flip")
	}
	flipAt := idx + len("\"weight\": ")
	if tampered[flipAt] != '1' {
		t.Fatalf("expected '1' at %d, got %q", flipAt, tampered[flipAt])
	}
	tampered[flipAt] = '0' // weight 1.0 -> 0.0: the rule stops mattering
	if string(tampered) == string(original) {
		t.Fatal("tamper was a no-op")
	}
	if _, err := LoadRulesBytes(tampered); err != nil {
		t.Fatalf("tampered ruleset should still parse (otherwise this test proves nothing): %v", err)
	}
	if err := os.WriteFile(rulesPath, tampered, 0o600); err != nil {
		t.Fatalf("write tampered rules: %v", err)
	}

	rc, prov, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath))
	if err == nil {
		t.Fatal("tampered ruleset ACCEPTED -- the signature check does not work")
	}
	if len(rc.Rules) != 0 {
		t.Errorf("rejected load returned %d rules; must return none", len(rc.Rules))
	}
	if prov.Signature.Verified {
		t.Error("Signature.Verified = true on a tampered ruleset")
	}
	if prov.Signature.Error == "" {
		t.Error("Signature.Error empty on a tampered ruleset: the audit record would not explain the refusal")
	}
	// The digest cross-check is what fires first, and the message must
	// name both digests so an operator can tell which file moved.
	if !strings.Contains(err.Error(), "signature covers sha256") {
		t.Errorf("unhelpful rejection message: %v", err)
	}
	// Provenance still records the digest of what was refused.
	if prov.SHA256 != RulesDigest(tampered) {
		t.Errorf("provenance digest %s does not describe the refused file", prov.SHA256)
	}

	// Permissive must reject it too: a signature that does not check out
	// is the attack, not a missing signature.
	if _, _, err := LoadRulesFileWithPolicy(rulesPath, SignaturePolicy{Mode: SigModePermissive, PublicKeyFile: pubPath}); err == nil {
		t.Fatal("tampered ruleset accepted under permissive mode")
	}
}

// TestVerify_TamperedSignatureRejected covers the other half of the same
// edit: an attacker who rewrites the rules also rewrites the sidecar
// signature so its digest matches.
func TestVerify_ResignedByAttackerRejected(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)

	weakened := strings.Replace(minimalRules, `"quarantine_threshold": 0.8`, `"quarantine_threshold": 2.0`, 1)
	if weakened == minimalRules {
		t.Fatal("fixture changed: threshold not replaced")
	}
	if err := os.WriteFile(rulesPath, []byte(weakened), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	// The attacker holds no signing key, so the best they can do is
	// re-sign with a key of their own.
	_, attackerPriv := keypair(t)
	env, err := SignRuleset([]byte(weakened), attackerPriv)
	if err != nil {
		t.Fatalf("SignRuleset: %v", err)
	}
	sigBytes, err := MarshalSignature(env)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}
	if err := os.WriteFile(rulesPath+".sig", sigBytes, 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}

	_, prov, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath))
	if err == nil {
		t.Fatal("attacker-signed ruleset accepted")
	}
	if !strings.Contains(err.Error(), "does not verify against any of the 1 trusted key") {
		t.Errorf("unexpected rejection message: %v", err)
	}
	if prov.Signature.Verified {
		t.Error("Signature.Verified = true for an attacker-signed ruleset")
	}
}

func TestVerify_WrongKeyRejected(t *testing.T) {
	_, priv := keypair(t)
	otherPub, _ := keypair(t)
	// Signed by priv, but the deployment trusts otherPub.
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, otherPub)

	_, prov, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath))
	if err == nil {
		t.Fatal("signature from an untrusted key was accepted")
	}
	if prov.Signature.Verified {
		t.Error("Signature.Verified = true under the wrong key")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestVerify_MissingSignatureRejectedWhenRequired(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)
	if err := os.Remove(rulesPath + ".sig"); err != nil {
		t.Fatalf("remove signature: %v", err)
	}

	_, prov, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath))
	if err == nil {
		t.Fatal("unsigned ruleset accepted under mode=required")
	}
	if prov.Signature.Verified {
		t.Error("Signature.Verified = true with no signature file")
	}
	if !strings.Contains(err.Error(), "read ruleset signature") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Permissive tolerates an ABSENT signature -- the rollout state -- but
// must record the ruleset as unverified and say so.
func TestVerify_MissingSignatureWarnsWhenPermissive(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)
	if err := os.Remove(rulesPath + ".sig"); err != nil {
		t.Fatalf("remove signature: %v", err)
	}

	rc, prov, err := LoadRulesFileWithPolicy(rulesPath, SignaturePolicy{Mode: SigModePermissive, PublicKeyFile: pubPath})
	if err != nil {
		t.Fatalf("permissive mode should tolerate a missing signature: %v", err)
	}
	if len(rc.Rules) != 2 {
		t.Fatalf("rules not loaded: %d", len(rc.Rules))
	}
	if prov.Signature.Verified {
		t.Error("Signature.Verified = true with no signature at all")
	}
	if prov.Signature.Warning == "" {
		t.Error("no warning recorded for an unverified permissive start")
	}
}

func TestVerify_MissingPublicKeyRejected(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, _ := signedRuleset(t, minimalRules, priv, pub)

	// Mode set, key file path points nowhere: fail closed, do not
	// quietly degrade to "no verification".
	_, _, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(filepath.Join(t.TempDir(), "absent.pub")))
	if err == nil {
		t.Fatal("missing public key file was accepted")
	}
	// And with no key configured at all.
	_, _, err = LoadRulesFileWithPolicy(rulesPath, SignaturePolicy{Mode: SigModeRequired})
	if err == nil {
		t.Fatal("mode=required with no public key file was accepted")
	}
	if !strings.Contains(err.Error(), "no public key file is configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The compatibility guarantee: with verification off, nothing about
// loading changes and nothing on disk is even looked at.
func TestVerify_DisabledMatchesLegacyBehaviour(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, _ := signedRuleset(t, minimalRules, priv, pub)
	// Remove the signature entirely; disabled mode must not care.
	if err := os.Remove(rulesPath + ".sig"); err != nil {
		t.Fatalf("remove signature: %v", err)
	}

	legacyRC, legacyProv, legacyErr := LoadRulesFile(rulesPath)
	if legacyErr != nil {
		t.Fatalf("LoadRulesFile: %v", legacyErr)
	}
	zeroRC, zeroProv, zeroErr := LoadRulesFileWithPolicy(rulesPath, SignaturePolicy{})
	if zeroErr != nil {
		t.Fatalf("zero policy: %v", zeroErr)
	}
	explicitRC, explicitProv, explicitErr := LoadRulesFileWithPolicy(rulesPath, SignaturePolicy{Mode: SigModeDisabled})
	if explicitErr != nil {
		t.Fatalf("explicit disabled: %v", explicitErr)
	}

	if len(legacyRC.Rules) != len(zeroRC.Rules) || len(legacyRC.Rules) != len(explicitRC.Rules) {
		t.Fatal("rule count differs between disabled paths")
	}
	for _, prov := range []RulesProvenance{legacyProv, zeroProv, explicitProv} {
		if prov.SHA256 != RulesDigest([]byte(minimalRules)) {
			t.Errorf("digest changed under disabled mode: %s", prov.SHA256)
		}
		if prov.Signature.Mode != SigModeDisabled {
			t.Errorf("Signature.Mode = %q, want %q", prov.Signature.Mode, SigModeDisabled)
		}
		if prov.Signature.Verified {
			t.Error("Signature.Verified = true under disabled mode")
		}
		// Nothing was consulted, so nothing may be reported.
		if prov.Signature.PublicKeyFile != "" || prov.Signature.SignatureFile != "" || prov.Signature.Error != "" {
			t.Errorf("disabled mode reported material it never read: %+v", prov.Signature)
		}
	}
	// A stale/garbage signature file must also be ignored outright.
	if err := os.WriteFile(rulesPath+".sig", []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("write junk signature: %v", err)
	}
	if _, _, err := LoadRulesFile(rulesPath); err != nil {
		t.Fatalf("disabled mode read a signature file it should have ignored: %v", err)
	}
}

func TestVerify_SignatureFromAnotherRulesetRejected(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)

	// A different but validly signed ruleset's signature, moved over.
	other := strings.Replace(minimalRules, "instruction_override", "something_else", 1)
	env, err := SignRuleset([]byte(other), priv)
	if err != nil {
		t.Fatalf("SignRuleset: %v", err)
	}
	sigBytes, err := MarshalSignature(env)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}
	if err := os.WriteFile(rulesPath+".sig", sigBytes, 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	if _, _, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath)); err == nil {
		t.Fatal("a signature issued for a different ruleset was accepted")
	}
}

func TestVerify_MalformedSignatureFileRejected(t *testing.T) {
	pub, priv := keypair(t)
	rulesPath, pubPath := signedRuleset(t, minimalRules, priv, pub)
	digest := RulesDigest([]byte(minimalRules))

	cases := map[string]string{
		"not json":         "{",
		"wrong algorithm":  `{"algorithm":"rsa","key_id":"x","sha256":"` + digest + `","signature":"AAAA"}`,
		"short sha":        `{"algorithm":"ed25519","key_id":"x","sha256":"abc","signature":"AAAA"}`,
		"sha not hex":      `{"algorithm":"ed25519","key_id":"x","sha256":"` + strings.Repeat("z", 64) + `","signature":"AAAA"}`,
		"sig not base64":   `{"algorithm":"ed25519","key_id":"x","sha256":"` + digest + `","signature":"!!!"}`,
		"sig wrong length": `{"algorithm":"ed25519","key_id":"x","sha256":"` + digest + `","signature":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`,
		"unknown field":    `{"algorithm":"ed25519","key_id":"x","sha256":"` + digest + `","signature":"AAAA","trust_me":true}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(rulesPath+".sig", []byte(body), 0o600); err != nil {
				t.Fatalf("write signature: %v", err)
			}
			if _, _, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath)); err == nil {
				t.Fatalf("malformed signature (%s) accepted", name)
			}
		})
	}
}

// Key rotation: a file holding both the outgoing and the incoming key
// must accept a ruleset signed by either, which is what makes a roll
// possible without a flag day.
func TestParsePublicKeys_MultipleKeysSupportRotation(t *testing.T) {
	oldPub, oldPriv := keypair(t)
	newPub, newPriv := keypair(t)

	oldPEM, err := MarshalPublicKey(oldPub)
	if err != nil {
		t.Fatalf("MarshalPublicKey: %v", err)
	}
	newPEM, err := MarshalPublicKey(newPub)
	if err != nil {
		t.Fatalf("MarshalPublicKey: %v", err)
	}
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "rules.pub")
	if err := os.WriteFile(pubPath, append(oldPEM, newPEM...), 0o600); err != nil {
		t.Fatalf("write keys: %v", err)
	}

	keys, err := LoadPublicKeys(pubPath)
	if err != nil {
		t.Fatalf("LoadPublicKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}

	for name, priv := range map[string]ed25519.PrivateKey{"old": oldPriv, "new": newPriv} {
		rulesPath := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(rulesPath, []byte(minimalRules), 0o600); err != nil {
			t.Fatalf("write rules: %v", err)
		}
		env, err := SignRuleset([]byte(minimalRules), priv)
		if err != nil {
			t.Fatalf("SignRuleset: %v", err)
		}
		sigBytes, err := MarshalSignature(env)
		if err != nil {
			t.Fatalf("MarshalSignature: %v", err)
		}
		if err := os.WriteFile(rulesPath+".sig", sigBytes, 0o600); err != nil {
			t.Fatalf("write signature: %v", err)
		}
		if _, _, err := LoadRulesFileWithPolicy(rulesPath, requiredPolicy(pubPath)); err != nil {
			t.Errorf("%s key rejected during rotation window: %v", name, err)
		}
	}
}

func TestParsePublicKeys_LineFormatAndComments(t *testing.T) {
	pub, _ := keypair(t)
	body := "# glovebox ruleset signing keys\n\n" + base64.StdEncoding.EncodeToString(pub) + "\n"
	keys, err := ParsePublicKeys([]byte(body))
	if err != nil {
		t.Fatalf("ParsePublicKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Fingerprint != KeyFingerprint(pub) {
		t.Fatalf("got %+v, want the one key %s", keys, KeyFingerprint(pub))
	}
}

func TestParsePublicKeys_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"comment only": "# nothing here\n",
		"not base64":   "%%%%\n",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("too short")) + "\n",
		"wrong PEM":    "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePublicKeys([]byte(body)); err == nil {
				t.Fatalf("%s accepted as a key file", name)
			}
		})
	}
}

// The domain prefix is what stops a ruleset signature being reusable as a
// signature over a bare digest somewhere else. If it ever changes,
// existing signatures stop verifying, so pin it.
func TestSigningMessage_IsDomainSeparated(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	got := string(SigningMessage(digest))
	want := "glovebox-ruleset-v1\n" + digest + "\n"
	if got != want {
		t.Fatalf("SigningMessage = %q, want %q", got, want)
	}
	// Case-folding: a digest handed in uppercase must sign the same
	// message the loader will later verify.
	if string(SigningMessage(strings.ToUpper(digest))) != want {
		t.Fatal("SigningMessage is not case-normalised")
	}
}

func TestPrivateKeyRoundTrip(t *testing.T) {
	pub, priv := keypair(t)
	pem, err := MarshalPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	back, err := ParsePrivateKey(pem)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if !back.Equal(priv) {
		t.Fatal("private key did not round-trip")
	}
	if KeyFingerprint(back.Public().(ed25519.PublicKey)) != KeyFingerprint(pub) {
		t.Fatal("fingerprint changed across the round trip")
	}
	if _, err := ParsePrivateKey([]byte("not a pem")); err == nil {
		t.Fatal("garbage accepted as a private key")
	}
}
