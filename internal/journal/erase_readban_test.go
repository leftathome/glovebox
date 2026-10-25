package journal

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// The read ban (review 2026-10-24, I-A): an erased member, still on the
// roster, reads nothing at all, not the household's notes, a sibling's rows
// or public rows, which they read before; everyone else reads as before.
// (Review probe TestZZ2ProbeBannedReader.)
func TestErasedMemberCannotRead(t *testing.T) {
	r := newNoteRig(t)
	x := rec("pub", "e_child2", `{"title": "P"}`)
	x.Audience = []string{"public"}
	y := rec("sib", "e_child2", `{"title": "S"}`)
	y.Audience = []string{"siblings"}
	r.apply(snap(1, "e_child2", "c1", T0.Add(-time.Hour), true, x, y))
	r.mustNote("family_note", "e_parent1", "", "2026-10-14", `{"text": "household"}`)
	before := r.readsOf("e_child1")
	for _, want := range []string{"state:e_child2/pub", "state:e_child2/sib", "state:/2026-10-14:e_parent1"} {
		if !contains(before, want) {
			t.Fatalf("setup: e_child1 does not read %s: %v", want, before)
		}
	}
	other := r.readsOf("e_child2")
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if !r.ros.Load().Has("e_child1") {
		t.Fatal("setup: e_child1 left the roster")
	}
	if !r.readRefused("e_child1") {
		t.Errorf("an erased member reads %v", r.readsOf("e_child1"))
	}
	_, err := r.j.State(ctx, StateQuery{Reader: "e_child1"})
	if !errors.Is(err, ErrUnknownReader) {
		t.Errorf("state: %v", err)
	}
	if got := r.readsOf("e_child2"); fmt.Sprint(got) != fmt.Sprint(other) {
		t.Errorf("another member's reads changed:\n%v\nwas\n%v", got, other)
	}
	// And it fails closed: a broken erasure table refuses every read.
	r.exec(t, `ALTER TABLE erasure RENAME TO erasure_gone`)
	if _, err := r.j.State(ctx, StateQuery{Reader: "e_child2"}); err == nil {
		t.Error("a read went through without the ban check")
	}
	r.exec(t, `ALTER TABLE erasure_gone RENAME TO erasure`)
}

// History fails closed for frozen readers (review 2026-10-24, I-B): a
// guardian frozen from the note's audience [guardians, subject] sees only
// the events of that audience, with old values across a change from another
// audience withheld, exactly as before the erase; the author sees the whole
// history. (Review probe TestZZ2ProbeHistoryWiden.)
func TestKeptHistoryFollowsTheFrozenAudience(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "OLDSECRET"}`)
	r.clock.Set(T0.Add(time.Hour))
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "new"}`)
	r.exec(t, `ALTER TABLE record_event DISABLE TRIGGER USER`)
	r.exec(t, `UPDATE record_event SET audience = '{subject}' WHERE kind = 'created'`)
	r.exec(t, `UPDATE record_event SET prev_audience = '{subject}' WHERE kind = 'updated'`)
	r.exec(t, `ALTER TABLE record_event ENABLE TRIGGER USER`)
	show := func(m string) string {
		t.Helper()
		evs, err := r.j.Events(ctx, EventsQuery{Reader: m, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var b []string
		for _, e := range evs {
			b = append(b, e.Kind+":"+string(e.Fields["what_happened"])+"/old="+string(e.Changed["what_happened"].Old)+
				"/prev_scope="+e.PrevScope)
		}
		return strings.Join(b, " ")
	}
	guardianBefore := show("e_parent2")
	if strings.Contains(guardianBefore, "OLDSECRET") || !strings.HasPrefix(guardianBefore, "updated:") {
		t.Fatalf("setup: the guardian reads %s", guardianBefore)
	}
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if got := show("e_parent2"); got != guardianBefore {
		t.Errorf("the guardian reads %q after the erase, %q before", got, guardianBefore)
	}
	author := show("e_parent1")
	if !strings.Contains(author, `created:"OLDSECRET"`) || !strings.Contains(author, `updated:/old="OLDSECRET"`) {
		t.Errorf("the author reads %q", author)
	}
	// State: the current row has the frozen audience, so both read it.
	for _, m := range []string{"e_parent1", "e_parent2"} {
		if n := len(keptReads(r.readsOf(m))); n == 0 {
			t.Errorf("%s does not read the kept note", m)
		}
	}
	r.noMismatch()
}

// Migration 0004 backfills kept notes made under 0003: frozen from their
// current audience, the author marked from the key.
func TestMigration0004BackfillsKeptNotes(t *testing.T) {
	db := journaltest.NewDB(t)
	if err := MigrateTo(ctx, db, 3); err != nil {
		t.Fatal(err)
	}
	p := "erased:" + strings.Repeat("ab", 16) + ":1"
	for _, q := range []string{
		`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields, first_seen_at, last_seen_at,
			last_confirmed_at, content_hash, last_event_seq) VALUES ('attendance_note', '` + p + `', '2026-10-14:e_parent1',
			'source=household/record_type=attendance_note', '{guardians,subject}', '{}', now(), now(), now(), '\x00', 1)`,
		`INSERT INTO erasure_reader VALUES ('` + p + `', 'e_parent1'), ('` + p + `', 'e_parent2')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var aud, authors string
	if err := db.QueryRow(`SELECT array_to_string(audience, ','), (SELECT string_agg(member || '=' || author, ',' ORDER BY member)
		FROM erasure_reader) FROM erasure_kept WHERE pseudonym = $1`, p).Scan(&aud, &authors); err != nil {
		t.Fatal(err)
	}
	if aud != "guardians,subject" || authors != "e_parent1=true,e_parent2=false" {
		t.Errorf("backfill %q %q", aud, authors)
	}
}

// The ban comes before the roster check: an erased author who has left the
// roster gets 410 for a note, not the non-member 403; a member who was
// never erased and is not on the roster still gets 403.
func TestErasedAuthorOffTheRosterIs410(t *testing.T) {
	r := newNoteRig(t)
	if _, err := r.j.Erase(ctx, "admin", "e_parent2"); err != nil {
		t.Fatal(err)
	}
	r.setRoster(rosterWithout(t, journaltest.HouseholdV1, "e_parent2"))
	if _, rej := r.note("e_parent2", "e_child1", "2026-10-14", `{"what_happened": "x"}`); rej == nil || rej.Status != 410 {
		t.Errorf("erased author off the roster: %v", rej)
	}
	if _, rej := r.note("e_parent9", "e_child1", "2026-10-14", `{"what_happened": "x"}`); rej == nil || rej.Status != 403 {
		t.Errorf("never erased, not on the roster: %v", rej)
	}
}
