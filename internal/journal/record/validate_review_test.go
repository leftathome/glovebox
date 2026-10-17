package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func TestValidateSnapshotReviewRefusals(t *testing.T) {
	reg := testRegistry(t)
	for name, mod := range map[string]func(*journalapi.Snapshot){
		"key too long":   func(s *journalapi.Snapshot) { s.Records[0].Key = strings.Repeat("k", MaxKeyBytes+1) },
		"key bad utf8":   func(s *journalapi.Snapshot) { s.Records[0].Key = "a\xffb" },
		"key blank":      func(s *journalapi.Snapshot) { s.Records[0].Key = "  \t" },
		"key zero width": func(s *journalapi.Snapshot) { s.Records[0].Key = "a\u200bb" },
		"range not a day": func(s *journalapi.Snapshot) {
			s.CompleteRange = &journalapi.CompleteRange{Field: "due", From: "2026-02-30", To: "2026-03-01"}
		},
		"range undeclared": func(s *journalapi.Snapshot) {
			s.CompleteRange = &journalapi.CompleteRange{Field: "nosuch", From: "2026-01-01", To: "2026-03-01"}
		},
		"range bad name": func(s *journalapi.Snapshot) {
			s.CompleteRange = &journalapi.CompleteRange{Field: "No Such", From: "2026-01-01", To: "2026-03-01"}
		},
		"record type bad": func(s *journalapi.Snapshot) { s.RecordType = "Bad Type" },
		"future +10m+1ns": func(s *journalapi.Snapshot) { s.ObservedAt = now.Add(MaxFutureSkew + time.Nanosecond) },
	} {
		s := goodSnapshot()
		mod(&s)
		if _, rej := ValidateSnapshot(reg, s, now); rej == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateSnapshotBoundaries(t *testing.T) {
	s := goodSnapshot()
	s.ObservedAt = now.Add(MaxFutureSkew)
	s.Records[0].Key = strings.Repeat("k", MaxKeyBytes)
	if _, rej := ValidateSnapshot(testRegistry(t), s, now); rej != nil {
		t.Errorf("exactly +10m and a maximal key: %v", rej)
	}
}

func TestRejectionDetailsAreBounded(t *testing.T) {
	reg := testRegistry(t)
	big := strings.Repeat("x", 1<<20)
	s := goodSnapshot()
	s.RecordType = big
	_, rej := ValidateSnapshot(reg, s, now)
	if rej == nil || len(rej.Detail) > 200 {
		t.Errorf("record_type echoed: %v", rej)
	}
	s = goodSnapshot()
	s.CompleteRange = &journalapi.CompleteRange{Field: big, From: "2026-01-01", To: "2026-02-01"}
	if _, rej = ValidateSnapshot(reg, s, now); rej == nil || len(rej.Detail) > 200 {
		t.Errorf("range field echoed: %v", rej)
	}
	_, rej = ValidateNote(reg, journalapi.Note{RecordType: big, Date: "2026-10-14", DataSubject: "e_child1", OnBehalfOf: "e_parent1"})
	if rej == nil || len(rej.Detail) > 200 {
		t.Errorf("note record_type echoed: %v", rej)
	}
}

func TestRangeFieldMustExistInActiveVersion(t *testing.T) {
	v1 := assignmentV1
	v2 := assignmentV1
	v2.SchemaVersion = 2
	v2.Fields = map[string]FieldDef{"title": {Type: String, Required: true}, "score": {Type: Number}, "comment": {Type: Text}}
	reg, err := NewRegistry([]Type{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	s := goodSnapshot()
	s.CompleteRange = &journalapi.CompleteRange{Field: "due", From: "2026-09-01", To: "2026-10-16"}
	s.Records[0].Fields = map[string]json.RawMessage{"title": raw(`"W1"`)}
	s.Records[1].Fields = map[string]json.RawMessage{"title": raw(`"W2"`)}
	if _, rej := ValidateSnapshot(reg, s, now); rej == nil {
		t.Error("a range on a field the active version dropped was accepted")
	}
}

func TestRegistryRefusesNewRequiredFields(t *testing.T) {
	v2 := assignmentV1
	v2.SchemaVersion = 2
	v2.Fields = map[string]FieldDef{"title": {Type: String, Required: true}, "extra": {Type: String, Required: true}, "comment": {Type: Text}}
	if _, err := NewRegistry([]Type{assignmentV1, v2}); err == nil {
		t.Error("a new required field was accepted")
	}
	v2.Fields = map[string]FieldDef{"title": {Type: String, Required: true}, "score": {Type: Number}, "due": {Type: Date, Required: true}, "comment": {Type: Text}}
	if _, err := NewRegistry([]Type{assignmentV1, v2}); err == nil {
		t.Error("a field made required was accepted")
	}
	v2.Fields = map[string]FieldDef{"title": {Type: String, Required: true}, "extra": {Type: String}, "comment": {Type: Text}}
	if _, err := NewRegistry([]Type{assignmentV1, v2}); err != nil {
		t.Errorf("a new optional field: %v", err)
	}
}

func TestSuspiciousInvisibles(t *testing.T) {
	for _, r := range []rune{0x200e, 0x200f, 0x061c, 0x200b, 0xfeff, 0x2028, 0x2029} {
		if !suspicious(r) {
			t.Errorf("%U not suspicious", r)
		}
	}
	got, rej := NormalizeValue(String, raw("\"a\u200bb\""))
	if rej != nil || strings.ContainsRune(string(got), 0x200b) {
		t.Errorf("zero-width kept: %q %v", got, rej)
	}
}

func TestEmptyAudienceTakesTypeDefault(t *testing.T) {
	s := goodSnapshot()
	s.Records[1].Audience = []string{}
	n, rej := ValidateSnapshot(testRegistry(t), s, now)
	if rej != nil || strings.Join(n.Records[1].Audience, ",") != "guardians,subject" {
		t.Errorf("%v %v", n, rej)
	}
}

func TestRangeContainsInclusive(t *testing.T) {
	r := &Range{Field: "due", From: "2026-09-01", To: "2026-10-16"}
	if !r.Contains(raw(`"2026-09-01"`)) || !r.Contains(raw(`"2026-10-16"`)) || r.Contains(raw(`"2026-08-31"`)) || r.Contains(raw(`7`)) {
		t.Error("Contains ends")
	}
}
