package journaltest

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// HouseholdV1 is the roster fixture: two guardians of two siblings, a
// guest outside the household. Placeholder ids only.
const HouseholdV1 = `{
  "people": {
    "e_parent1": {"agent": "agent-parent1", "guardian_of": ["e_child1", "e_child2"]},
    "e_parent2": {"agent": "agent-parent2", "guardian_of": ["e_child1", "e_child2"]},
    "e_child1":  {"agent": "agent-child1", "siblings": ["e_child2"]},
    "e_child2":  {"agent": "agent-child2", "siblings": ["e_child1"]},
    "e_guest1":  {"agent": "agent-guest1"}
  },
  "household_members": ["e_parent1", "e_parent2", "e_child1", "e_child2"]
}
`

// Types are the record types the journal and HTTP tests use.
func Types() []record.Type {
	src := []string{"subject", "guardians"}
	return []record.Type{
		{Name: "assignment", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: src,
			Fields: map[string]record.FieldDef{"title": {Type: record.String}, "due": {Type: record.Date},
				"score": {Type: record.Number}, "missing": {Type: record.Bool}, "comment": {Type: record.Text}}},
		{Name: "attendance_day", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: src,
			Fields: map[string]record.FieldDef{"date": {Type: record.Date}, "classification": {Type: record.String},
				"any_unexcused": {Type: record.Bool}}},
		{Name: "term_comment", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: []string{"guardians"},
			Fields: map[string]record.FieldDef{"comment": {Type: record.Text}}},
		{Name: "lms_assignment", SchemaVersion: 1, Owner: "schoology", DefaultAudience: src,
			Fields: map[string]record.FieldDef{"title": {Type: record.String}, "status": {Type: record.String}}},
		{Name: "attendance_note", SchemaVersion: 1, Owner: record.OwnerHousehold, FixedAudience: src,
			Fields: map[string]record.FieldDef{"date": {Type: record.Date}, "what_happened": {Type: record.Text},
				"heard_from": {Type: record.String}, "explained_to_school": {Type: record.String}}},
	}
}

// Registry builds Types (plus extra versions) into a registry.
func Registry(t testing.TB, extra ...record.Type) *record.Registry {
	t.Helper()
	r, err := record.NewRegistry(append(Types(), extra...))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Rules are the two rules of spec 4.10, in the canonical conjunction form:
// field_becomes {classification, any_unexcused: true}.
func Rules() []rules.Rule {
	day := func(name, class, importance string, kinds []string, notify []rules.Notice) rules.Rule {
		return rules.Rule{Name: name, Importance: importance, Notify: notify, GroupBy: []string{"data_subject", "field:date"},
			Match: rules.Match{RecordType: "attendance_day", Kind: kinds,
				FieldBecomes: map[string]json.RawMessage{
					"classification": record.Encode(class),
					"any_unexcused":  record.Encode(true),
				}}}
	}
	prompt := rules.Notice{Token: "subject", Mode: "prompt", Purpose: "attendance_followup"}
	return []rules.Rule{
		day("full_day_absence", "full_day", rules.High, []string{"created", "updated"},
			[]rules.Notice{{Token: "guardians", Mode: "push"}, prompt}),
		day("partial_day_absence", "partial_day", rules.Normal, nil,
			[]rules.Notice{{Token: "guardians", Mode: "digest"}, prompt}),
	}
}

// Roster parses HouseholdV1.
func Roster(t testing.TB) *roster.Roster {
	t.Helper()
	r, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(HouseholdV1))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// FakeScanner withholds every text containing "INJECT".
type FakeScanner struct {
	mu      sync.Mutex
	version string
	calls   int
}

// NewFakeScanner starts at version.
func NewFakeScanner(version string) *FakeScanner { return &FakeScanner{version: version} }

// Verdict implements journal.TextScanner.
func (s *FakeScanner) Verdict(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if strings.Contains(text, "INJECT") {
		return "quarantine"
	}
	return record.VerdictPass
}

// Version implements journal.TextScanner.
func (s *FakeScanner) Version() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// SetVersion simulates a scanner upgrade.
func (s *FakeScanner) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
}

// Calls counts Verdict calls.
func (s *FakeScanner) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
