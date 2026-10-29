package records

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestScopeOrderAndSubject(t *testing.T) {
	s := Scope("powerschool", "assignments", "e_child1", Part("course", "c1"))
	want := []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"},
		{"data_subject", "e_child1"}, {"course", "c1"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("scope = %v", s)
	}
	if p := Part("course", "a\nb"); p[1] == "a\nb" {
		t.Fatal("Part kept a value the journal refuses")
	}
}

func TestNewSnapshotIsValidForTheJournal(t *testing.T) {
	reg := testRegistry(t)
	f := NewFields(nil)
	f.String("a", "x")
	f.Date("d", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 10, 16, 13, 0, 41, 123456789, time.FixedZone("x", 3600))
	snap := NewSnapshot("t1", 1, Scope("src", "s", "e_child1"), true,
		&journalapi.CompleteRange{Field: "d", From: "2026-10-01", To: "2026-10-09"}, now,
		[]journalapi.Record{{Key: "k1", DataSubject: "e_child1", Audience: []string{"subject", "guardians"}, Fields: f.Map()}})
	if _, err := uuid.Parse(snap.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if !snap.ObservedAt.Equal(now.Truncate(time.Millisecond)) || snap.ObservedAt.Location() != time.UTC {
		t.Fatalf("observed_at %v", snap.ObservedAt)
	}
	if _, rej := record.ValidateSnapshot(reg, snap, now); rej != nil {
		t.Fatalf("journal would refuse: %v", rej)
	}
	if other := NewSnapshot("t1", 1, Scope("src", "s", "e_child1"), true, nil, now, nil); other.SnapshotID == snap.SnapshotID {
		t.Fatal("snapshot ids repeat")
	} else if other.Records == nil {
		t.Fatal("records must encode as [], not null")
	}
}

func TestSchoolYear(t *testing.T) {
	for in, want := range map[string]string{"2026-08-01": "2026-27", "2026-07-31": "2025-26", "2027-01-15": "2026-27", "2099-12-31": "2099-00"} {
		d, _ := time.Parse("2006-01-02", in)
		if got := SchoolYear(d); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
