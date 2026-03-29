package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

type MatchType string

const (
	MatchSubstring                MatchType = "substring"
	MatchSubstringCaseInsensitive MatchType = "substring_case_insensitive"
	MatchRegex                    MatchType = "regex"
	MatchCustomDetector           MatchType = "custom_detector"
)

type Rule struct {
	Name        string    `json:"name"`
	Patterns    []string  `json:"patterns"`
	Weight      float64   `json:"weight"`
	MatchType   MatchType `json:"match_type"`
	Detector    string    `json:"detector"`
	Behavior    string    `json:"behavior"`
	BoostFactor float64   `json:"boost_factor"`
}

type RuleConfig struct {
	Rules               []Rule  `json:"rules"`
	QuarantineThreshold float64 `json:"quarantine_threshold"`
}

func LoadRules(r io.Reader) (RuleConfig, error) {
	var rc RuleConfig
	if err := json.NewDecoder(r).Decode(&rc); err != nil {
		return RuleConfig{}, fmt.Errorf("parsing rules JSON: %w", err)
	}
	if err := validateRuleConfig(rc); err != nil {
		return RuleConfig{}, err
	}
	return rc, nil
}

var validMatchTypes = map[MatchType]bool{
	MatchSubstring:                true,
	MatchSubstringCaseInsensitive: true,
	MatchRegex:                    true,
	MatchCustomDetector:           true,
}

func validateRuleConfig(rc RuleConfig) error {
	if len(rc.Rules) == 0 {
		return fmt.Errorf("rules must not be empty")
	}
	if rc.QuarantineThreshold <= 0.0 || rc.QuarantineThreshold > 2.0 {
		return fmt.Errorf("quarantine_threshold must be in (0.0, 2.0], got %f", rc.QuarantineThreshold)
	}

	for i, rule := range rc.Rules {
		if rule.Weight < 0.0 || rule.Weight > 1.0 {
			return fmt.Errorf("rule[%d] %q: weight must be in [0.0, 1.0], got %f", i, rule.Name, rule.Weight)
		}
		if !validMatchTypes[rule.MatchType] {
			return fmt.Errorf("rule[%d] %q: unknown match_type %q", i, rule.Name, rule.MatchType)
		}
		if rule.MatchType == MatchCustomDetector && rule.Detector == "" {
			return fmt.Errorf("rule[%d] %q: custom_detector requires non-empty detector field", i, rule.Name)
		}
		if rule.MatchType == MatchRegex {
			for _, p := range rule.Patterns {
				if _, err := regexp.Compile(p); err != nil {
					return fmt.Errorf("rule[%d] %q: invalid regex pattern %q: %w", i, rule.Name, p, err)
				}
			}
		}
		if rule.Behavior == "weight_booster" {
			if rule.BoostFactor < 1.0 || rule.BoostFactor > 3.0 {
				return fmt.Errorf("rule[%d] %q: boost_factor must be in [1.0, 3.0], got %f", i, rule.Name, rule.BoostFactor)
			}
		}
	}
	return nil
}
