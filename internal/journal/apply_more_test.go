package journal

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// tenRecords builds records c1:1 .. c1:n with the given title suffix.
func tenRecords(n int, suffix string) []journalapi.Record {
	var out []journalapi.Record
	for i := 1; i <= n; i++ {
		out = append(out, rec(fmt.Sprintf("c1:%d", i), "e_child1", fmt.Sprintf(`{"title": "W%d%s"}`, i, suffix)))
	}
	return out
}

// A complete snapshot that omits a key removes it; the record stays as a
// tombstone and its reappearance is a restore.
func TestCompleteSnapshotRemovesAndRestores(t *testing.T) {
	r := newRig(t)
	t1, t2, t3 := T0.Add(-3*time.Hour), T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	r.apply(snap(1, "e_child1", "c1", t1, true, tenRecords(10, "")...))
	out := r.apply(snap(2, "e_child1", "c1", t2, true, tenRecords(9, "")...))
	if out.Status != journalapi.StatusApplied || out.Stats.Removed != 1 || out.Stats.Unchanged != 9 {
		t.Fatalf("removal: %+v", out)
	}
	c, ok := r.current("assignment", "e_child1", "c1:10")
	if !ok || !c.Removed || !c.LastSeen.Equal(t1) || !c.LastConfirmedAt.Equal(t2) || c.ConfirmedBy.String != uid(2) {
		t.Fatalf("tombstone %+v", c)
	}
	ev := r.events("e_child1")
	rm := ev[len(ev)-1]
	var before map[string]json.RawMessage
	_ = json.Unmarshal(rm.Changed, &before)
	if rm.Kind != "removed" || rm.Key != "c1:10" || string(before["title"]) != `"W10"` || rm.Fields != nil ||
		!rm.PrevObserved.Valid || !rm.PrevObserved.Time.Equal(t1) || !rm.Observed.Equal(t2) {
		t.Errorf("removed event %+v changed %s", rm, rm.Changed)
	}
	back := append(tenRecords(9, ""), rec("c1:10", "e_child1", `{"title": "back"}`))
	out = r.apply(snap(3, "e_child1", "c1", t3, true, back...))
	if out.Stats.Restored != 1 || out.Stats.Unchanged != 9 {
		t.Fatalf("restore: %+v", out)
	}
	ev = r.events("e_child1")
	rs := ev[len(ev)-1]
	if rs.Kind != "restored" || !strings.Contains(string(rs.Fields), `"back"`) || !rs.PrevObserved.Time.Equal(t2) {
		t.Errorf("restored event %+v fields %s", rs, rs.Fields)
	}
	c, _ = r.current("assignment", "e_child1", "c1:10")
	if c.Removed || !c.FirstSeen.Equal(t1) || !c.LastSeen.Equal(t3) {
		t.Errorf("restored row %+v", c)
	}
}

// An incomplete snapshot never removes anything.
func TestIncompleteSnapshotDoesNotRemove(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, tenRecords(2, "")...))
	out := r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), false, tenRecords(1, "")...))
	if out.Stats.Removed != 0 || out.Stats.Unchanged != 1 {
		t.Fatalf("%+v", out)
	}
	if c, _ := r.current("assignment", "e_child1", "c1:2"); c.Removed {
		t.Error("removed by an incomplete snapshot")
	}
}

// A record that moves scope is an updated event carrying prev_scope, and an
// older observation of the old scope does not pull it back (decision 6).
func TestScopeMoveAndStale(t *testing.T) {
	r := newRig(t)
	t1, t2, t3 := T0.Add(-3*time.Hour), T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	r.apply(snap(1, "e_child1", "c1", t1, true, rec("k", "e_child1", `{"title": "a"}`)))
	out := r.apply(snap(2, "e_child1", "c2", t3, true, rec("k", "e_child1", `{"title": "a"}`)))
	if out.Stats.Updated != 1 {
		t.Fatalf("move: %+v", out)
	}
	ev := r.events("e_child1")
	mv := ev[len(ev)-1]
	if mv.Kind != "updated" || mv.PrevScope != scopeKey("e_child1", "c1") || mv.Scope != scopeKey("e_child1", "c2") || string(mv.Changed) != "{}" {
		t.Errorf("move event %+v changed %s", mv, mv.Changed)
	}
	out = r.apply(snap(3, "e_child1", "c1", t2, true, rec("k", "e_child1", `{"title": "a"}`)))
	if out.Status != journalapi.StatusApplied || out.Stats.Stale != 1 || out.Stats.Updated != 0 {
		t.Fatalf("stale: %+v", out)
	}
	c, _ := r.current("assignment", "e_child1", "k")
	if c.Scope != scopeKey("e_child1", "c2") || len(r.events("e_child1")) != 2 {
		t.Errorf("stale snapshot moved the record back: %+v", c)
	}
}

// An audience change is an updated event with prev_audience; a created event
// has none.
func TestAudienceChangeCarriesPrevAudience(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "a"}`)))
	s := snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "a"}`))
	s.Records[0].Audience = []string{"guardians"}
	r.apply(s)
	ev := r.events("e_child1")
	u := ev[len(ev)-1]
	if u.Kind != "updated" || strings.Join(u.Audience, ",") != "guardians" || strings.Join(u.PrevAudience, ",") != "guardians,subject" {
		t.Errorf("event %+v", u)
	}
	if ev[0].PrevAudience != nil {
		t.Errorf("created event has a prev_audience: %v", ev[0].PrevAudience)
	}
}

// Applying a snapshot discards older held ones of the scope, and only those.
func TestApplyDiscardsOlderHeldSnapshots(t *testing.T) {
	r := newRig(t)
	for id, age := range map[int]time.Duration{7: 3 * time.Hour, 8: 2 * time.Hour} {
		if _, err := r.j.db.Exec(`INSERT INTO snapshot (id, record_type, schema_version, scope, data_subject, complete,
			approximate, observed_at, received_at, writer, status, payload) VALUES ($1::uuid, 'assignment', 1, $2, 'e_child1',
			true, false, $3, $3, 'x', 'held', '{}')`, uid(id), scopeKey("e_child1", "c1"), T0.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-150*time.Minute), true, rec("k", "e_child1", `{"title": "a"}`)))
	if r.snapshotStatus(7) != "discarded" || r.snapshotStatus(8) != "held" {
		t.Errorf("statuses %s %s", r.snapshotStatus(7), r.snapshotStatus(8))
	}
}

// The snapshot row and events carry the writer and the approximate flag, and
// the recorder sees a duplicate as one.
func TestApplyStoresWriterAndApproximate(t *testing.T) {
	r := newRig(t)
	s := snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "a"}`))
	s.Approximate = true
	r.apply(s)
	ev := r.events("e_child1")
	if len(ev) != 1 || !ev[0].Approximate {
		t.Errorf("approximate %+v", ev)
	}
	var w string
	var appr bool
	if err := r.j.db.QueryRow(`SELECT writer, approximate FROM snapshot WHERE id = $1::uuid`, uid(1)).Scan(&w, &appr); err != nil || w != writer || !appr {
		t.Errorf("snapshot row %q %v %v", w, appr, err)
	}
	r.apply(s)
	if r.rec.snapshots["applied"] != 1 || r.rec.snapshots["duplicate"] != 1 {
		t.Errorf("snapshots %+v", r.rec.snapshots)
	}
}

// Dropping a text value keeps its old verdict in the event and clears it from
// the row.
func TestRemovedTextFieldVerdicts(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "W", "comment": "INJECT x"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "W"}`)))
	ev := r.events("e_child1")[1]
	var scan map[string]struct{ Old, New string }
	_ = json.Unmarshal(ev.Scan, &scan)
	if scan["comment"].Old != "quarantine" || scan["comment"].New != "" {
		t.Errorf("event scan %s", ev.Scan)
	}
	if c, _ := r.current("assignment", "e_child1", "k"); len(c.Scan) != 0 {
		t.Errorf("row scan %+v", c.Scan)
	}
}
