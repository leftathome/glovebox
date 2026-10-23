package journal

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func allowAll(string, record.Type, record.Scope) *record.Rejection { return nil }

// five records in course c1 for e_child1, observed at t
func five(r *rig, n int, t time.Time) {
	var recs []journalapi.Record
	for i := 1; i <= 5; i++ {
		recs = append(recs, rec(fmt.Sprintf("c1:%d", i), "e_child1", `{"title": "A"}`))
	}
	r.apply(snap(n, "e_child1", "c1", t, true, recs...))
}

func keep(n int) []journalapi.Record {
	var recs []journalapi.Record
	for i := 1; i <= n; i++ {
		recs = append(recs, rec(fmt.Sprintf("c1:%d", i), "e_child1", `{"title": "A"}`))
	}
	return recs
}

func TestGuardHoldsAndAResendIsHeldAgain(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	s := snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...) // removes 2 of 5 = 0.4
	out := r.apply(s)
	if out.Status != journalapi.StatusHeld || out.Stats.Removed != 2 {
		t.Fatalf("%+v", out)
	}
	if r.count("record_event") != 5 {
		t.Error("a held snapshot applied something")
	}
	if again := r.apply(s); again.Status != journalapi.StatusHeld || again.Stats.Removed != 2 {
		t.Errorf("resend: %+v", again)
	}
	// a held snapshot counts once when stored; its resend is a duplicate
	if r.rec.snapshots["held"] != 1 || r.rec.snapshots["duplicate"] != 1 {
		t.Errorf("recorder %v", r.rec.snapshots)
	}
	var payload []byte
	if err := r.j.db.QueryRow(`SELECT payload FROM snapshot WHERE id = $1::uuid`, uid(2)).Scan(&payload); err != nil || len(payload) < 10 {
		t.Errorf("held payload %q %v", payload, err)
	}
}

func TestALaterSnapshotThatPassesDiscardsOlderHeldOnes(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	out := r.apply(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true, keep(5)...))
	if out.Status != journalapi.StatusApplied || r.snapshotStatus(2) != "discarded" {
		t.Errorf("%+v, held one is %s", out, r.snapshotStatus(2))
	}
}

func TestConfirmAppliesWithoutTheGuardAndDiscardsOlderHeld(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-4*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-3*time.Hour), true, keep(3)...))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(1)...))
	out, err := r.j.ConfirmHeld(ctx, uid(3), allowAll)
	if err != nil || out.Status != journalapi.StatusApplied || out.Stats.Removed != 4 {
		t.Fatalf("%+v %v", out, err)
	}
	if r.snapshotStatus(3) != "applied" || r.snapshotStatus(2) != "discarded" {
		t.Errorf("statuses %s %s", r.snapshotStatus(3), r.snapshotStatus(2))
	}
	for i := 2; i <= 5; i++ {
		if c, _ := r.current("assignment", "e_child1", fmt.Sprintf("c1:%d", i)); !c.Removed {
			t.Errorf("c1:%d not removed", i)
		}
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(2), allowAll); !errors.Is(err, ErrNotHeld) {
		t.Errorf("confirming a discarded snapshot: %v", err)
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(99), allowAll); !errors.Is(err, ErrNotFound) {
		t.Errorf("confirming nothing: %v", err)
	}
	if out, _, _ := r.j.Lookup(ctx, uid(3), writer, "assignment", scopeKey("e_child1", "c1")); out.Status != journalapi.StatusDuplicate || out.Stats.Removed != 4 {
		t.Errorf("a resend after confirmation: %+v", out)
	}
}

func TestConfirmingAnOvertakenHeldSnapshotMakesItLate(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-4*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	// Make the scope's applied snapshot #1 newer than held #2, as if a later
	// snapshot had been applied without discarding #2. Apply cannot produce
	// this; ConfirmHeld must still never move state backwards.
	if _, err := r.j.db.Exec(`UPDATE snapshot SET observed_at = $2 WHERE id = $1::uuid`, uid(1), T0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	out, err := r.j.ConfirmHeld(ctx, uid(2), allowAll)
	if err != nil || out.Status != journalapi.StatusLate || r.snapshotStatus(2) != "late" {
		t.Errorf("%+v %v %s", out, err, r.snapshotStatus(2))
	}
}

func TestDiscard(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	if err := r.j.DiscardHeld(ctx, uid(2)); err != nil {
		t.Fatal(err)
	}
	if r.snapshotStatus(2) != "discarded" || r.count("record_event") != 5 {
		t.Error("discard applied something")
	}
	if err := r.j.DiscardHeld(ctx, uid(2)); !errors.Is(err, ErrNotHeld) {
		t.Errorf("second discard: %v", err)
	}
	if err := r.j.DiscardHeld(ctx, uid(98)); !errors.Is(err, ErrNotFound) {
		t.Errorf("discard nothing: %v", err)
	}
}

func TestConfirmRechecksTheWritersPolicy(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	var asked string
	revoked := func(w string, _ record.Type, _ record.Scope) *record.Rejection {
		asked = w
		return &record.Rejection{Status: 403, Reason: journalapi.ReasonPolicy, Detail: "revoked"}
	}
	out, err := r.j.ConfirmHeld(ctx, uid(2), revoked)
	if err != nil || out.Rejection == nil || out.Rejection.Status != 403 || asked != writer || r.snapshotStatus(2) != "held" {
		t.Errorf("%+v %v asked=%q status=%s", out, err, asked, r.snapshotStatus(2))
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(2), nil); err == nil {
		t.Error("a confirm without a write check")
	}
}

func TestConfirmRechecksTheRoster(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	r.ros.Store(nil)
	out, err := r.j.ConfirmHeld(ctx, uid(2), allowAll)
	if err != nil || out.Rejection == nil || out.Rejection.Status != 409 || r.snapshotStatus(2) != "held" {
		t.Errorf("%+v %v", out, err)
	}
}

// insertHeld stores a held snapshot row directly (Apply cannot produce one
// that is older than an applied snapshot of the same scope).
func (r *rig) insertHeld(n int, typ, scope, subject string, observed time.Time) {
	r.t.Helper()
	if _, err := r.j.db.Exec(`INSERT INTO snapshot (id, record_type, schema_version, scope, data_subject, complete,
		approximate, observed_at, received_at, writer, status, payload) VALUES ($1::uuid, $2, 1, $3, $4,
		true, false, $5, $5, 'x', 'held', '{}')`, uid(n), typ, scope, subject, observed); err != nil {
		r.t.Fatal(err)
	}
}

// An applied snapshot discards only strictly older held snapshots of the same
// scope and record type: an equal observed_at, another scope, another type
// and a newer one all stay held.
func TestAppliedSnapshotDiscardsOnlyStrictlyOlderHeldOfSameScopeAndType(t *testing.T) {
	r := newRig(t)
	at := T0.Add(-time.Hour)
	sc := scopeKey("e_child1", "c1")
	r.insertHeld(10, "assignment", sc, "e_child1", at.Add(-time.Minute))
	r.insertHeld(11, "assignment", sc, "e_child1", at)
	r.insertHeld(12, "assignment", sc, "e_child1", at.Add(time.Minute))
	r.insertHeld(13, "assignment", scopeKey("e_child1", "c2"), "e_child1", at.Add(-time.Minute))
	r.insertHeld(14, "lms_assignment", sc, "e_child1", at.Add(-time.Minute))
	r.insertHeld(15, "assignment", sc, "e_child1", at.Add(-2*time.Minute))
	if _, err := r.j.db.Exec(`UPDATE snapshot SET status = 'late' WHERE id = $1::uuid`, uid(15)); err != nil {
		t.Fatal(err)
	}
	out := r.apply(snap(1, "e_child1", "c1", at, true, keep(2)...))
	if out.Status != journalapi.StatusApplied {
		t.Fatalf("%+v", out)
	}
	want := map[int]string{10: "discarded", 11: "held", 12: "held", 13: "held", 14: "held", 15: "late"}
	for n, w := range want {
		if got := r.snapshotStatus(n); got != w {
			t.Errorf("snapshot %d is %s, want %s", n, got, w)
		}
	}
}

// Confirming discards strictly older held snapshots of the same scope and
// type only, and never itself.
func TestConfirmDiscardsOnlyOlderHeldOfSameScopeAndType(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-5*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-3*time.Hour), true, keep(3)...))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(1)...))
	sc := scopeKey("e_child1", "c1")
	r.insertHeld(20, "assignment", scopeKey("e_child1", "c2"), "e_child1", T0.Add(-4*time.Hour))
	r.insertHeld(21, "lms_assignment", sc, "e_child1", T0.Add(-4*time.Hour))
	r.insertHeld(22, "assignment", sc, "e_child1", T0.Add(-time.Hour))
	if _, err := r.j.ConfirmHeld(ctx, uid(3), allowAll); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{2: "discarded", 3: "applied", 20: "held", 21: "held", 22: "held"}
	for n, w := range want {
		if got := r.snapshotStatus(n); got != w {
			t.Errorf("snapshot %d is %s, want %s", n, got, w)
		}
	}
}

// Confirmation writes events, reports them after the commit, and counts the
// snapshot as applied; a refused confirm reports nothing.
func TestConfirmReportsAfterCommitOnly(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))
	before := len(r.rec.events)
	snapsBefore := map[string]int{}
	for k, v := range r.rec.snapshots {
		snapsBefore[k] = v
	}
	r.ros.Store(nil)
	if _, err := r.j.ConfirmHeld(ctx, uid(2), allowAll); err != nil {
		t.Fatal(err)
	}
	if len(r.rec.events) != before || !reflect.DeepEqual(r.rec.snapshots, snapsBefore) {
		t.Errorf("a refused confirm reported: %v %v", r.rec.events, r.rec.snapshots)
	}
	r.ros.Store(journaltest.Roster(t))
	if _, err := r.j.ConfirmHeld(ctx, uid(2), allowAll); err != nil {
		t.Fatal(err)
	}
	if r.rec.snapshots["applied"] != 2 || r.rec.snapshots["held"] != 1 {
		t.Errorf("recorder %v", r.rec.snapshots)
	}
	n := 0
	for k, v := range r.rec.events {
		if strings.HasPrefix(k, "removed/") {
			n += v
		}
	}
	if n != 2 {
		t.Errorf("removed events reported %d: %v", n, r.rec.events)
	}
	ev := r.events("e_child1")
	last := ev[len(ev)-1]
	if last.Kind != "removed" || !last.SnapshotID.Valid || last.SnapshotID.String != uid(2) {
		t.Errorf("last event %+v", last)
	}
}
