package journal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func TestEraseRemovesEverythingAndRefusesReplays(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-3*time.Hour), true, rec("c1:1", "e_child2", `{"title": "B"}`)))
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	maxSeq := r.events("e_child2")[0].Seq
	r.clock.Set(T0.Add(-time.Hour))
	resp, err := r.eraseOff(ctx, "openclaw/glovebox-journal-admin", "e_child1")
	if err != nil {
		t.Fatal(err)
	}
	// 1 snapshot, 1 current row and 1 event deleted (the record); the
	// parent's note about the child is kept, pseudonymised (decision
	// 2026-10-23; erase_notes_test.go).
	if resp.DataSubject != "e_child1" || resp.Snapshots != 1 || resp.Current != 1 || resp.Events != 1 ||
		resp.NotesPseudonymised != 1 || resp.NotesDeletedAsAuthor != 0 || !resp.ErasedAt.Equal(T0.Add(-time.Hour)) {
		t.Errorf("%+v", resp)
	}
	for _, table := range []string{"snapshot", "record_current", "record_event"} {
		var n int
		if err := r.j.db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE data_subject = 'e_child1'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows of the erased subject", table, n)
		}
	}
	if len(r.events("e_child2")) != 1 {
		t.Error("another subject's events were erased")
	}
	var actor, counts string
	var at time.Time
	if err := r.j.db.QueryRow(`SELECT actor, rows::text, erased_at FROM erasure WHERE data_subject = 'e_child1'`).Scan(&actor, &counts, &at); err != nil {
		t.Fatal("no erasure row:", err)
	}
	if actor != "openclaw/glovebox-journal-admin" || !at.Equal(T0.Add(-time.Hour)) ||
		counts != `{"snapshot": 1, "record_event": 1, "record_current": 1, "notes_pseudonymised": 1, "notes_deleted_as_author": 0}` {
		t.Errorf("erasure row: %q %v %s", actor, at, counts)
	}
	// The spool replays the old snapshot: refused, and it stays gone.
	out := r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	if out.Rejection == nil || out.Rejection.Reason != journalapi.ReasonErasedSubject {
		t.Errorf("replay: %+v", out)
	}
	if r.snapshotStatus(1) != "absent" {
		t.Error("the replayed snapshot was stored")
	}
	// New data after the erase is accepted and gets new sequence numbers.
	r.apply(snap(3, "e_child1", "c1", T0, true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	if ev := r.events("e_child1"); len(ev) != 1 || ev[0].Seq <= maxSeq {
		t.Errorf("sequence numbers reused: %+v", ev)
	}
	if _, err := r.eraseOff(ctx, "admin", ""); !errors.Is(err, ErrNoSubject) {
		t.Errorf("erased the empty subject: %v", err)
	}
}

// Every table that carries data_subject is known to Erase: a table added by
// a later migration must make this test fail until Erase handles it.
func TestEraseKnowsEveryTableWithASubject(t *testing.T) {
	r := newRig(t)
	rows, err := r.j.db.Query(`SELECT DISTINCT table_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'data_subject' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	// erasure is the record of erasures: never erased itself, no field data.
	if strings.Join(got, ",") != "erasure,record_current,record_event,snapshot" {
		t.Errorf("tables with data_subject: %v", got)
	}
}

// dump is every row of one subject in every table, as text, ordered.
func (r *rig) dump(subject string) string {
	r.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT to_jsonb(s)::text FROM snapshot s WHERE data_subject = $1 ORDER BY id`,
		`SELECT to_jsonb(c)::text FROM record_current c WHERE data_subject = $1 ORDER BY record_type, key`,
		`SELECT to_jsonb(e)::text FROM record_event e WHERE data_subject = $1 ORDER BY seq`,
	} {
		rows, err := r.j.db.Query(q, subject)
		if err != nil {
			r.t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			b.WriteString(s + "\n")
		}
		rows.Close()
	}
	return b.String()
}

// Snapshots of every status (applied, held, late, discarded) and their
// payloads go; nothing of the subject survives anywhere; the other subject's
// rows are byte-for-byte what they were; household rows (no subject) stay.
func TestEraseClearsEveryStatusAndTouchesNoOneElse(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...))    // held
	r.apply(snap(3, "e_child1", "c1", T0.Add(-90*time.Minute), true, keep(3)...)) // held, then discarded
	r.apply(snap(4, "e_child1", "c1", T0.Add(-5*time.Hour), true, keep(5)...))    // late
	r.apply(snap(5, "e_child1", "c1", T0.Add(-80*time.Minute), true, keep(3)...)) // held
	if err := r.j.DiscardHeld(ctx, uid(3)); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]string{1: "applied", 2: "held", 3: "discarded", 4: "late", 5: "held"} {
		if got := r.snapshotStatus(n); got != want {
			t.Fatalf("setup: snapshot %d is %s, want %s", n, got, want)
		}
	}
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	// e_child2: an applied and a held snapshot, with different payload text.
	var recs2 []journalapi.Record
	for i := 1; i <= 5; i++ {
		recs2 = append(recs2, rec("c1:"+string(rune('0'+i)), "e_child2", `{"title": "B"}`))
	}
	r.apply(snap(11, "e_child2", "c1", T0.Add(-3*time.Hour), true, recs2...))
	r.apply(snap(12, "e_child2", "c1", T0.Add(-2*time.Hour), true, recs2[:3]...))
	if r.snapshotStatus(12) != "held" {
		t.Fatal("setup: e_child2 snapshot not held")
	}
	// A household row: no subject. Copied from a child1 row so it is real.
	for _, q := range []string{
		`INSERT INTO snapshot SELECT '00000000-0000-4000-8000-0000000000aa', record_type, schema_version, scope, '', complete,
			approximate, observed_at, received_at, writer, 'applied', payload, stats FROM snapshot WHERE id = '` + uid(1) + `'`,
		`INSERT INTO record_current SELECT record_type, '', 'hh:1', scope, audience, fields, removed, first_seen_at, last_seen_at,
			last_confirmed_snapshot, last_confirmed_at, content_hash, last_event_seq, scan FROM record_current
			WHERE data_subject = 'e_child1' AND key = 'c1:1'`,
		`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience, fields, scan,
			observed_at, approximate, actor, importance) SELECT record_type, '', 'hh:1', kind, schema_version, scope, audience,
			fields, scan, observed_at, approximate, actor, importance FROM record_event WHERE data_subject = 'e_child1' AND key = 'c1:1' LIMIT 1`,
	} {
		if _, err := r.j.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	other, household := r.dump("e_child2"), r.dump("")
	if household == "" || other == "" {
		t.Fatal("setup: nothing to compare")
	}
	if _, err := r.eraseOff(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if got := r.dump("e_child1"); got != "" {
		t.Errorf("rows of the erased subject remain:\n%s", got)
	}
	if got := r.dump("e_child2"); got != other {
		t.Errorf("another subject changed:\n%s\nwas\n%s", got, other)
	}
	// The parent's note about the child is kept under its own pseudonymous
	// subject (erase_frozen_test.go), not in the household.
	if got := r.dump(""); got != household {
		t.Errorf("household rows changed:\n%s\nwas\n%s", got, household)
	}
	if n := len(r.kept("attendance_note")); n != 1 {
		t.Errorf("%d kept notes", n)
	}
	// The subject's name survives nowhere, payloads included. (The household
	// row above was cloned from a child1 row, so its scope still names child1.)
	for _, table := range []string{"snapshot", "record_current", "record_event"} {
		var n int
		_ = r.j.db.QueryRow(`SELECT count(*) FROM ` + table + ` t WHERE data_subject <> '' AND to_jsonb(t)::text LIKE '%e_child1%'`).Scan(&n)
		if n != 0 {
			t.Errorf("%s: %d rows still mention the subject", table, n)
		}
	}
	// Remaining subject still folds: each current row's last_event_seq is
	// its newest event.
	var bad int
	_ = r.j.db.QueryRow(`SELECT count(*) FROM record_current c WHERE data_subject = 'e_child2' AND last_event_seq <>
		(SELECT max(seq) FROM record_event e WHERE e.record_type = c.record_type AND e.data_subject = c.data_subject AND e.key = c.key)`).Scan(&bad)
	if bad != 0 {
		t.Errorf("%d rows of the other subject lost their events", bad)
	}
}

// A second erase is allowed: it counts nothing, adds its own erasure row and
// keeps the refusal window at the later of the two erasures.
func TestEraseTwiceAndOfAnUnknownSubject(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	first, err := r.eraseOff(ctx, "a", "e_child1")
	if err != nil || first.Events != 5 || first.Current != 5 || first.Snapshots != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	r.clock.Set(T0.Add(time.Hour))
	second, err := r.eraseOff(ctx, "b", "e_child1")
	if err != nil || second.Events != 0 || second.Current != 0 || second.Snapshots != 0 || !second.ErasedAt.Equal(T0.Add(time.Hour)) {
		t.Fatalf("%+v %v", second, err)
	}
	if n := r.count("erasure"); n != 2 {
		t.Errorf("erasure rows: %d", n)
	}
	// Observed before the second erase: refused, though after the first.
	out := r.apply(snap(2, "e_child1", "c1", T0.Add(30*time.Minute), true, keep(1)...))
	if out.Rejection == nil || out.Rejection.Status != 410 {
		t.Errorf("%+v", out)
	}
	// A subject with no data still gets its erasure row, so a replay of
	// anything not yet received is refused.
	if _, err := r.eraseOff(ctx, "a", "e_child2"); err != nil {
		t.Fatal(err)
	}
	if n := r.count("erasure"); n != 3 {
		t.Errorf("erasure rows: %d", n)
	}
}

// Erase interacts with the other writers as specified.
func TestEraseAndTheOtherWriters(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(3)...)) // held
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	if _, err := r.eraseOff(ctx, "a", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.ConfirmHeld(ctx, uid(2), allowAll); !errors.Is(err, ErrNotFound) {
		t.Errorf("confirm after erase: %v", err)
	}
	if err := r.j.DiscardHeld(ctx, uid(2)); !errors.Is(err, ErrNotFound) {
		t.Errorf("discard after erase: %v", err)
	}
	for _, reader := range []string{"e_parent1", "e_child1"} {
		for _, k := range r.stateKeys(reader, StateQuery{}) {
			if strings.HasPrefix(k, "e_child1/") {
				t.Errorf("%s still reads %s", reader, k)
			}
		}
		// The parent, its author, reads the kept note only under its
		// pseudonymous subject; the erased child, put back, reads nothing.
		evs, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if reader == "e_child1" || !keptSubject.MatchString(e.DataSubject) {
				t.Errorf("%s still reads %+v", reader, e)
			}
		}
		if want := map[string]int{"e_parent1": 1, "e_child1": 0}[reader]; len(evs) != want {
			t.Errorf("%s reads %d events, want %d", reader, len(evs), want)
		}
	}
	// The clock is at T0 == erased_at: the note is not refused (strict >),
	// a snapshot inside the skew window is.
	if _, rej := r.note("e_parent1", "e_child1", "2026-10-15", `{"what_happened": "y"}`); rej != nil {
		t.Errorf("note at the erase instant: %v", rej)
	}
	out := r.apply(snap(3, "e_child1", "c1", T0.Add(5*time.Minute), true, keep(1)...))
	if out.Rejection == nil || out.Rejection.Status != 410 {
		t.Errorf("inside the skew window: %+v", out)
	}
	// A note when the clock stepped back before the erase is refused.
	r.clock.Set(T0.Add(-time.Minute))
	if _, rej := r.note("e_parent1", "e_child1", "2026-10-16", `{"what_happened": "z"}`); rej == nil || rej.Status != 410 {
		t.Errorf("note before the erase: %v", rej)
	}
}

// Erase takes the subject lock: it waits for a writer of the subject, and a
// timed-out erase changes nothing. Another subject's lock does not block it.
func TestEraseTakesTheSubjectLock(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
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
	if _, err := r.eraseOff(c, "a", "e_child1"); err == nil {
		t.Error("Erase did not wait for the subject lock")
	}
	if r.count("record_event") != 5 || r.count("erasure") != 0 {
		t.Error("a timed-out erase changed something")
	}
	_ = holder.Rollback()
	holder2, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder2.Rollback() }()
	if err := lockSubject(bounded(t), holder2, "e_child2"); err != nil {
		t.Fatal(err)
	}
	c2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	if _, err := r.eraseOff(c2, "a", "e_child1"); err != nil {
		t.Errorf("Erase blocked on another subject's lock: %v", err)
	}
}

// Erase is atomic: a failure after the deletes leaves everything in place.
func TestEraseRollsBackAsOne(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	if _, err := r.j.db.Exec(`ALTER TABLE erasure ADD CONSTRAINT erasure_test_fail CHECK (actor <> 'boom')`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eraseOff(ctx, "boom", "e_child1"); err == nil {
		t.Fatal("expected the insert to fail")
	}
	if r.count("record_event") != 5 || r.count("record_current") != 5 || r.count("snapshot") != 1 || r.count("erasure") != 0 {
		t.Error("a failed erase left a partial deletion")
	}
}
