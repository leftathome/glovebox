package main

// Rule-based pre-filter for the mbox importer.
//
// The filter is a list of rules evaluated first-match-wins. Each rule has an
// `action` of "include" or "exclude". Messages that match no rule are
// excluded by default; users who want opt-out semantics append a trailing
// wildcard `{"action": "include"}` rule as a terminator.
//
// Design reference: docs/specs/09-mbox-importer-design.md §3.4 (all
// subsections: schema, match fields table, action semantics, evaluation).
//
// Evaluate is a pure function: no I/O, no globals. LoadFilter is the only
// function in this file that touches the filesystem.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// Action values for a FilterRule.
const (
	ActionInclude = "include"
	ActionExclude = "exclude"
)

// defaultRuleKey is the ruleKey returned by Evaluate when no rule matches
// and the implicit default-exclude action is applied.
const defaultRuleKey = "default_excluded"

// FilterConfig is the on-disk shape of `<source>.filter.json`.
type FilterConfig struct {
	SchemaVersion string       `json:"schema_version"`
	FilterRules   []FilterRule `json:"filter_rules"`
}

// FilterRule pairs a match specification with an action.
type FilterRule struct {
	Match  MatchSpec `json:"match"`
	Action string    `json:"action"`
}

// MatchSpec is the set of typed fields a rule can match against. Unset
// fields are "don't care." A match requires ALL set fields to satisfy
// (logical AND within a MatchSpec).
type MatchSpec struct {
	Label           string     `json:"label,omitempty"`
	ListID          string     `json:"list_id,omitempty"`
	Sender          string     `json:"sender,omitempty"`
	SenderDomain    string     `json:"sender_domain,omitempty"`
	SubjectContains string     `json:"subject_contains,omitempty"`
	DateAfter       *time.Time `json:"date_after,omitempty"`
	DateBefore      *time.Time `json:"date_before,omitempty"`
	MinSizeBytes    *int64     `json:"min_size_bytes,omitempty"`
	MaxSizeBytes    *int64     `json:"max_size_bytes,omitempty"`
}

// isEmpty reports whether the MatchSpec has no fields set. An empty
// MatchSpec is a wildcard: it matches every message.
func (m MatchSpec) isEmpty() bool {
	return m.Label == "" &&
		m.ListID == "" &&
		m.Sender == "" &&
		m.SenderDomain == "" &&
		m.SubjectContains == "" &&
		m.DateAfter == nil &&
		m.DateBefore == nil &&
		m.MinSizeBytes == nil &&
		m.MaxSizeBytes == nil
}

// LoadFilter reads and validates a filter config from path. If the file does
// not exist the returned error satisfies errors.Is(err, fs.ErrNotExist) so
// callers can distinguish "no filter defined" from "filter is broken."
//
// Validation:
//   - Every rule's Action must be ActionInclude or ActionExclude.
//   - Every rule's MatchSpec must set at least one field, OR the rule must
//     be an explicit wildcard include (empty match + action=include). The
//     wildcard include is permitted per spec §3.4.3 as an opt-out terminator.
//     A wildcard exclude is rejected because it would silently swallow every
//     following rule; users who want "exclude everything" should omit the
//     filter entirely (the default action is already exclude).
func LoadFilter(path string) (*FilterConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// os.ReadFile already wraps the underlying error so
		// errors.Is(err, fs.ErrNotExist) works for missing files.
		return nil, err
	}

	var cfg FilterConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse filter %q: %w", path, err)
	}

	for i, r := range cfg.FilterRules {
		if r.Action != ActionInclude && r.Action != ActionExclude {
			return nil, fmt.Errorf("filter rule %d: unknown action %q (must be %q or %q)",
				i, r.Action, ActionInclude, ActionExclude)
		}
		if r.Match.isEmpty() && r.Action != ActionInclude {
			// Empty match + exclude would silently consume every
			// subsequent rule. Reject it as a likely mistake.
			return nil, fmt.Errorf("filter rule %d: empty match with action %q is not allowed (empty match is only valid as a wildcard %q terminator)",
				i, r.Action, ActionInclude)
		}
	}

	return &cfg, nil
}

// Evaluate walks the filter rules first-match-wins against m. It returns the
// matching rule's action, its 0-based index, and a human-readable ruleKey
// suitable for use as a stable key in the manifest's filter_hit_counts map.
//
// If no rule matches, Evaluate returns (ActionExclude, -1, "default_excluded")
// per spec §3.4.3.
func (c *FilterConfig) Evaluate(m *Message) (action string, ruleIdx int, ruleKey string) {
	if c == nil {
		return ActionExclude, -1, defaultRuleKey
	}
	for i, r := range c.FilterRules {
		if matches(r.Match, m) {
			return r.Action, i, ruleKeyFor(i, r.Match)
		}
	}
	return ActionExclude, -1, defaultRuleKey
}

// matches reports whether m satisfies every set field in spec.
//
// An empty MatchSpec matches every message (wildcard).
func matches(spec MatchSpec, m *Message) bool {
	if spec.Label != "" {
		if !containsLabel(m.GmailLabels, spec.Label) {
			return false
		}
	}
	if spec.ListID != "" {
		if !globMatch(spec.ListID, m.ListID) {
			return false
		}
	}
	if spec.Sender != "" {
		if spec.Sender != m.From {
			return false
		}
	}
	if spec.SenderDomain != "" {
		if !globMatch(spec.SenderDomain, senderDomain(m.From)) {
			return false
		}
	}
	if spec.SubjectContains != "" {
		if !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(spec.SubjectContains)) {
			return false
		}
	}
	if spec.DateAfter != nil {
		// Fail-safe: if Date is zero, the date constraint does not
		// match. A message with an unparseable or missing Date cannot
		// satisfy a date-bound rule.
		if m.Date.IsZero() || !m.Date.After(*spec.DateAfter) {
			return false
		}
	}
	if spec.DateBefore != nil {
		if m.Date.IsZero() || !m.Date.Before(*spec.DateBefore) {
			return false
		}
	}
	if spec.MinSizeBytes != nil {
		if int64(m.Size) < *spec.MinSizeBytes {
			return false
		}
	}
	if spec.MaxSizeBytes != nil {
		if int64(m.Size) > *spec.MaxSizeBytes {
			return false
		}
	}
	return true
}

// containsLabel reports whether s is an exact match for any entry in labels.
func containsLabel(labels []string, s string) bool {
	for _, l := range labels {
		if l == s {
			return true
		}
	}
	return false
}

// globMatch returns true iff pattern matches s. The pattern supports the
// two standard glob metacharacters `*` and `?` (via path.Match, whose
// semantics are: `*` matches any sequence of non-separator characters, `?`
// matches any single non-separator character). There are no path separators
// in list-ids, domains, etc., so path.Match behaves as expected here.
//
// If pattern has no glob metacharacters, globMatch degenerates to exact
// equality, which is the right behavior for a literal pattern.
func globMatch(pattern, s string) bool {
	// path.Match returns an error only if the pattern is syntactically
	// malformed; for our purposes a malformed pattern is treated as
	// non-matching (the caller can still author patterns containing
	// unescaped brackets, which we want to treat as no-match rather than
	// panic).
	ok, err := path.Match(pattern, s)
	if err != nil {
		return false
	}
	return ok
}

// senderDomain extracts the domain portion of an addr-spec. For an input
// like "alice@example.com" returns "example.com"; for a malformed input
// without an "@" returns an empty string.
func senderDomain(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return addr[at+1:]
}

// ruleKeyFor derives a concise, deterministic, human-readable key for a
// rule. The key is used as a map key in the manifest's `filter_hit_counts`
// so it must be stable across runs.
//
// Format: `rule_<idx>_<field>_<value>` where `<field>` is the first
// distinctive field present in the MatchSpec and `<value>` is its literal
// (dates formatted as 2006-01-02; numeric sizes formatted as decimal).
// An empty MatchSpec renders as `rule_<idx>_wildcard`.
//
// The field precedence is fixed so that re-running against the same filter
// config always produces the same keys.
func ruleKeyFor(idx int, spec MatchSpec) string {
	// Field precedence matches the order documented in spec §3.4.2.
	switch {
	case spec.Label != "":
		return fmt.Sprintf("rule_%d_label_%s", idx, spec.Label)
	case spec.ListID != "":
		return fmt.Sprintf("rule_%d_list_id_%s", idx, spec.ListID)
	case spec.Sender != "":
		return fmt.Sprintf("rule_%d_sender_%s", idx, spec.Sender)
	case spec.SenderDomain != "":
		return fmt.Sprintf("rule_%d_sender_domain_%s", idx, spec.SenderDomain)
	case spec.SubjectContains != "":
		return fmt.Sprintf("rule_%d_subject_contains_%s", idx, spec.SubjectContains)
	case spec.DateAfter != nil:
		return fmt.Sprintf("rule_%d_date_after_%s", idx, spec.DateAfter.UTC().Format("2006-01-02"))
	case spec.DateBefore != nil:
		return fmt.Sprintf("rule_%d_date_before_%s", idx, spec.DateBefore.UTC().Format("2006-01-02"))
	case spec.MinSizeBytes != nil:
		return fmt.Sprintf("rule_%d_min_size_bytes_%d", idx, *spec.MinSizeBytes)
	case spec.MaxSizeBytes != nil:
		return fmt.Sprintf("rule_%d_max_size_bytes_%d", idx, *spec.MaxSizeBytes)
	default:
		return fmt.Sprintf("rule_%d_wildcard", idx)
	}
}
