package journal

import (
	"context"
	"testing"
	"time"
)

// A record that was removed (its last fields live on in the removed event)
// goes too: every row of the subject, in every table, is gone.
func TestEraseRemovesRemovedRecordsToo(t *testing.T) {
	r := newRig(t)
	t1, t2 := T0.Add(-3*time.Hour), T0.Add(-2*time.Hour)
	r.apply(snap(1, "e_child1", "c1", t1, true, rec("c1:1", "e_child1", `{"title": "A"}`), rec("c1:2", "e_child1", `{"title": "B"}`)))
	if out := r.apply(snap(2, "e_child1", "c1", t2, true, rec("c1:1", "e_child1", `{"title": "A"}`))); out.Stats.Removed != 1 {
		t.Fatalf("setup: %+v", out)
	}
	if c, ok := r.current("assignment", "e_child1", "c1:2"); !ok || !c.Removed {
		t.Fatal("setup: no removed row")
	}
	resp, err := r.j.Erase(ctx, "a", "e_child1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Current != 2 || resp.Events != 3 || resp.Snapshots != 2 {
		t.Errorf("%+v", resp)
	}
	if got := r.dump("e_child1"); got != "" {
		t.Errorf("rows survive:\n%s", got)
	}
}

// The refusal window never moves backward: a second erase after the clock
// stepped back does not weaken the guard.
func TestEraseClockStepBackKeepsRefusing(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.clock.Set(T0)
	if _, err := r.j.Erase(ctx, "a", "e_child1"); err != nil {
		t.Fatal(err)
	}
	r.clock.Set(T0.Add(-2 * time.Hour))
	if _, err := r.j.Erase(ctx, "a", "e_child1"); err != nil {
		t.Fatal(err)
	}
	r.clock.Set(T0.Add(time.Hour))
	out := r.apply(snap(2, "e_child1", "c1", T0.Add(-30*time.Minute), true, keep(1)...))
	if out.Rejection == nil || out.Rejection.Status != 410 {
		t.Errorf("clock step back weakened the guard: %+v", out)
	}
}

// Erase locks the subject's rows (in rescan's order) before deleting: a row
// lock held by another transaction, as a rescan batch holds one without the
// subject lock, makes it wait, and a timed-out erase changes nothing.
func TestEraseWaitsForRowLocks(t *testing.T) {
	for name, q := range map[string]string{
		"record_current": `SELECT 1 FROM record_current WHERE data_subject = 'e_child1' ORDER BY key LIMIT 1 FOR UPDATE`,
		"record_event":   `SELECT 1 FROM record_event WHERE data_subject = 'e_child1' ORDER BY seq LIMIT 1 FOR UPDATE`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			five(r, 1, T0.Add(-3*time.Hour))
			holder, err := r.j.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback() }()
			rows, err := holder.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
			}
			_ = rows.Close()
			c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
			defer cancel()
			if _, err := r.j.Erase(c, "a", "e_child1"); err == nil {
				t.Error("Erase did not wait for the row lock")
			}
			_ = holder.Rollback()
			if r.count("record_event") != 5 || r.count("record_current") != 5 || r.count("snapshot") != 1 || r.count("erasure") != 0 {
				t.Error("a timed-out erase changed something")
			}
		})
	}
}
