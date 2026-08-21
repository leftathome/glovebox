package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// RulesProvenance describes the ruleset a process is actually enforcing.
//
// The rules file is the single place where every boundary in the service is
// defined, and it arrives as a mounted ConfigMap. Anyone who can edit that
// ConfigMap can weaken every check at once -- most simply by raising
// quarantine_threshold past anything the rules can score -- and the change
// leaves no trace: the daemon logs a rule count and carries on. This type
// makes the enforced ruleset identifiable and recordable.
type RulesProvenance struct {
	// SHA256 of the rules file exactly as read.
	SHA256 string `json:"sha256"`
	// RuleCount and QuarantineThreshold summarise the loaded config.
	RuleCount           int     `json:"rule_count"`
	QuarantineThreshold float64 `json:"quarantine_threshold"`
	// MaxAchievableScore is the highest total the ruleset can produce:
	// every non-booster weight summed, multiplied by every booster.
	MaxAchievableScore float64 `json:"max_achievable_score"`
	// ThresholdReachable is false when no combination of rules can reach
	// the threshold -- a ruleset that cannot quarantine anything.
	ThresholdReachable bool `json:"threshold_reachable"`
	// Signature records what the signature check concluded. Always
	// present, including when verification is off: "no signature field"
	// and "signature not checked" must not look the same to whoever
	// reads audit/ruleset.jsonl a year from now.
	Signature RulesSignature `json:"signature"`
}

// RulesSignature is the outcome of the ruleset signature check.
//
// It rides on RulesProvenance so that the one audit record already
// written at startup answers both halves of the question that matters
// after an incident: which rules was this process enforcing, and were
// they signed by someone entitled to write them.
type RulesSignature struct {
	// Mode is the configured policy: disabled, permissive or required.
	Mode string `json:"mode"`
	// Verified is true only when a signature checked out against a
	// trusted key. False under mode=disabled, and false when a
	// permissive start tolerated a missing signature.
	Verified bool `json:"verified"`
	// KeyFingerprint names the key that verified the signature.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// SignatureFile and PublicKeyFile record where the material was
	// read from, so a later reader can tell which key file was in force.
	SignatureFile string `json:"signature_file,omitempty"`
	PublicKeyFile string `json:"public_key_file,omitempty"`
	// TrustedKeys is how many keys the public key file offered. More
	// than one is normal only during a key roll; a deployment that
	// stayed at two keys forgot to retire one.
	TrustedKeys int `json:"trusted_keys,omitempty"`
	// Warning is set when verification was skipped in a way the
	// operator asked for but should not stay in (permissive with no
	// signature deployed yet).
	Warning string `json:"warning,omitempty"`
	// Error is why verification failed. When it is set the process
	// refuses to enforce this ruleset; the record exists so that the
	// refusal is explainable from the audit log rather than only from
	// whatever captured stderr.
	Error string `json:"error,omitempty"`
}

// RulesDigest returns the hex SHA-256 of raw rules bytes.
func RulesDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MaxAchievableScore returns the highest score the ruleset can produce.
//
// Booster rules contribute their multiplier rather than their weight (see
// ScoreSignals), so the ceiling is the sum of the scored weights times the
// product of the boosters.
func MaxAchievableScore(rc RuleConfig) float64 {
	total := 0.0
	boost := 1.0
	for _, r := range rc.Rules {
		if r.Behavior == "weight_booster" {
			if r.BoostFactor > 0 {
				boost *= r.BoostFactor
			}
			continue
		}
		total += r.Weight
	}
	return total * boost
}

// SignaturePolicy is how a caller wants the ruleset's signature treated.
//
// The zero value is mode=disabled: no key is read, no signature file is
// looked for, and LoadRulesFileWithPolicy behaves exactly as
// LoadRulesFile always has.
type SignaturePolicy struct {
	// Mode is SigModeDisabled (default), SigModePermissive or
	// SigModeRequired.
	Mode string
	// PublicKeyFile holds the Ed25519 key(s) trusted to sign rulesets.
	//
	// A PATH, not an inline key, and that is the whole point. The chart
	// renders rules.json and config.json into the SAME ConfigMap, so a
	// key pasted into the config would sit in the very object this
	// feature exists to distrust: an attacker with edit rights would
	// swap the rules, the signature and the key together and every
	// check would pass. A path lets the key be mounted from a separate
	// object with its own RBAC (the chart mounts a Secret), which is
	// the only arrangement in which the check means anything.
	PublicKeyFile string
	// SignatureFile is the detached signature. Empty means
	// <rules file>.sig.
	SignatureFile string
}

// Active reports whether any verification is performed.
func (p SignaturePolicy) Active() bool {
	return p.Mode == SigModePermissive || p.Mode == SigModeRequired
}

// EffectiveSignatureFile resolves the detached signature path for a
// given rules file.
func (p SignaturePolicy) EffectiveSignatureFile(rulesFile string) string {
	if p.SignatureFile != "" {
		return p.SignatureFile
	}
	return rulesFile + ".sig"
}

// LoadRulesFile reads, parses and fingerprints a rules file, without
// checking any signature.
//
// Reading the bytes once and hashing exactly what was parsed matters: a
// digest taken from a second read could describe a different file than the
// one in force.
func LoadRulesFile(path string) (RuleConfig, RulesProvenance, error) {
	return LoadRulesFileWithPolicy(path, SignaturePolicy{})
}

// LoadRulesFileWithPolicy loads a rules file and applies a signature
// policy to it.
//
// On failure the RuleConfig returned is the zero value -- a caller that
// ignores the error gets an empty, unusable ruleset rather than the
// attacker's -- but the RulesProvenance is fully populated, including
// Signature.Error. That asymmetry is deliberate: the caller is expected
// to write the provenance to audit/ruleset.jsonl and THEN exit, so that a
// refused boot leaves a record of exactly which ruleset was refused and
// why. A rejection that only reaches stderr is a rejection nobody can
// reconstruct later.
func LoadRulesFileWithPolicy(path string, policy SignaturePolicy) (RuleConfig, RulesProvenance, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RuleConfig{}, RulesProvenance{}, fmt.Errorf("read rules file %s: %w", path, err)
	}
	rc, err := LoadRulesBytes(raw)
	if err != nil {
		return RuleConfig{}, RulesProvenance{}, err
	}
	max := MaxAchievableScore(rc)
	prov := RulesProvenance{
		SHA256:              RulesDigest(raw),
		RuleCount:           len(rc.Rules),
		QuarantineThreshold: rc.QuarantineThreshold,
		MaxAchievableScore:  max,
		ThresholdReachable:  max >= rc.QuarantineThreshold,
		Signature:           RulesSignature{Mode: effectiveSigMode(policy.Mode)},
	}
	if !policy.Active() {
		return rc, prov, nil
	}

	status, err := verifyRulesSignature(raw, path, policy)
	prov.Signature = status
	if err != nil {
		return RuleConfig{}, prov, err
	}
	return rc, prov, nil
}

func effectiveSigMode(mode string) string {
	if mode == "" {
		return SigModeDisabled
	}
	return mode
}

// verifyRulesSignature runs the signature check and reports both the
// audit-facing status and the fatal error, if any.
func verifyRulesSignature(raw []byte, rulesFile string, policy SignaturePolicy) (RulesSignature, error) {
	sigFile := policy.EffectiveSignatureFile(rulesFile)
	status := RulesSignature{
		Mode:          policy.Mode,
		SignatureFile: sigFile,
		PublicKeyFile: policy.PublicKeyFile,
	}

	fail := func(err error) (RulesSignature, error) {
		status.Error = err.Error()
		return status, err
	}

	if policy.PublicKeyFile == "" {
		return fail(fmt.Errorf("rules signature verification is %q but no public key file is configured", policy.Mode))
	}
	keys, err := LoadPublicKeys(policy.PublicKeyFile)
	if err != nil {
		return fail(err)
	}
	status.TrustedKeys = len(keys)

	sigRaw, err := os.ReadFile(sigFile)
	if err != nil {
		if os.IsNotExist(err) && policy.Mode == SigModePermissive {
			// The rollout state: the key is deployed, the signature
			// is not yet. Tolerated, loudly, and recorded as
			// unverified -- not as verified-with-a-caveat.
			status.Warning = fmt.Sprintf("no ruleset signature at %s and mode is %q: the ruleset is UNVERIFIED; move to %q once signatures are deployed",
				sigFile, SigModePermissive, SigModeRequired)
			return status, nil
		}
		return fail(fmt.Errorf("read ruleset signature %s: %w", sigFile, err))
	}

	env, err := ParseSignatureEnvelope(sigRaw)
	if err != nil {
		return fail(err)
	}
	fingerprint, err := VerifyRuleset(raw, env, keys)
	if err != nil {
		return fail(err)
	}
	status.Verified = true
	status.KeyFingerprint = fingerprint
	return status, nil
}
