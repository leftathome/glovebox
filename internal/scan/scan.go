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

// Scan scans an item's content. Callers that also hold attacker-controlled
// metadata (a Subject line, a Sender) must use ScanWithMetadata instead --
// see the note there for why metadata is part of the attack surface.
func (s *Scanner) Scan(content []byte, contentType string) (engine.ScanResult, error) {
	return s.ScanWithMetadata(content, contentType, nil)
}

// ScanWithMetadata scans an item's content together with the metadata
// fields that travel with it.
//
// The engine used to scan content.raw alone, but metadata is delivered
// verbatim into the agent workspace on PASS (routing.RoutePass copies
// metadata.json into the inbox) and into the quarantine notification the
// review agent reads. An injection written entirely into a Subject line
// therefore scored 0.00, passed, and arrived at the agent intact -- the
// whole engine bypassed by putting the payload in a field nobody scanned.
//
// Metadata is matched, not detected: the matchers carry the high-weight
// instruction rules, while the custom detectors (language, template
// structure) are tuned for prose and would be noisy on a short subject
// line -- a spurious non-English boost on a two-word subject is exactly
// the sort of false positive that gets a scanner switched off.
func (s *Scanner) ScanWithMetadata(content []byte, contentType string, metadata []string) (engine.ScanResult, error) {
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
	// Additional scan-only views of the same item. Each is nil unless it
	// would differ from Normalized, so ordinary ASCII content still costs
	// exactly one pass. Signals are deduped by rule name, so a payload
	// visible in more than one view is counted once.
	for _, extra := range []struct {
		name    string
		content []byte
		// Detectors are re-run only where they can see something the
		// normalized pass cannot; matchers run against every view.
		detectors []engine.ScanFunc
	}{
		// Homoglyph skeleton: catches "ignоre previоus" (Cyrillic о).
		{name: "folded", content: pp.Folded},
		// Decoded base64/hex/percent payloads and recovered Tags-block text.
		{name: "decoded", content: pp.Decoded},
		// Escapes decoded where they sit: catches "Ignore%20all%20previous",
		// where the words are plain text and only the separators are encoded.
		{name: "unescaped", content: pp.Unescaped},
		// The same decoding over the unstripped HTML, where the
		// attributes still are: catches a payload written into an
		// href's query, which the tag strip removes before the
		// decoding above can see it.
		{name: "unescaped-html", content: pp.UnescapedHTML},
		// Bidi-reordered view: catches a payload written backwards inside an
		// RLO override, which renders forwards and stores backwards.
		{name: "reordered", content: pp.Reordered},
		// Pre-scrub view: the only one where invisibles are still present,
		// so the smuggling and encoding detectors can report them.
		{name: "pre-scrub", content: pp.PreScrub, detectors: s.detectors},
	} {
		if extra.content == nil {
			continue
		}
		extraSignals, err := engine.ScanContent(bytes.NewReader(extra.content), s.matchers, extra.detectors)
		if err != nil {
			return engine.ScanResult{}, fmt.Errorf("scan %s: %w", extra.name, err)
		}
		signals = appendDeduped(signals, extraSignals)
	}
	if metaSignals, err := s.scanMetadata(metadata); err != nil {
		return engine.ScanResult{}, err
	} else {
		signals = appendDeduped(signals, metaSignals)
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
	result := engine.ScoreSignals(scored, boosts, s.threshold)
	// Preserve ALL fired signals (including weight_booster signals excluded from
	// the scored sum) on the result, matching main.go deliverResult's audit
	// behavior. Score and verdict are computed from `scored`+`boosts`; only the
	// Signals field reflects the full set.
	result.Signals = signals
	return result, nil
}

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
			// Sampling is a per-detector opt-in. A detector that has not
			// declared itself sample-safe sees the whole document, so a
			// payload cannot be hidden by padding it past a window.
			sampled := false
			if sd, ok := d.(detector.SampledDetector); ok {
				sampled = sd.AllowSampling()
			}
			detectors = append(detectors, func(content []byte) ([]engine.Signal, error) {
				if sampled {
					content = engine.SampleContent(content, engine.DefaultSampleSize)
				}
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

// scanMetadata runs the matchers over the item's metadata fields. The
// fields are joined and pushed through the same Preprocess pipeline as
// content, so a Subject carrying homoglyphs, invisible characters or an
// encoded payload is caught by the same hardening.
func (s *Scanner) scanMetadata(fields []string) ([]engine.Signal, error) {
	var present []string
	for _, f := range fields {
		if strings.TrimSpace(f) != "" {
			present = append(present, f)
		}
	}
	if len(present) == 0 {
		return nil, nil
	}

	joined := []byte(strings.Join(present, "\n"))
	pp := engine.Preprocess(joined, "text/plain")

	var signals []engine.Signal
	for _, view := range [][]byte{pp.Normalized, pp.Folded, pp.Decoded, pp.Unescaped, pp.Reordered} {
		if view == nil {
			continue
		}
		viewSignals, err := engine.ScanContent(bytes.NewReader(view), s.matchers, nil)
		if err != nil {
			return nil, fmt.Errorf("scan metadata: %w", err)
		}
		signals = appendDeduped(signals, viewSignals)
	}
	return signals, nil
}
