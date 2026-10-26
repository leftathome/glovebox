package policy

import (
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// MayWriteNote needs that specific household type in write_types, not any
// household type.
func TestMayWriteNoteNeedsThatType(t *testing.T) {
	r, err := record.NewRegistry([]record.Type{
		{Name: "attendance_note", SchemaVersion: 1, Owner: record.OwnerHousehold, Fields: map[string]record.FieldDef{"date": {Type: record.Date}}, FixedAudience: []string{"subject", "guardians"}},
		{Name: "pickup_note", SchemaVersion: 1, Owner: record.OwnerHousehold, Fields: map[string]record.FieldDef{"date": {Type: record.Date}}, FixedAudience: []string{"guardians"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(map[string]Service{"openclaw/notes": {WriteTypes: []string{"attendance_note"}, Readers: Readers{Any: true}}}, r)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := p.Service("openclaw/notes")
	att, _ := r.Active("attendance_note")
	pick, _ := r.Active("pickup_note")
	if rej := s.MayWriteNote(att, "e_parent1"); rej != nil {
		t.Errorf("its own type: %+v", rej)
	}
	if rej := s.MayWriteNote(pick, "e_parent1"); rej == nil || rej.Status != 403 {
		t.Errorf("another household type: %+v", rej)
	}
	if !s.WritesNotes(r) {
		t.Error("WritesNotes")
	}
}

// Allows compares member ids exactly: no case folding, no trimming.
func TestAllowsIsExact(t *testing.T) {
	rs := Readers{IDs: []string{"e_parent1", "e_parent2"}}
	if !rs.Allows("e_parent1") || !rs.Allows("e_parent2") {
		t.Error("listed members refused")
	}
	nl, tab := string(rune(10)), string(rune(9))
	for _, id := range []string{"E_PARENT1", "E_parent1", "e_parent1 ", " e_parent1", "e_parent1" + nl, tab + "e_parent1", "e_parent", "e_parent12", ""} {
		if rs.Allows(id) {
			t.Errorf("%q allowed", id)
		}
	}
	s := Service{Read: true, Counts: true, Readers: rs}
	if s.MayRead("E_PARENT1") || s.MayCount("e_parent1 ") {
		t.Error("MayRead/MayCount fold or trim")
	}
}
