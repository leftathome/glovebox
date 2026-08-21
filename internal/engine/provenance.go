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

// LoadRulesFile reads, parses and fingerprints a rules file.
//
// Reading the bytes once and hashing exactly what was parsed matters: a
// digest taken from a second read could describe a different file than the
// one in force.
func LoadRulesFile(path string) (RuleConfig, RulesProvenance, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RuleConfig{}, RulesProvenance{}, fmt.Errorf("read rules file %s: %w", path, err)
	}
	rc, err := LoadRulesBytes(raw)
	if err != nil {
		return RuleConfig{}, RulesProvenance{}, err
	}
	max := MaxAchievableScore(rc)
	return rc, RulesProvenance{
		SHA256:              RulesDigest(raw),
		RuleCount:           len(rc.Rules),
		QuarantineThreshold: rc.QuarantineThreshold,
		MaxAchievableScore:  max,
		ThresholdReachable:  max >= rc.QuarantineThreshold,
	}, nil
}
