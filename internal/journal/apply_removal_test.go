package journal

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func TestRemovalAndRestore(t *testing.T) {
	r := newRig(t)
	t1, t2, t3 := T0.Add(-3*time.Hour), T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	r.apply(snap(1, "e_child1", "c1", t1, true, rec("c1:1", "e_child1", `{"title": "A"}`), rec("c1:2", "e_child1", `{"title": "B", "comment": "note"}`)))
	out := r.apply(snap(2, "e_child1", "c1", t2, true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	if out.Stats.Removed != 1 {
		t.Fatalf("%+v", out)
	}
	c, _ := r.current("assignment", "e_child1", "c1:2")
	if !c.Removed || string(c.Fields["title"]) != `"B"` || !c.LastSeen.Equal(t1) || !c.LastConfirmedAt.Equal(t2) {
		t.Errorf("tombstone %+v", c)
	}
	rm := r.events("e_child1")[2]
	var last map[string]json.RawMessage
	_ = json.Unmarshal(rm.Changed, &last)
	if rm.Kind != "removed" || string(last["title"]) != `"B"` || rm.Fields != nil || !rm.PrevObserved.Time.Equal(t1) {
		t.Errorf("removed event %+v %s", rm, rm.Changed)
	}
	var scan map[string]map[string]string
	_ = json.Unmarshal(rm.Scan, &scan)
	if scan["comment"]["old"] != "pass" {
		t.Errorf("removed event scan %s", rm.Scan)
	}
	r.apply(snap(3, "e_child1", "c1", t3, true, rec("c1:1", "e_child1", `{"title": "A"}`), rec("c1:2", "e_child1", `{"title": "B2"}`)))
	back := r.events("e_child1")[3]
	if back.Kind != "restored" || !back.PrevObserved.Time.Equal(t2) {
		t.Errorf("restored %+v", back)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(back.Fields, &fields)
	if string(fields["title"]) != `"B2"` || len(fields) != 1 {
		t.Errorf("restored carries the full new fields: %s", back.Fields)
	}
	c, _ = r.current("assignment", "e_child1", "c1:2")
	if c.Removed || c.Fields["comment"] != nil || c.Scan["comment"].Verdict != "" {
		t.Errorf("after restore %+v", c)
	}
}

func TestIncompleteSnapshotNeverRemoves(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	out := r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), false))
	if out.Status != journalapi.StatusApplied || out.Stats.Removed != 0 {
		t.Fatalf("%+v", out)
	}
	if c, _ := r.current("assignment", "e_child1", "c1:1"); c.Removed {
		t.Error("removed by an incomplete snapshot")
	}
}

func TestScopeMoveAndNoRemovalFromTheOldScope(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child1", "c2", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	ev := r.events("e_child1")[1]
	if ev.Kind != "updated" || ev.PrevScope != scopeKey("e_child1", "c1") || ev.Scope != scopeKey("e_child1", "c2") || string(ev.Changed) != "{}" {
		t.Errorf("move %+v changed %s", ev, ev.Changed)
	}
	out := r.apply(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true))
	if out.Stats.Removed != 0 {
		t.Errorf("the old scope removed a record that moved away: %+v", out)
	}
}

// Decision 6: a snapshot of the old scope observed before the move (but
// after that scope's own last snapshot) leaves the moved record alone.
func TestStaleRecordFromAnOlderObservationIsLeftAlone(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-4*time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child1", "c2", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "moved"}`)))
	out := r.apply(snap(3, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	if out.Status != journalapi.StatusApplied || out.Stats.Stale != 1 {
		t.Fatalf("%+v", out)
	}
	c, _ := r.current("assignment", "e_child1", "k")
	if c.Scope != scopeKey("e_child1", "c2") || string(c.Fields["title"]) != `"moved"` {
		t.Errorf("state moved backwards: %+v", c)
	}
}

func TestCompleteRangeLimitsRemoval(t *testing.T) {
	r := newRig(t)
	days := func(n int, observed time.Time, from, to string, recs ...journalapi.Record) journalapi.Snapshot {
		return journalapi.Snapshot{SnapshotID: uid(n), RecordType: "attendance_day", SchemaVersion: 1,
			Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "attendance_days"}, {"data_subject", "e_child1"}, {"school_year", "2026-27"}},
			Complete: true, CompleteRange: &journalapi.CompleteRange{Field: "date", From: from, To: to}, ObservedAt: observed, Records: recs}
	}
	day := func(d string) journalapi.Record {
		return rec(d, "e_child1", `{"date": "`+d+`", "classification": "partial_day", "any_unexcused": false}`)
	}
	r.apply(days(1, T0.Add(-2*time.Hour), "2026-09-01", "2026-10-15", day("2026-09-10"), day("2026-10-14")))
	// New term: the grid shows only October 16 onwards, with no codes yet.
	out := r.apply(days(2, T0.Add(-time.Hour), "2026-10-16", "2026-12-20"))
	if out.Stats.Removed != 0 {
		t.Errorf("last term's days were removed: %+v", out)
	}
	// A correction inside the shown range removes exactly that day.
	out = r.apply(days(3, T0, "2026-09-01", "2026-10-15", day("2026-09-10")))
	if out.Stats.Removed != 1 {
		t.Errorf("%+v", out)
	}
	if c, _ := r.current("attendance_day", "e_child1", "2026-10-14"); !c.Removed {
		t.Error("the corrected day is not removed")
	}
}

func TestAudienceChangeRecordsBothAudiences(t *testing.T) {
	r := newRig(t)
	a := rec("k", "e_child1", `{"title": "A"}`)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, a))
	a.Audience = []string{"guardians"}
	a.Fields = map[string]json.RawMessage{"title": json.RawMessage(`"B"`)}
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, a))
	ev := r.events("e_child1")[1]
	if len(ev.PrevAudience) != 2 || len(ev.Audience) != 1 || ev.Audience[0] != "guardians" {
		t.Errorf("%+v", ev)
	}
}

// The removal guard counts live keys inside complete_range only: four of the
// ten days the grid shows is a mass removal even though it is a small share of
// everything the scope holds, and the days outside the range are neither
// counted nor removed.
func TestCompleteRangeGuardCountsOnlyKeysInRange(t *testing.T) {
	r := newRig(t)
	days := func(n int, observed time.Time, from, to string, recs ...journalapi.Record) journalapi.Snapshot {
		return journalapi.Snapshot{SnapshotID: uid(n), RecordType: "attendance_day", SchemaVersion: 1,
			Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "attendance_days"}, {"data_subject", "e_child1"}, {"school_year", "2026-27"}},
			Complete: true, CompleteRange: &journalapi.CompleteRange{Field: "date", From: from, To: to}, ObservedAt: observed, Records: recs}
	}
	day := func(d string) journalapi.Record {
		return rec(d, "e_child1", `{"date": "`+d+`", "classification": "partial_day", "any_unexcused": false}`)
	}
	var all, inRange []journalapi.Record
	for i := 1; i <= 30; i++ {
		all = append(all, day(fmt.Sprintf("2026-08-%02d", i)))
	}
	for i := 1; i <= 10; i++ {
		d := day(fmt.Sprintf("2026-09-%02d", i))
		all = append(all, d)
		inRange = append(inRange, d)
	}
	r.apply(days(1, T0.Add(-2*time.Hour), "2026-08-01", "2026-09-30", all...))
	// 4 of the 10 in-range days vanish: 40 percent of 10, but only 10 percent of 40.
	out := r.apply(days(2, T0.Add(-time.Hour), "2026-09-01", "2026-09-30", inRange[:6]...))
	if out.Status != journalapi.StatusHeld {
		t.Fatalf("expected held, got %+v", out)
	}
	if c, _ := r.current("attendance_day", "e_child1", "2026-09-10"); c.Removed {
		t.Error("a held snapshot removed a day")
	}
	// Three of ten is exactly at the threshold and is applied; the August
	// days stay live.
	out = r.apply(days(3, T0, "2026-09-01", "2026-09-30", inRange[:7]...))
	if out.Status != journalapi.StatusApplied || out.Stats.Removed != 3 {
		t.Fatalf("%+v", out)
	}
	if c, _ := r.current("attendance_day", "e_child1", "2026-08-15"); c.Removed {
		t.Error("a day outside the range was removed")
	}
}
