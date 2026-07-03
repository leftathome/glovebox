// Package scan is the single compiled-rule scan path shared by the async
// scanner daemon and the synchronous /v1/sanitize gate. Building the matchers
// and detectors here (once) guarantees the gate enforces exactly what the
// daemon enforces.
package scan

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
)

type Scanner struct {
	matchers    []engine.ScanFunc
	detectors   []engine.ScanFunc
	boostConfig map[string]float64
	threshold   float64
}

func New(rules engine.RuleConfig, registry *detector.Registry) (*Scanner, error) {
	matchers, detectors, err := buildScanFuncs(rules, registry)
	if err != nil {
		return nil, err
	}
	boostConfig := make(map[string]float64)
	for _, rule := range rules.Rules {
		if rule.Behavior == "weight_booster" {
			boostConfig[rule.Name] = rule.BoostFactor
		}
	}
	return &Scanner{matchers: matchers, detectors: detectors, boostConfig: boostConfig, threshold: rules.QuarantineThreshold}, nil
}

func (s *Scanner) Scan(content []byte, contentType string) (engine.ScanResult, error) {
	pp := engine.Preprocess(content, contentType)
	signals, err := engine.ScanContent(bytes.NewReader(pp.Normalized), s.matchers, s.detectors)
	if err != nil {
		return engine.ScanResult{}, fmt.Errorf("scan normalized: %w", err)
	}
	if pp.RawHTML != nil {
		htmlSignals, err := engine.ScanContent(bytes.NewReader(pp.RawHTML), s.matchers, nil)
		if err != nil {
			return engine.ScanResult{}, fmt.Errorf("scan raw html: %w", err)
		}
		signals = appendDeduped(signals, htmlSignals)
	}
	// Separate boost signals (weight_booster rules) from scored signals, matching
	// deliverResult (main.go): a boost signal contributes its multiplier only,
	// NOT its own weight, to the total. Replicate the separation so a future
	// non-zero booster can't silently change the score.
	var scored []engine.Signal
	var boosts []engine.BoostRule
	for _, sig := range signals {
		if factor, ok := s.boostConfig[sig.Name]; ok {
			boosts = append(boosts, engine.BoostRule{Name: sig.Name, BoostFactor: factor})
			continue
		}
		scored = append(scored, sig)
	}
	return engine.ScoreSignals(scored, boosts, s.threshold), nil
}

// Accessors so Task 2 can rewire the async worker/deliverResult onto this one
// build path without duplicating rule compilation.
func (s *Scanner) Matchers() []engine.ScanFunc  { return s.matchers }
func (s *Scanner) Detectors() []engine.ScanFunc { return s.detectors }
func (s *Scanner) Threshold() float64           { return s.threshold }
func (s *Scanner) Boosts() map[string]float64   { return s.boostConfig }

func makeMatcherScanFunc(m engine.Matcher, rule engine.Rule) engine.ScanFunc {
	return func(content []byte) ([]engine.Signal, error) {
		results, err := m.Match(content, rule.Patterns)
		if err != nil || len(results) == 0 {
			return nil, err
		}
		matched := make([]string, len(results))
		for i, r := range results {
			matched[i] = fmt.Sprintf("%s at %d", r.Pattern, r.Position)
		}
		return []engine.Signal{{
			Name:    rule.Name,
			Weight:  rule.Weight,
			Matched: strings.Join(matched, "; "),
		}}, nil
	}
}

// buildScanFuncs compiles the configured rules into matcher and detector scan
// funcs. Unlike main.go's original (which called log.Fatalf), this returns an
// error on bad regex or unknown detector: a library must never exit the
// process.
func buildScanFuncs(rules engine.RuleConfig, registry *detector.Registry) ([]engine.ScanFunc, []engine.ScanFunc, error) {
	var matchers []engine.ScanFunc
	var detectors []engine.ScanFunc

	for _, rule := range rules.Rules {
		rule := rule
		switch rule.MatchType {
		case engine.MatchSubstring:
			matchers = append(matchers, makeMatcherScanFunc(engine.SubstringMatcher{}, rule))

		case engine.MatchSubstringCaseInsensitive:
			matchers = append(matchers, makeMatcherScanFunc(engine.CaseInsensitiveMatcher{}, rule))

		case engine.MatchRegex:
			m, err := engine.NewRegexMatcher(rule.Patterns)
			if err != nil {
				return nil, nil, fmt.Errorf("compile regex for rule %s: %w", rule.Name, err)
			}
			matchers = append(matchers, makeMatcherScanFunc(m, rule))

		case engine.MatchCustomDetector:
			d, ok := registry.Get(rule.Detector)
			if !ok {
				return nil, nil, fmt.Errorf("unknown detector %q for rule %s", rule.Detector, rule.Name)
			}
			detectors = append(detectors, func(content []byte) ([]engine.Signal, error) {
				signals, err := d.Detect(content)
				if err != nil {
					return nil, err
				}
				// Override signal name and weight with rule config values
				for i := range signals {
					signals[i].Name = rule.Name
					signals[i].Weight = rule.Weight
				}
				return signals, nil
			})
		}
	}

	return matchers, detectors, nil
}

// appendDeduped adds signals from src to dst, skipping signals with the
// same name that already exist in dst (avoids double-counting from dual scan).
func appendDeduped(dst, src []engine.Signal) []engine.Signal {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]bool, len(dst))
	for _, s := range dst {
		seen[s.Name] = true
	}
	for _, s := range src {
		if !seen[s.Name] {
			dst = append(dst, s)
		}
	}
	return dst
}
