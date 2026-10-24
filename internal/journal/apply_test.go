package journal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestApplyCreatesThenConfirms(t *testing.T) {
	r := newRig(t)
	t1, t2 := T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	out := r.apply(snap(1, "e_child1", "c1", t1, true,
		rec("c1:1", "e_child1", `{"title": "W1", "score": 8.50}`),
		rec("c1:2", "e_child1", `{"title": "W2"}`)))
	if out.Status != journalapi.StatusApplied || out.Stats.Created != 2 {
		t.Fatalf("first: %+v", out)
	}
	c, ok := r.current("assignment", "e_child1", "c1:1")
	if !ok || c.Scope != scopeKey("e_child1", "c1") || string(c.Fields["score"]) != "8.5" ||
		!c.FirstSeen.Equal(t1) || !c.LastConfirmedAt.Equal(t1) || c.ConfirmedBy.String != uid(1) ||
		strings.Join(c.Audience, ",") != "guardians,subject" {
		t.Fatalf("current %+v", c)
	}
	ev := r.events("e_child1")
	if len(ev) != 2 || ev[0].Kind != "created" || ev[0].SnapshotID.String != uid(1) || ev[0].PrevObserved.Valid ||
		ev[0].Actor != writer || ev[0].Importance != "low" || ev[0].Version != 1 {
		t.Fatalf("events %+v", ev)
	}
	out = r.apply(snap(2, "e_child1", "c1", t2, true,
		rec("c1:1", "e_child1", `{"score": 8.5, "title": "W1"}`),
		rec("c1:2", "e_child1", `{"title": "W2"}`)))
	if out.Status != journalapi.StatusApplied || out.Stats.Unchanged != 2 || out.Stats.Created != 0 {
		t.Fatalf("second: %+v", out)
	}
	if len(r.events("e_child1")) != 2 {
		t.Error("an unchanged observation wrote events")
	}
	c, _ = r.current("assignment", "e_child1", "c1:1")
	if !c.LastSeen.Equal(t2) || !c.LastConfirmedAt.Equal(t2) || c.ConfirmedBy.String != uid(2) || !c.FirstSeen.Equal(t1) {
		t.Errorf("observation columns %+v", c)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	r := newRig(t)
	s := snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child1", `{"title": "W1"}`))
	first := r.apply(s)
	again := r.apply(s)
	if again.Status != journalapi.StatusDuplicate || again.Stats != first.Stats {
		t.Errorf("resend: %+v (first %+v)", again, first)
	}
	if r.count("record_event") != 1 || r.count("snapshot") != 1 {
		t.Error("a resend wrote something")
	}
	if out, found, err := r.j.Lookup(ctx, uid(1), writer, "assignment", scopeKey("e_child1", "c1")); err != nil || !found || out.Status != journalapi.StatusDuplicate {
		t.Errorf("Lookup: %+v %v %v", out, found, err)
	}
	if _, found, _ := r.j.Lookup(ctx, uid(99), writer, "assignment", scopeKey("e_child1", "c1")); found {
		t.Error("Lookup found an unknown snapshot")
	}
}

func TestUpdatedEventCarriesOnlyTheChangeAndItsWindow(t *testing.T) {
	r := newRig(t)
	t1, t2 := T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	r.apply(snap(1, "e_child1", "c1", t1, true, rec("c1:1", "e_child1", `{"title": "W1", "score": 1, "missing": true}`)))
	out := r.apply(snap(2, "e_child1", "c1", t2, true, rec("c1:1", "e_child1", `{"title": "W1", "score": 2}`)))
	if out.Stats.Updated != 1 {
		t.Fatalf("%+v", out)
	}
	ev := r.events("e_child1")[1]
	var changed map[string][2]json.RawMessage
	_ = json.Unmarshal(ev.Changed, &changed)
	if ev.Kind != "updated" || len(changed) != 2 || string(changed["score"][0]) != "1" || string(changed["score"][1]) != "2" ||
		string(changed["missing"][0]) != "true" || string(changed["missing"][1]) != "null" || ev.Fields != nil {
		t.Errorf("event %+v changed %s", ev, ev.Changed)
	}
	if !ev.PrevObserved.Valid || !ev.PrevObserved.Time.Equal(t1) || !ev.Observed.Equal(t2) {
		t.Errorf("window (%v, %v]", ev.PrevObserved, ev.Observed)
	}
}

// A late snapshot is stored with its payload and changes nothing (4.4 step 4).
func TestLateSnapshotNeverMovesStateBackwards(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child1", `{"title": "new"}`)))
	out := r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("c1:1", "e_child1", `{"title": "old"}`), rec("c1:9", "e_child1", `{"title": "x"}`)))
	if out.Status != journalapi.StatusLate {
		t.Fatalf("%+v", out)
	}
	c, _ := r.current("assignment", "e_child1", "c1:1")
	if string(c.Fields["title"]) != `"new"` || r.count("record_event") != 1 || r.snapshotStatus(2) != "late" {
		t.Error("a late snapshot changed state")
	}
	if _, ok := r.current("assignment", "e_child1", "c1:9"); ok {
		t.Error("a late snapshot created a record")
	}
	var payload []byte
	_ = r.j.db.QueryRow(`SELECT payload FROM snapshot WHERE id = $1::uuid`, uid(2)).Scan(&payload)
	if !strings.Contains(string(payload), `"old"`) {
		t.Error("the late payload was not kept")
	}
}

// An erase is a ban (decision 2026-10-24): every snapshot of the subject is
// refused with 410, observed before the erasure or long after it.
func TestErasedSubjectIsBannedWhateverTheObservedAt(t *testing.T) {
	r := newRig(t)
	erasedAt := T0.Add(-time.Hour)
	if _, err := r.j.db.Exec(`INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ('e_child1', $1, 'test', '{}')`, erasedAt); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{erasedAt.Add(-time.Minute), erasedAt.Add(record.MaxFutureSkew), T0.Add(record.MaxFutureSkew - time.Second)} {
		out := r.apply(snap(i+1, "e_child1", "c1", at, true, rec("c1:1", "e_child1", `{"title": "x"}`)))
		if out.Rejection == nil || out.Rejection.Status != 410 || out.Rejection.Reason != journalapi.ReasonErasedSubject ||
			!strings.Contains(out.Rejection.Detail, "ban") {
			t.Errorf("observed %v: %+v", at, out)
		}
	}
	if r.count("snapshot") != 0 {
		t.Error("a refused snapshot was stored")
	}
	if out := r.apply(snap(9, "e_child2", "c1", T0, true, rec("c1:1", "e_child2", `{"title": "x"}`))); out.Status != journalapi.StatusApplied {
		t.Errorf("another subject: %+v", out)
	}
}

func TestUnknownSubjectIsRefusedAndNothingStored(t *testing.T) {
	r := newRig(t)
	out := r.apply(snap(1, "e_stranger", "c1", T0, true, rec("c1:1", "e_stranger", `{"title": "x"}`)))
	if out.Rejection == nil || out.Rejection.Status != 409 || out.Rejection.Reason != journalapi.ReasonUnknownSubject {
		t.Fatalf("%+v", out)
	}
	if r.count("snapshot") != 0 || r.count("record_current") != 0 {
		t.Error("stored")
	}
	r.ros.Store(nil) // no roster loaded: every subject is unknown
	if out := r.apply(snap(2, "e_child1", "c1", T0, true)); out.Rejection == nil || out.Rejection.Status != 409 {
		t.Errorf("without a roster: %+v", out)
	}
}

func TestTextValuesCarryTheirOwnVerdicts(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("c1:1", "e_child1", `{"title": "W", "comment": "INJECT this"}`)))
	c, _ := r.current("assignment", "e_child1", "c1:1")
	if c.Scan["comment"].Verdict != "quarantine" || c.Scan["comment"].ScannerVersion != "scanner-1" {
		t.Errorf("current scan %+v", c.Scan)
	}
	if r.rec.withheld != 1 {
		t.Errorf("withheld counted %d", r.rec.withheld)
	}
	calls := r.scanner.Calls()
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child1", `{"title": "W", "comment": "fine now"}`)))
	ev := r.events("e_child1")[1]
	var scan map[string]record.EventScan
	_ = json.Unmarshal(ev.Scan, &scan)
	if scan["comment"].Old != "quarantine" || scan["comment"].New != "pass" {
		t.Errorf("event scan %s", ev.Scan)
	}
	if r.scanner.Calls() != calls+1 {
		t.Errorf("scanned %d values; only the new one needed it", r.scanner.Calls()-calls)
	}
	r.apply(snap(3, "e_child1", "c1", T0, true, rec("c1:1", "e_child1", `{"title": "W", "comment": "fine now"}`)))
	if r.scanner.Calls() != calls+1 {
		t.Error("an unchanged value was scanned again")
	}
}

func TestRulesAreStampedOnEvents(t *testing.T) {
	r := newRig(t)
	s := journalapi.Snapshot{SnapshotID: uid(1), RecordType: "attendance_day", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "attendance_days"}, {"data_subject", "e_child1"}, {"school_year", "2026-27"}},
		Complete: true, CompleteRange: &journalapi.CompleteRange{Field: "date", From: "2026-09-01", To: "2026-10-15"},
		ObservedAt: T0, Records: []journalapi.Record{rec("2026-10-14", "e_child1", `{"date": "2026-10-14", "classification": "full_day", "any_unexcused": true}`)}}
	r.apply(s)
	ev := r.events("e_child1")[0]
	if ev.Importance != "high" || ev.Rule != "full_day_absence" || ev.GroupKey != "data_subject=e_child1/field:date=2026-10-14" ||
		!strings.Contains(string(ev.Notify), `"attendance_followup"`) {
		t.Errorf("event %+v notify %s", ev, ev.Notify)
	}
	if r.rec.events["created/high"] != 1 || r.rec.snapshots["applied"] != 1 {
		t.Errorf("recorder %+v %+v", r.rec.events, r.rec.snapshots)
	}
}
