package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func reg(t *testing.T) *record.Registry {
	t.Helper()
	r, err := record.NewRegistry([]record.Type{
		{Name: "assignment", SchemaVersion: 1, Owner: "powerschool", Fields: map[string]record.FieldDef{"title": {Type: record.String}}, DefaultAudience: []string{"subject"}},
		{Name: "lms_assignment", SchemaVersion: 1, Owner: "schoology", Fields: map[string]record.FieldDef{"title": {Type: record.String}}, DefaultAudience: []string{"subject"}},
		{Name: "attendance_note", SchemaVersion: 1, Owner: record.OwnerHousehold, Fields: map[string]record.FieldDef{"date": {Type: record.Date}}, FixedAudience: []string{"subject", "guardians"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The spec's example policy, as the chart renders it to JSON.
const specPolicy = `{
  "openclaw/openclaw-gateway": {"read": true, "readers": "*", "write_types": ["attendance_note"], "counts": true},
  "openclaw/glovebox-glovebox-connector-powerschool": {"write_types": ["assignment"], "write_scopes": {"source": ["powerschool"]}, "write_subjects": "roster_subjects"},
  "openclaw/glovebox-glovebox-connector-schoology": {"write_types": ["lms_assignment"], "write_scopes": {"source": ["schoology"]}, "write_subjects": "roster_subjects"},
  "trunchbull/trunchbull": {"read": true, "readers": ["e_parent1", "e_parent2"], "counts": true},
  "openclaw/glovebox-journal-admin": {"admin": true},
  "openclaw/shared-writer": {"write_types": ["assignment", "lms_assignment"], "write_subjects": "roster_subjects"}
}`

func load(t *testing.T) *Policy {
	t.Helper()
	var s map[string]Service
	if err := json.Unmarshal([]byte(specPolicy), &s); err != nil {
		t.Fatal(err)
	}
	p, err := New(s, reg(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func scope(t *testing.T, kv ...string) record.Scope {
	var parts []journalapi.ScopePart
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, journalapi.ScopePart{kv[i], kv[i+1]})
	}
	s, rej := record.ParseScope(parts)
	if rej != nil {
		t.Fatal(rej)
	}
	return s
}

func TestWritePolicy(t *testing.T) {
	p := load(t)
	r := reg(t)
	asg, _ := r.Active("assignment")
	lms, _ := r.Active("lms_assignment")
	note, _ := r.Active("attendance_note")
	ps, _ := p.Service("openclaw/glovebox-glovebox-connector-powerschool")
	if rej := ps.MayWrite(asg, scope(t, "source", "powerschool", "data_subject", "e_child1")); rej != nil {
		t.Errorf("powerschool writing assignment: %v", rej)
	}
	for name, c := range map[string]struct {
		svc   string
		typ   record.Type
		scope record.Scope
	}{
		"other type":            {"openclaw/glovebox-glovebox-connector-powerschool", lms, scope(t, "source", "schoology")},
		"other source":          {"openclaw/glovebox-glovebox-connector-powerschool", asg, scope(t, "source", "schoology")},
		"owner != source":       {"openclaw/shared-writer", asg, scope(t, "source", "schoology")},
		"household via records": {"openclaw/openclaw-gateway", note, scope(t, "source", "household")},
		"reader cannot write":   {"trunchbull/trunchbull", asg, scope(t, "source", "powerschool")},
	} {
		svc, _ := p.Service(c.svc)
		if rej := svc.MayWrite(c.typ, c.scope); rej == nil || rej.Status != 403 || rej.Reason != "policy" {
			t.Errorf("%s: %v", name, rej)
		}
	}
	gw, _ := p.Service("openclaw/openclaw-gateway")
	if rej := gw.MayWriteNote(note, "e_parent1"); rej != nil {
		t.Errorf("gateway note: %v", rej)
	}
	dash, _ := p.Service("trunchbull/trunchbull")
	if rej := dash.MayWriteNote(note, "e_parent1"); rej == nil {
		t.Error("the dashboard wrote a note")
	}
}

func TestReadPolicy(t *testing.T) {
	p := load(t)
	gw, _ := p.Service("openclaw/openclaw-gateway")
	dash, _ := p.Service("trunchbull/trunchbull")
	ps, _ := p.Service("openclaw/glovebox-glovebox-connector-powerschool")
	admin, _ := p.Service("openclaw/glovebox-journal-admin")
	if !gw.MayRead("e_child1") || !gw.MayCount("e_child1") {
		t.Error("readers: * lets the gateway read as anyone")
	}
	if !dash.MayRead("e_parent1") || dash.MayRead("e_child1") || dash.MayCount("e_child2") {
		t.Error("the dashboard reads only as guardians")
	}
	if ps.MayRead("e_child1") || admin.MayRead("e_parent1") || !admin.Admin {
		t.Error("writers and operators read nothing")
	}
	if !ps.WritesSnapshots(reg(t)) || gw.WritesSnapshots(reg(t)) || dash.WritesSnapshots(reg(t)) || admin.WritesSnapshots(reg(t)) {
		t.Error("WritesSnapshots: only source-type writers")
	}
	if !gw.WritesNotes(reg(t)) || ps.WritesNotes(reg(t)) || dash.WritesNotes(reg(t)) || admin.WritesNotes(reg(t)) {
		t.Error("WritesNotes: only household-type writers")
	}
	if _, ok := p.Service("openclaw/glovebox-glovebox-connector-rss"); ok {
		t.Error("the RSS connector has a policy")
	}
	if got := p.Callers(); len(got) != 6 || got[0] != "openclaw/glovebox-glovebox-connector-powerschool" {
		t.Errorf("Callers() = %v", got)
	}
}

func TestPolicyRefusals(t *testing.T) {
	for name, js := range map[string]string{
		"bad caller":         `{"glovebox-rss": {"admin": true}}`,
		"wildcard caller":    `{"openclaw/*": {"admin": true}}`,
		"unknown type":       `{"a/b": {"write_types": ["nosuch"], "write_subjects": "roster_subjects"}}`,
		"no write_subjects":  `{"a/b": {"write_types": ["assignment"]}}`,
		"bad write_subjects": `{"a/b": {"write_types": ["assignment"], "write_subjects": "any"}}`,
		"reader w/o readers": `{"a/b": {"read": true}}`,
		"note w/o readers":   `{"a/b": {"write_types": ["attendance_note"]}}`,
		"grants nothing":     `{"a/b": {}}`,
		"bad readers":        `{"a/b": {"read": true, "readers": "everyone"}}`,
		"bad scope part":     `{"a/b": {"write_types": ["assignment"], "write_subjects": "roster_subjects", "write_scopes": {"Source": ["x"]}}}`,
	} {
		// Parse covers both decoding and validation: an unparseable
		// readers value is a refusal too.
		if _, err := Parse([]byte(js), reg(t)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPolicyMoreRefusals(t *testing.T) {
	long := strings.Repeat("a", 64)
	for name, js := range map[string]string{
		"empty reader id":       `{"a/b": {"read": true, "readers": [""]}}`,
		"star in list":          `{"a/b": {"read": true, "readers": ["*"]}}`,
		"duplicate reader":      `{"a/b": {"read": true, "readers": ["e_parent1", "e_parent1"]}}`,
		"bad reader id":         `{"a/b": {"read": true, "readers": ["e parent"]}}`,
		"null readers":          `{"a/b": {"read": true, "readers": null}}`,
		"readers on admin":      `{"a/b": {"admin": true, "readers": ["e_parent1"]}}`,
		"write_scopes on admin": `{"a/b": {"admin": true, "write_scopes": {"source": ["x"]}}}`,
		"scopes, note only":     `{"a/b": {"write_types": ["attendance_note"], "readers": "*", "write_scopes": {"source": ["x"]}}}`,
		"long namespace":        `{"` + long + `/b": {"admin": true}}`,
		"case duplicate key":    `{"a/b": {"write_types": ["assignment"], "write_subjects": "roster_subjects", "write_scopes": {"source": ["powerschool"]}, "WRITE_SCOPES": null}}`,
		"duplicate caller":      `{"a/b": {"admin": true}, "a/b": {"read": true, "readers": "*"}}`,
		"case variant key":      `{"a/b": {"Admin": true, "admin": false}}`,
	} {
		if _, err := Parse([]byte(js), reg(t)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(nil, nil); err == nil {
		t.Error("New accepted a nil registry")
	}
}

func TestWriteScopesRestrictSubject(t *testing.T) {
	r := reg(t)
	asg, _ := r.Active("assignment")
	p, err := Parse([]byte(`{"a/b": {"write_types": ["assignment"], "write_subjects": "roster_subjects", "write_scopes": {"data_subject": ["e_child1"]}}}`), r)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := p.Service("a/b")
	if rej := s.MayWrite(asg, scope(t, "source", "powerschool", "data_subject", "e_child1")); rej != nil {
		t.Errorf("allowed subject: %v", rej)
	}
	if rej := s.MayWrite(asg, scope(t, "source", "powerschool", "data_subject", "e_child2")); rej == nil || rej.Status != 403 {
		t.Errorf("other subject: %v", rej)
	}
	if rej := s.MayWrite(asg, scope(t, "source", "powerschool")); rej == nil || rej.Status != 403 {
		t.Errorf("missing scope part: %v", rej)
	}
}

func TestNoteOnBehalfOfIsLimitedToReaders(t *testing.T) {
	r := reg(t)
	note, _ := r.Active("attendance_note")
	p, err := Parse([]byte(`{"a/b": {"write_types": ["attendance_note"], "readers": ["e_parent1"]}}`), r)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := p.Service("a/b")
	if rej := s.MayWriteNote(note, "e_parent1"); rej != nil {
		t.Errorf("listed member: %v", rej)
	}
	if rej := s.MayWriteNote(note, "e_child1"); rej == nil || rej.Status != 403 {
		t.Errorf("unlisted member: %v", rej)
	}
}

func TestCountsDoNotImplyRead(t *testing.T) {
	p, err := Parse([]byte(`{"a/b": {"counts": true, "readers": ["e_parent1"]}}`), reg(t))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := p.Service("a/b")
	if s.MayRead("e_parent1") {
		t.Error("a counts-only service may read")
	}
	if !s.MayCount("e_parent1") || s.MayCount("e_child1") {
		t.Error("counts-only service count decisions wrong")
	}
}

func TestPolicyIsImmutable(t *testing.T) {
	r := reg(t)
	asg, _ := r.Active("assignment")
	in := map[string]Service{"a/b": {
		Read: true, Readers: Readers{IDs: []string{"e_parent1"}},
		WriteTypes: []string{"assignment"}, WriteSubjects: RosterSubjects,
		WriteScopes: map[string][]string{"source": {"powerschool"}},
	}}
	p, err := New(in, r)
	if err != nil {
		t.Fatal(err)
	}
	in["a/b"].Readers.IDs[0] = "e_child1"
	in["a/b"].WriteScopes["source"][0] = "schoology"
	in["a/b"].WriteTypes[0] = "lms_assignment"
	delete(in, "a/b")
	check := func(when string) {
		s, ok := p.Service("a/b")
		if !ok || !s.MayRead("e_parent1") || s.MayRead("e_child1") ||
			s.MayWrite(asg, scope(t, "source", "powerschool")) != nil {
			t.Errorf("%s: decisions changed", when)
		}
	}
	check("after mutating the input")
	s, _ := p.Service("a/b")
	s.Readers.IDs[0] = "e_child1"
	s.WriteScopes["source"][0] = "schoology"
	s.WriteTypes[0] = "lms_assignment"
	check("after mutating a returned Service")
}

func TestUnlistedCallersAreDenied(t *testing.T) {
	p := load(t)
	r := reg(t)
	asg, _ := r.Active("assignment")
	note, _ := r.Active("attendance_note")
	for _, caller := range []string{"openclaw/glovebox", "openclaw/glovebox-glovebox-connector-rss", ""} {
		s, ok := p.Service(caller)
		if ok {
			t.Errorf("%q has a policy", caller)
		}
		if s.MayWrite(asg, scope(t, "source", "powerschool")) == nil || s.MayWriteNote(note, "e_parent1") == nil ||
			s.MayRead("e_parent1") || s.MayCount("e_parent1") || s.WritesSnapshots(r) || s.Admin {
			t.Errorf("%q: the zero Service allows something", caller)
		}
	}
}

// A misspelled key in a security policy must be an error, never a wider
// grant (a dropped write_scopes would let a connector write any source).
func TestPolicyUnknownKeysRefused(t *testing.T) {
	for name, js := range map[string]string{
		"misspelled write_scopes": `{"a/b": {"write_types": ["assignment"], "write_subjects": "roster_subjects", "write_scope": {"source": ["powerschool"]}}}`,
		"misspelled readers":      `{"a/b": {"read": true, "reader": ["e_parent1"], "readers": ["e_parent1"]}}`,
		"unknown admin key":       `{"a/b": {"admin": true, "admins": true}}`,
	} {
		var s map[string]Service
		if err := json.Unmarshal([]byte(js), &s); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
