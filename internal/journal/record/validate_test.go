package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

var now = time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	day := Type{Name: "attendance_day", SchemaVersion: 1, Owner: "powerschool",
		Fields:          map[string]FieldDef{"date": {Type: Date}, "classification": {Type: String}},
		DefaultAudience: []string{"subject", "guardians"}}
	cal := Type{Name: "calendar_event", SchemaVersion: 1, Owner: "gcalendar",
		Fields: map[string]FieldDef{"title": {Type: String}}, DefaultAudience: []string{"household"}, AllowNoSubject: true}
	r, err := NewRegistry([]Type{assignmentV1, assignmentV2(), noteV1, day, cal})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func goodSnapshot() journalapi.Snapshot {
	return journalapi.Snapshot{
		SnapshotID: "0b9f6c8e-6a64-4c49-9f55-6f4cf1b3b0a1", RecordType: "assignment", SchemaVersion: 1,
		Scope:    parts("source", "powerschool", "surface", "assignments", "data_subject", "e_child1", "course", "c1"),
		Complete: true, ObservedAt: now.Add(-time.Minute),
		Records: []journalapi.Record{
			{Key: "c1:1", DataSubject: "e_child1", Fields: map[string]json.RawMessage{"title": raw(`"W1"`), "comment": raw(`"ok"`)}},
			{Key: "c1:2", DataSubject: "e_child1", Audience: []string{"guardians", "guardians"}, Fields: map[string]json.RawMessage{"title": raw(`"W2"`)}},
		},
	}
}

func TestValidateSnapshotNormalizesAndRenames(t *testing.T) {
	n, rej := ValidateSnapshot(testRegistry(t), goodSnapshot(), now)
	if rej != nil {
		t.Fatal(rej)
	}
	if n.Type.SchemaVersion != 2 || n.Version != 1 || n.Subject != "e_child1" || n.ID != "0b9f6c8e-6a64-4c49-9f55-6f4cf1b3b0a1" {
		t.Errorf("normalized %+v", n)
	}
	if n.ScopeKey != "source=powerschool/surface=assignments/data_subject=e_child1/course=c1" {
		t.Errorf("scope key %s", n.ScopeKey)
	}
	r0 := n.Records[0]
	if string(r0.Fields["name"]) != `"W1"` || string(r0.Fields["teacher_comment"]) != `"ok"` || r0.Fields["title"] != nil {
		t.Errorf("v1 names are renamed to the active version: %v", r0.Fields)
	}
	if strings.Join(r0.Audience, ",") != "guardians,subject" {
		t.Errorf("default audience, sorted: %v", r0.Audience)
	}
	if strings.Join(n.Records[1].Audience, ",") != "guardians" {
		t.Errorf("audience deduplicated: %v", n.Records[1].Audience)
	}
	if len(r0.Hash) == 0 {
		t.Error("no hash")
	}
}

func TestValidateSnapshotRefusals(t *testing.T) {
	reg := testRegistry(t)
	type c struct {
		mod    func(*journalapi.Snapshot)
		status int
		reason string
	}
	for name, tc := range map[string]c{
		"bad id":           {func(s *journalapi.Snapshot) { s.SnapshotID = "x" }, 422, "schema"},
		"unknown type":     {func(s *journalapi.Snapshot) { s.RecordType = "nosuch" }, 422, "schema"},
		"unknown version":  {func(s *journalapi.Snapshot) { s.SchemaVersion = 9 }, 422, "schema"},
		"household type":   {func(s *journalapi.Snapshot) { s.RecordType = "attendance_note" }, 422, "schema"},
		"future":           {func(s *journalapi.Snapshot) { s.ObservedAt = now.Add(11 * time.Minute) }, 422, "future_observed_at"},
		"wrong owner":      {func(s *journalapi.Snapshot) { s.Scope[0][1] = "gcalendar" }, 422, "schema"},
		"year 10000":       {func(s *journalapi.Snapshot) { s.ObservedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, 422, "schema"},
		"bidi in key":      {func(s *journalapi.Snapshot) { s.Records[0].Key = "a\u202eb" }, 422, "schema"},
		"bad field name":   {func(s *journalapi.Snapshot) { s.Records[0].Fields["Bad Name"] = raw(`"x"`) }, 422, "schema"},
		"no observed_at":   {func(s *journalapi.Snapshot) { s.ObservedAt = time.Time{} }, 422, "schema"},
		"subject mismatch": {func(s *journalapi.Snapshot) { s.Records[0].DataSubject = "e_child2" }, 422, "schema"},
		"duplicate key":    {func(s *journalapi.Snapshot) { s.Records[1].Key = "c1:1" }, 422, "duplicate_key"},
		"empty key":        {func(s *journalapi.Snapshot) { s.Records[0].Key = "" }, 422, "schema"},
		"bad token":        {func(s *journalapi.Snapshot) { s.Records[0].Audience = []string{"Everyone"} }, 422, "schema"},
		"too many records": {func(s *journalapi.Snapshot) { s.Records = make([]journalapi.Record, MaxRecords+1) }, 413, "too_large"},
		"oversized string": {func(s *journalapi.Snapshot) {
			s.Records[0].Fields["title"] = raw(`"` + strings.Repeat("x", 2000) + `"`)
		}, 413, "too_large"},
		"range not complete": {func(s *journalapi.Snapshot) {
			s.Complete = false
			s.CompleteRange = &journalapi.CompleteRange{Field: "due", From: "2026-09-01", To: "2026-10-16"}
		}, 422, "schema"},
		"range on non-date": {func(s *journalapi.Snapshot) {
			s.CompleteRange = &journalapi.CompleteRange{Field: "title", From: "2026-09-01", To: "2026-10-16"}
		}, 422, "schema"},
		"range reversed": {func(s *journalapi.Snapshot) {
			s.CompleteRange = &journalapi.CompleteRange{Field: "due", From: "2026-10-16", To: "2026-09-01"}
		}, 422, "schema"},
	} {
		s := goodSnapshot()
		tc.mod(&s)
		_, rej := ValidateSnapshot(reg, s, now)
		if rej == nil || rej.Status != tc.status || rej.Reason != tc.reason {
			t.Errorf("%s: %v, want %d %s", name, rej, tc.status, tc.reason)
		}
	}
}

func TestValidateSnapshotRangeIsRenamed(t *testing.T) {
	s := goodSnapshot()
	s.CompleteRange = &journalapi.CompleteRange{Field: "due", From: "2026-09-01", To: "2026-10-16"}
	n, rej := ValidateSnapshot(testRegistry(t), s, now)
	if rej != nil || n.Range == nil || n.Range.Field != "due" {
		t.Fatalf("%v %v", n, rej)
	}
	if !n.Range.Contains(raw(`"2026-10-01"`)) || n.Range.Contains(raw(`"2026-10-17"`)) || n.Range.Contains(nil) {
		t.Error("Range.Contains")
	}
}

func TestValidateSnapshotSubjectlessScopes(t *testing.T) {
	reg := testRegistry(t)
	s := journalapi.Snapshot{SnapshotID: "1b9f6c8e-6a64-4c49-9f55-6f4cf1b3b0a1", RecordType: "calendar_event", SchemaVersion: 1,
		Scope: parts("source", "gcalendar", "calendar", "family"), ObservedAt: now,
		Records: []journalapi.Record{{Key: "ev1", Fields: map[string]json.RawMessage{"title": raw(`"T"`)}}}}
	if _, rej := ValidateSnapshot(reg, s, now); rej != nil {
		t.Errorf("a type that allows no subject: %v", rej)
	}
	s.Records[0].DataSubject = "e_child1"
	if _, rej := ValidateSnapshot(reg, s, now); rej == nil {
		t.Error("a record with a subject in a scope without one was accepted")
	}
	a := goodSnapshot()
	a.Scope = parts("source", "powerschool", "surface", "assignments")
	for i := range a.Records {
		a.Records[i].DataSubject = ""
	}
	if _, rej := ValidateSnapshot(reg, a, now); rej == nil {
		t.Error("a subjectless scope for a type that needs subjects was accepted")
	}
}

func TestValidateNote(t *testing.T) {
	reg := testRegistry(t)
	n, rej := ValidateNote(reg, journalapi.Note{RecordType: "attendance_note", Date: "2026-10-14", DataSubject: "e_child1",
		OnBehalfOf: "e_parent1", Fields: map[string]json.RawMessage{"what_happened": raw(`"sick"`), "heard_from": raw(`"subject"`)}})
	if rej != nil {
		t.Fatal(rej)
	}
	if n.Key != "2026-10-14:e_parent1" || string(n.Fields["date"]) != `"2026-10-14"` ||
		strings.Join(n.Audience, ",") != "guardians,subject" ||
		n.Scope != "source=household/record_type=attendance_note/data_subject=e_child1" {
		t.Errorf("note %+v", n)
	}
	for name, note := range map[string]journalapi.Note{
		"not household": {RecordType: "assignment", Date: "2026-10-14", DataSubject: "e_child1", OnBehalfOf: "e_parent1"},
		"bad date":      {RecordType: "attendance_note", Date: "14/10", DataSubject: "e_child1", OnBehalfOf: "e_parent1"},
		"no writer":     {RecordType: "attendance_note", Date: "2026-10-14", DataSubject: "e_child1"},
		"no subject":    {RecordType: "attendance_note", Date: "2026-10-14", OnBehalfOf: "e_parent1"},
		"date twice": {RecordType: "attendance_note", Date: "2026-10-14", DataSubject: "e_child1", OnBehalfOf: "e_parent1",
			Fields: map[string]json.RawMessage{"date": raw(`"2026-10-13"`)}},
		"bad field name": {RecordType: "attendance_note", Date: "2026-10-14", DataSubject: "e_child1", OnBehalfOf: "e_parent1",
			Fields: map[string]json.RawMessage{"Bad Name": raw(`"x"`)}},
		"control in writer": {RecordType: "attendance_note", Date: "2026-10-14", DataSubject: "e_child1", OnBehalfOf: "e_par\nent1"},
		"undeclared": {RecordType: "attendance_note", Date: "2026-10-14", DataSubject: "e_child1", OnBehalfOf: "e_parent1",
			Fields: map[string]json.RawMessage{"audience": raw(`["public"]`)}},
	} {
		if _, rej := ValidateNote(reg, note); rej == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
