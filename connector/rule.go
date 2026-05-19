package connector

// Rule defines a routing rule with an optional set of metadata tags.
// Rules are evaluated in order; the first match wins.
type Rule struct {
	Match       string            `json:"match"`
	Destination string            `json:"destination"`
	Tags        map[string]string `json:"tags,omitempty"`
	DataSubject string            `json:"data_subject,omitempty"`
	Audience    []string          `json:"audience,omitempty"`
}

// MatchResult holds the destination, tags, data_subject, and audience
// produced by a successful match.
type MatchResult struct {
	Destination string
	Tags        map[string]string
	DataSubject string
	Audience    []string
}

// RuleMatcher evaluates a list of rules against a key string.
// It uses first-match-wins semantics: the first rule whose Match field
// equals the key (or is the wildcard "*") is returned.
type RuleMatcher struct {
	rules []Rule
}

// NewRuleMatcher creates a RuleMatcher from an ordered list of rules.
func NewRuleMatcher(rules []Rule) *RuleMatcher {
	return &RuleMatcher{rules: rules}
}

// Match returns the MatchResult for the first rule that matches key.
// A rule matches if its Match field equals key exactly, or if it is "*".
// Returns false if no rule matches.
func (rm *RuleMatcher) Match(key string) (MatchResult, bool) {
	for _, rule := range rm.rules {
		if rule.Match == key || rule.Match == "*" {
			var tags map[string]string
			if len(rule.Tags) > 0 {
				tags = make(map[string]string, len(rule.Tags))
				for k, v := range rule.Tags {
					tags[k] = v
				}
			}
			var audience []string
			if len(rule.Audience) > 0 {
				audience = append([]string(nil), rule.Audience...)
			}
			return MatchResult{
				Destination: rule.Destination,
				Tags:        tags,
				DataSubject: rule.DataSubject,
				Audience:    audience,
			}, true
		}
	}
	return MatchResult{}, false
}
