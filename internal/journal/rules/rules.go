// Package rules evaluates the journal's importance, grouping and
// notification rules when an event is written (spec 2026-10-16 4.10).
// A rule's notify list is advice to readers, not delivery.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// Importance levels.
const (
	High   = journalapi.ImportanceHigh
	Normal = journalapi.ImportanceNormal
	Low    = journalapi.ImportanceLow
)

// ImportanceRank orders importance levels; 0 for anything else.
func ImportanceRank(s string) int {
	switch s {
	case High:
		return 3
	case Normal:
		return 2
	case Low:
		return 1
	}
	return 0
}

// AtLeast lists the importance levels at or above min, highest first.
func AtLeast(min string) []string {
	var out []string
	for _, l := range []string{High, Normal, Low} {
		if ImportanceRank(l) >= ImportanceRank(min) {
			out = append(out, l)
		}
	}
	return out
}

// Notice is one notify entry; it is the wire type, so the stored advice and
// what readers receive cannot drift apart.
type Notice = journalapi.Notice

// MaxGroupKeyBytes bounds a group_key. A longer key (a long text or list
// field in group_by) is replaced by "sha256=" and the hex digest of the full
// key; no normal key starts with "sha256=" because entry names are fixed.
const MaxGroupKeyBytes = 512

// Match selects events.
type Match struct {
	RecordType   string                       `json:"record_type"`
	Kind         []string                     `json:"kind,omitempty"`
	FieldBecomes map[string]json.RawMessage   `json:"field_becomes,omitempty"`
	FieldIn      map[string][]json.RawMessage `json:"field_in,omitempty"`
}

// UnmarshalJSON refuses unknown keys: a misspelled condition such as
// "field_become" would otherwise vanish and leave a catch-all rule.
func (m *Match) UnmarshalJSON(b []byte) error {
	type bare Match
	return strictDecode(b, (*bare)(m))
}

// strictDecode also refuses duplicate and case-variant keys, which
// encoding/json would resolve silently (last wins).
func strictDecode(b []byte, v any) error { return strictjson.Decode(b, v) }

// Rule is one configured rule.
type Rule struct {
	Name       string   `json:"name"`
	Match      Match    `json:"match"`
	Importance string   `json:"importance"`
	Notify     []Notice `json:"notify,omitempty"`
	GroupBy    []string `json:"group_by,omitempty"`
}

// UnmarshalJSON refuses unknown keys in the rule and, because the decoder
// is strict throughout, in its notify entries.
func (r *Rule) UnmarshalJSON(b []byte) error {
	type bare Rule
	return strictDecode(b, (*bare)(r))
}

type compiled struct {
	Rule
	kinds   map[string]bool
	becomes map[string]json.RawMessage
	in      map[string][]json.RawMessage
}

// Set is a compiled, ordered rule list. A nil *Set matches nothing.
type Set struct {
	rules []compiled
}

// Compile validates rules against the active record types.
func Compile(rs []Rule, reg *record.Registry) (*Set, error) {
	if reg == nil {
		return nil, fmt.Errorf("rules: no record type registry")
	}
	s := &Set{}
	names := map[string]bool{}
	for _, r := range rs {
		if !record.ValidName(r.Name) || names[r.Name] {
			return nil, fmt.Errorf("rule %q: names are [a-z][a-z0-9_]* and distinct", r.Name)
		}
		names[r.Name] = true
		if ImportanceRank(r.Importance) == 0 {
			return nil, fmt.Errorf("rule %s: importance is high, normal or low", r.Name)
		}
		t, ok := reg.Active(r.Match.RecordType)
		if !ok {
			return nil, fmt.Errorf("rule %s: unknown record type %q", r.Name, r.Match.RecordType)
		}
		r.Notify = append([]Notice(nil), r.Notify...)
		c := compiled{Rule: r, kinds: map[string]bool{}, becomes: map[string]json.RawMessage{}, in: map[string][]json.RawMessage{}}
		for _, k := range r.Match.Kind {
			switch k {
			case journalapi.KindCreated, journalapi.KindUpdated, journalapi.KindRemoved, journalapi.KindRestored:
				c.kinds[k] = true
			default:
				return nil, fmt.Errorf("rule %s: unknown kind %q", r.Name, k)
			}
		}
		norm := func(field string, v json.RawMessage) (json.RawMessage, error) {
			def, ok := t.Fields[field]
			if !ok {
				return nil, fmt.Errorf("rule %s: %s has no field %q", r.Name, t.Name, field)
			}
			nv, rej := record.NormalizeValue(def.Type, v)
			if rej != nil || nv == nil {
				return nil, fmt.Errorf("rule %s: value for %s is not a non-null %s", r.Name, field, def.Type)
			}
			return nv, nil
		}
		for field, v := range r.Match.FieldBecomes {
			nv, err := norm(field, v)
			if err != nil {
				return nil, err
			}
			c.becomes[field] = nv
		}
		for field, vs := range r.Match.FieldIn {
			if len(vs) == 0 {
				return nil, fmt.Errorf("rule %s: field_in %s lists no values", r.Name, field)
			}
			for _, v := range vs {
				nv, err := norm(field, v)
				if err != nil {
					return nil, err
				}
				c.in[field] = append(c.in[field], nv)
			}
		}
		for _, n := range r.Notify {
			// Syntax only: Compile has no roster, so a token no reader holds
			// is not caught here.
			if !record.ValidToken(n.Token) {
				return nil, fmt.Errorf("rule %s: invalid notify token %q", r.Name, n.Token)
			}
			switch n.Mode {
			case journalapi.ModePush, journalapi.ModeDigest:
			case journalapi.ModePrompt:
				if n.Purpose == "" {
					return nil, fmt.Errorf("rule %s: a prompt names its purpose", r.Name)
				}
			default:
				return nil, fmt.Errorf("rule %s: notify mode is push, prompt or digest", r.Name)
			}
			if n.Purpose != "" && !record.ValidName(n.Purpose) {
				return nil, fmt.Errorf("rule %s: invalid purpose %q", r.Name, n.Purpose)
			}
		}
		for _, g := range r.GroupBy {
			switch {
			case g == "data_subject", g == "record_type", g == "key", g == "scope":
			case strings.HasPrefix(g, "field:"):
				if _, ok := t.Fields[strings.TrimPrefix(g, "field:")]; !ok {
					return nil, fmt.Errorf("rule %s: group_by %s names no field of %s", r.Name, g, t.Name)
				}
			default:
				return nil, fmt.Errorf("rule %s: group_by entries are data_subject, record_type, key, scope or field:<name>", r.Name)
			}
		}
		s.rules = append(s.rules, c)
	}
	return s, nil
}

// EventView is what a rule sees of an event. Before is nil for created and
// restored; After is nil for removed.
type EventView struct {
	RecordType, DataSubject, Key, Scope, Kind string
	Before, After                             record.Fields
}

// Outcome is what the first matching rule assigns.
type Outcome struct {
	Importance string
	Rule       string
	GroupKey   string
	Notify     []Notice
}

// Evaluate applies the first matching rule; no match is low importance.
func (s *Set) Evaluate(e EventView) Outcome {
	if s != nil {
		for _, c := range s.rules {
			if c.matches(e) {
				return Outcome{Importance: c.Importance, Rule: c.Name, GroupKey: c.groupKey(e), Notify: append([]Notice(nil), c.Notify...)}
			}
		}
	}
	return Outcome{Importance: Low}
}

func (c compiled) matches(e EventView) bool {
	if c.Match.RecordType != e.RecordType {
		return false
	}
	if len(c.kinds) > 0 && !c.kinds[e.Kind] {
		return false
	}
	values := e.After
	if values == nil {
		values = e.Before
	}
	if len(c.becomes) > 0 {
		// The conjunction of all field_becomes conditions must hold after
		// the event and must not have held before it (nil Before: it did
		// not). Removed events never become anything; the kind test is
		// defence in depth, since After is nil there and nothing holds.
		if e.Kind == journalapi.KindRemoved || !holdsAll(c.becomes, e.After) || holdsAll(c.becomes, e.Before) {
			return false
		}
	}
	for field, vs := range c.in {
		found := false
		for _, v := range vs {
			if bytes.Equal(values[field], v) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func holdsAll(want map[string]json.RawMessage, have record.Fields) bool {
	for field, v := range want {
		if !bytes.Equal(have[field], v) {
			return false
		}
	}
	return true
}

func (c compiled) groupKey(e EventView) string {
	if len(c.GroupBy) == 0 {
		return ""
	}
	values := e.After
	if values == nil {
		values = e.Before
	}
	parts := make([]string, 0, len(c.GroupBy))
	for _, g := range c.GroupBy {
		var v string
		switch g {
		case "data_subject":
			v = e.DataSubject
		case "record_type":
			v = e.RecordType
		case "key":
			v = e.Key
		case "scope":
			v = e.Scope
		default:
			raw, present := values[strings.TrimPrefix(g, "field:")]
			if !present {
				// "%00" cannot be produced by EscapeScopeValue (a literal
				// % becomes %25), so an absent field never collides with
				// an empty or any other value.
				parts = append(parts, record.EscapeScopeValue(g)+"=%00")
				continue
			}
			v = plain(raw)
		}
		parts = append(parts, record.EscapeScopeValue(g)+"="+record.EscapeScopeValue(v))
	}
	key := strings.Join(parts, "/")
	if len(key) > MaxGroupKeyBytes {
		sum := sha256.Sum256([]byte(key))
		return "sha256=" + hex.EncodeToString(sum[:])
	}
	return key
}

// plain renders a value for a group key: strings without quotes.
func plain(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}
