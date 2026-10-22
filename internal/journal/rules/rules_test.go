package rules

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

func reg(t *testing.T) *record.Registry {
	t.Helper()
	r, err := record.NewRegistry([]record.Type{{
		Name: "attendance_day", SchemaVersion: 1, Owner: "powerschool",
		Fields: map[string]record.FieldDef{"date": {Type: record.Date}, "classification": {Type: record.String},
			"any_unexcused": {Type: record.Bool}},
		DefaultAudience: []string{"subject", "guardians"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func j(s string) json.RawMessage { return json.RawMessage(s) }

// The two rules of spec 4.10, as the chart would render them to JSON.
const specRules = `[
 {"name": "full_day_absence",
  "match": {"record_type": "attendance_day", "kind": ["created", "updated"],
            "field_becomes": {"classification": "full_day"}, "field_in": {"any_unexcused": [true]}},
  "importance": "high",
  "notify": [{"token": "guardians", "mode": "push"}, {"token": "subject", "mode": "prompt", "purpose": "attendance_followup"}],
  "group_by": ["data_subject", "field:date"]},
 {"name": "partial_day_absence",
  "match": {"record_type": "attendance_day", "field_becomes": {"classification": "partial_day"}, "field_in": {"any_unexcused": [true]}},
  "importance": "normal",
  "notify": [{"token": "guardians", "mode": "digest"}, {"token": "subject", "mode": "prompt", "purpose": "attendance_followup"}],
  "group_by": ["data_subject", "field:date"]}
]`

func compileSpec(t *testing.T) *Set {
	t.Helper()
	var rs []Rule
	if err := json.Unmarshal([]byte(specRules), &rs); err != nil {
		t.Fatal(err)
	}
	s, err := Compile(rs, reg(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func day(class string, unexcused bool) record.Fields {
	return record.Fields{"date": j(`"2026-10-14"`), "classification": record.Encode(class), "any_unexcused": record.Encode(unexcused)}
}

func TestEvaluate(t *testing.T) {
	s := compileSpec(t)
	base := EventView{RecordType: "attendance_day", DataSubject: "e_child1", Key: "2026-10-14", Scope: "x"}
	for name, c := range map[string]struct {
		kind          string
		before, after record.Fields
		rule, imp     string
	}{
		"new full day":            {"created", nil, day("full_day", true), "full_day_absence", High},
		"partial becomes full":    {"updated", day("partial_day", true), day("full_day", true), "full_day_absence", High},
		"full stays full":         {"updated", day("full_day", true), day("full_day", true), "", Low},
		"full but excused":        {"created", nil, day("full_day", false), "", Low},
		"restored, kind unlisted": {"restored", nil, day("full_day", true), "", Low},
		"restored partial day":    {"restored", nil, day("partial_day", true), "partial_day_absence", Normal},
		"new partial day":         {"created", nil, day("partial_day", true), "partial_day_absence", Normal},
		"removed day":             {"removed", day("full_day", true), nil, "", Low},
		"late only":               {"created", nil, day("late_or_left_early", true), "", Low},
	} {
		v := base
		v.Kind, v.Before, v.After = c.kind, c.before, c.after
		o := s.Evaluate(v)
		if o.Rule != c.rule || o.Importance != c.imp {
			t.Errorf("%s: %+v", name, o)
		}
	}
	v := base
	v.Kind, v.After = "created", day("full_day", true)
	o := s.Evaluate(v)
	if o.GroupKey != "data_subject=e_child1/field:date=2026-10-14" || len(o.Notify) != 2 || o.Notify[1].Purpose != "attendance_followup" {
		t.Errorf("outcome %+v", o)
	}
	var none *Set
	if o := none.Evaluate(v); o.Importance != Low || o.Rule != "" {
		t.Errorf("no rules: %+v", o)
	}
}

func TestFieldInMatchesTheLastValueOfARemovedRecord(t *testing.T) {
	s, err := Compile([]Rule{{Name: "day_cleared", Importance: Normal,
		Match: Match{RecordType: "attendance_day", Kind: []string{"removed"}, FieldIn: map[string][]json.RawMessage{"classification": {j(`"full_day"`)}}}}}, reg(t))
	if err != nil {
		t.Fatal(err)
	}
	o := s.Evaluate(EventView{RecordType: "attendance_day", Kind: "removed", Before: day("full_day", true)})
	if o.Rule != "day_cleared" {
		t.Errorf("%+v", o)
	}
}

func TestCompileRefusals(t *testing.T) {
	ok := Rule{Name: "r", Importance: High, Match: Match{RecordType: "attendance_day"}}
	for name, mod := range map[string]func(*Rule){
		"bad name":          func(r *Rule) { r.Name = "R!" },
		"bad importance":    func(r *Rule) { r.Importance = "urgent" },
		"unknown type":      func(r *Rule) { r.Match.RecordType = "nosuch" },
		"bad kind":          func(r *Rule) { r.Match.Kind = []string{"deleted"} },
		"unknown field":     func(r *Rule) { r.Match.FieldIn = map[string][]json.RawMessage{"nosuch": {j(`1`)}} },
		"wrongly typed":     func(r *Rule) { r.Match.FieldBecomes = map[string]json.RawMessage{"any_unexcused": j(`"yes"`)} },
		"null becomes":      func(r *Rule) { r.Match.FieldBecomes = map[string]json.RawMessage{"classification": j(`null`)} },
		"empty field_in":    func(r *Rule) { r.Match.FieldIn = map[string][]json.RawMessage{"classification": {}} },
		"bad mode":          func(r *Rule) { r.Notify = []Notice{{Token: "guardians", Mode: "email"}} },
		"prompt no purpose": func(r *Rule) { r.Notify = []Notice{{Token: "subject", Mode: "prompt"}} },
		"bad token":         func(r *Rule) { r.Notify = []Notice{{Token: "Guardians", Mode: "push"}} },
		"bad group_by":      func(r *Rule) { r.GroupBy = []string{"field:nosuch"} },
		"bad group_by word": func(r *Rule) { r.GroupBy = []string{"date"} },
	} {
		r := ok
		mod(&r)
		if _, err := Compile([]Rule{r}, reg(t)); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if _, err := Compile([]Rule{ok, ok}, reg(t)); err == nil {
		t.Error("two rules with one name compiled")
	}
}

func TestImportanceRank(t *testing.T) {
	if !(ImportanceRank(High) > ImportanceRank(Normal) && ImportanceRank(Normal) > ImportanceRank(Low) && ImportanceRank("x") == 0) {
		t.Error("ranks")
	}
	if got := AtLeast(Normal); len(got) != 2 || got[0] != High || got[1] != Normal {
		t.Errorf("AtLeast(normal) = %v", got)
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	m := Match{RecordType: "attendance_day", FieldIn: map[string][]json.RawMessage{"any_unexcused": {j(`true`)}}}
	a := Rule{Name: "first", Importance: Normal, Match: m}
	b := Rule{Name: "second", Importance: High, Match: m}
	v := EventView{RecordType: "attendance_day", Kind: "created", After: day("full_day", true)}
	for name, rs := range map[string][]Rule{"a first": {a, b}, "b first": {b, a}} {
		s, err := Compile(rs, reg(t))
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Evaluate(v).Rule; got != rs[0].Name {
			t.Errorf("%s: matched %s", name, got)
		}
	}
}

func TestGroupKeyCannotCollideAndIsBounded(t *testing.T) {
	s, err := Compile([]Rule{{Name: "g", Importance: Normal,
		Match:   Match{RecordType: "attendance_day"},
		GroupBy: []string{"data_subject", "key"}}}, reg(t))
	if err != nil {
		t.Fatal(err)
	}
	a := s.Evaluate(EventView{RecordType: "attendance_day", DataSubject: "x/key=y", Key: "z"}).GroupKey
	b := s.Evaluate(EventView{RecordType: "attendance_day", DataSubject: "x", Key: "y/key=z"}).GroupKey
	if a == b || a == "" {
		t.Errorf("collision: %q %q", a, b)
	}
	long := strings.Repeat("a", 10000)
	l1 := s.Evaluate(EventView{RecordType: "attendance_day", DataSubject: "x", Key: long + "1"}).GroupKey
	l2 := s.Evaluate(EventView{RecordType: "attendance_day", DataSubject: "x", Key: long + "2"}).GroupKey
	if len(l1) > MaxGroupKeyBytes || len(l2) > MaxGroupKeyBytes || l1 == l2 {
		t.Errorf("bounded keys: %d %d equal=%v", len(l1), len(l2), l1 == l2)
	}
}

func TestEvaluateDoesNotAliasNotify(t *testing.T) {
	s := compileSpec(t)
	v := EventView{RecordType: "attendance_day", Kind: "created", After: day("full_day", true)}
	o := s.Evaluate(v)
	o.Notify[0].Token = "changed"
	if s.Evaluate(v).Notify[0].Token != "guardians" {
		t.Error("outcome shares the rule's notify slice")
	}
}
