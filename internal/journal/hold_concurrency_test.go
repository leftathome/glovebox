package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// A discard that is in flight (uncommitted) when a confirm starts wins: the
// confirm waits on the snapshot row, then sees it discarded and applies
// nothing.
func TestConfirmHeldWaitsForAnInFlightDiscard(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	holder, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.Exec(`UPDATE snapshot SET status = 'discarded' WHERE id = $1::uuid AND status = 'held'`, uid(2)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.j.ConfirmHeld(bounded(t), uid(2), allowAll)
		done <- err
	}()
	time.Sleep(400 * time.Millisecond)
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrNotHeld) {
		t.Errorf("confirm after an in-flight discard: %v", err)
	}
	if st, ev := r.snapshotStatus(2), r.count("record_event"); st != "discarded" || ev != 5 {
		t.Errorf("status %s events %d", st, ev)
	}
}

// ConfirmHeld takes the subject lock like every other write of the subject.
func TestConfirmHeldTakesTheSubjectLock(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	holder, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if err := lockSubject(bounded(t), holder, "e_child1"); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	if _, err := r.j.ConfirmHeld(c, uid(2), allowAll); err == nil {
		t.Error("ConfirmHeld did not wait for the subject lock")
	}
	if r.snapshotStatus(2) != "held" {
		t.Error("a timed-out confirm changed the snapshot")
	}
}

// The write check gets the snapshot's record type and canonical scope.
func TestConfirmHeldPassesTypeAndScopeToTheWriteCheck(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	var gotT, gotS string
	chk := func(_ string, ty record.Type, sc record.Scope) *record.Rejection {
		gotT, gotS = ty.Name, sc.Canonical()
		return nil
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(2), chk); err != nil {
		t.Fatal(err)
	}
	if gotT != "assignment" || gotS != scopeKey("e_child1", "c1") {
		t.Errorf("type %q scope %q", gotT, gotS)
	}
}

// storeRawHeld stores a held row whose payload is the given snapshot.
func (r *rig) storeRawHeld(rowID int, typ, scope string, s journalapi.Snapshot) {
	r.t.Helper()
	payload, _ := json.Marshal(s)
	if _, err := r.j.db.Exec(`INSERT INTO snapshot (id, record_type, schema_version, scope, data_subject, complete,
		approximate, observed_at, received_at, writer, status, payload) VALUES ($1::uuid, $2, 1, $3, 'e_child1',
		true, false, $4, $4, $5, 'held', $6)`, uid(rowID), typ, scope, s.ObservedAt, writer, payload); err != nil {
		r.t.Fatal(err)
	}
}

// A held snapshot that no longer validates (its schema version is gone) is
// refused with 422 and stays held for the operator to discard.
func TestConfirmHeldRefusesAPayloadThatNoLongerValidates(t *testing.T) {
	r := newRig(t)
	s := snap(30, "e_child1", "c1", T0.Add(-time.Hour), true, keep(2)...)
	s.SchemaVersion = 99
	r.storeRawHeld(30, "assignment", scopeKey("e_child1", "c1"), s)
	out, err := r.j.ConfirmHeld(ctx, uid(30), allowAll)
	if err != nil || out.Rejection == nil || out.Rejection.Status != 422 || r.snapshotStatus(30) != "held" {
		t.Errorf("%+v %v %s", out, err, r.snapshotStatus(30))
	}
	if r.count("record_event") != 0 {
		t.Error("a refused confirm applied something")
	}
}

// A stored payload that does not describe its own row (another id, scope or
// type) is refused with 422 and nothing is applied.
func TestConfirmHeldRefusesAPayloadThatDoesNotMatchItsRow(t *testing.T) {
	cases := map[string]struct {
		rowType, rowScope string
		payload           journalapi.Snapshot
	}{
		"id":    {"assignment", scopeKey("e_child1", "c1"), snap(41, "e_child1", "c1", T0.Add(-time.Hour), true, keep(2)...)},
		"scope": {"assignment", scopeKey("e_child1", "c1"), snap(40, "e_child1", "c2", T0.Add(-time.Hour), true, keep(2)...)},
		"type":  {"lms_assignment", scopeKey("e_child1", "c1"), snap(40, "e_child1", "c1", T0.Add(-time.Hour), true, keep(2)...)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.storeRawHeld(40, c.rowType, c.rowScope, c.payload)
			out, err := r.j.ConfirmHeld(ctx, uid(40), allowAll)
			if err != nil || out.Rejection == nil || out.Rejection.Status != 422 || r.snapshotStatus(40) != "held" {
				t.Errorf("%+v %v %s rej=%+v", out, err, r.snapshotStatus(40), out.Rejection)
			}
			if r.count("record_event") != 0 || r.count("record_current") != 0 {
				t.Error("a refused confirm applied something")
			}
		})
	}
}

// The confirmed snapshot's row keeps the stats of what the confirm did, not
// the would-be stats stored when it was held.
func TestConfirmHeldStoresTheStatsOfWhatItApplied(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.storeRawHeld(30, "assignment", scopeKey("e_child1", "c1"), snap(30, "e_child1", "c1", T0.Add(-time.Hour), true, keep(1)...))
	if out, err := r.j.ConfirmHeld(ctx, uid(30), allowAll); err != nil || out.Stats.Removed != 4 {
		t.Fatalf("%+v %v", out, err)
	}
	out, found, err := r.j.Lookup(ctx, uid(30), writer, "assignment", scopeKey("e_child1", "c1"))
	if err != nil || !found || out.Status != journalapi.StatusDuplicate || out.Stats.Removed != 4 {
		t.Errorf("%+v %v %v", out, found, err)
	}
}
