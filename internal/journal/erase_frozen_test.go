package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/access"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// Kept notes and their frozen readers (decisions 2026-10-23, spec 4.13).

var keptSubject = regexp.MustCompile(`^(erased:[0-9a-f]{32}):[1-9][0-9]*$`)

// kept is the kept notes of a type: key -> pseudonymous data_subject.
func (r *rig) kept(typ string) map[string]string {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT key, data_subject FROM record_current WHERE record_type = $1 AND data_subject LIKE 'erased:%'`, typ)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, s string
		_ = rows.Scan(&k, &s)
		if !keptSubject.MatchString(s) {
			r.t.Fatalf("kept subject %q has the wrong shape", s)
		}
		out[k] = s
	}
	return out
}

// readersOf is a kept note's frozen readers, sorted, joined by ",".
func (r *rig) readersOf(subject string) string {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT member FROM erasure_reader WHERE pseudonym = $1 ORDER BY 1`, subject)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		out = append(out, m)
	}
	return strings.Join(out, ",")
}

// readsOf is every key a member reads in State, Events and Counts, as
// "state:<subject>/<key>", "event:<subject>/<key>" and "count:<subject>".
func (r *rig) readsOf(m string) []string {
	r.t.Helper()
	if r.readRefused(m) {
		return []string{"refused"}
	}
	var out []string
	for _, k := range r.stateKeys(m, StateQuery{IncludeRemoved: true}) {
		out = append(out, "state:"+k)
	}
	for _, k := range r.eventKeys(m) {
		out = append(out, "event:"+k)
	}
	cs, err := r.j.Counts(ctx, m, nil, T0.Add(-1000*time.Hour))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, c := range cs {
		out = append(out, fmt.Sprintf("count:%s/%s/%d", c.DataSubject, c.RecordType, c.Live))
	}
	sort.Strings(out)
	return out
}

// readRefused reports whether State, Events and Counts all refuse the
// reader with ErrUnknownReader; it fails the test when they disagree.
func (r *rig) readRefused(m string) bool {
	r.t.Helper()
	_, e1 := r.j.State(ctx, StateQuery{Reader: m})
	_, e2 := r.j.Events(ctx, EventsQuery{Reader: m, Limit: 10})
	_, e3 := r.j.Counts(ctx, m, nil, T0.Add(-1000*time.Hour))
	n := 0
	for _, err := range []error{e1, e2, e3} {
		if errors.Is(err, ErrUnknownReader) {
			n++
		} else if err != nil {
			r.t.Fatal(err)
		}
	}
	if n != 0 && n != 3 {
		r.t.Fatalf("%s: reads disagree: %v %v %v", m, e1, e2, e3)
	}
	return n == 3
}

// keptReads is the part of readsOf about kept notes.
func keptReads(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		if strings.Contains(x, access.ErasedPrefix) {
			out = append(out, x)
		}
	}
	return out
}

// Erase needs a member id and a roster; the household, a pseudonym and a
// malformed id are refused, and nothing changes.
func TestEraseValidatesItsSubject(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	before := r.rowsWhere(`true`)
	for _, s := range []string{"", "erased:" + strings.Repeat("0", 32) + ":1", "a b"} {
		if _, err := r.j.Erase(ctx, "admin", s); !errors.Is(err, ErrNoSubject) {
			t.Errorf("erase %q: %v", s, err)
		}
	}
	r.ros.Store(nil)
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); !errors.Is(err, ErrNoRoster) {
		t.Errorf("erase with no roster: %v", err)
	}
	if got := r.rowsWhere(`true`); got != before || r.count("erasure") != 0 {
		t.Error("a refused erase changed something")
	}
}

// Decision A (2026-10-24): a member still on the roster is erased; the
// roster snapshot, which still has them, makes their guardians frozen
// readers of the notes kept about them, and they themselves read none of
// it although still on the roster.
func TestEraseWhileOnTheRoster(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	s := r.kept("attendance_note")["2026-10-14:e_parent1"]
	if r.readersOf(s) != "e_parent1,e_parent2" {
		t.Errorf("readers %q", r.readersOf(s))
	}
	for m, n := range map[string]int{"e_parent1": 3, "e_parent2": 3, "e_child1": 0, "e_child2": 0, "e_guest1": 0} {
		if g := keptReads(r.readsOf(m)); len(g) != n {
			t.Errorf("%s reads %v", m, g)
		}
	}
}

// The frozen readers are computed by the read rule over the roster
// snapshot, minus the erased member and anyone erased before, plus the
// author if on the roster and not erased.
func TestFrozenReadersFollowTheReadRule(t *testing.T) {
	ros := journaltest.Roster(t)
	none := map[string]bool{}
	for _, c := range []struct {
		n      keptNote
		erased map[string]bool
		want   string
	}{
		{keptNote{"attendance_note", "2026-10-14:e_parent1", []string{"guardians", "subject"}}, none, "e_parent1,e_parent2"},
		{keptNote{"kid_note", "2026-10-14:e_child2", []string{"household"}}, none, "e_child2,e_parent1,e_parent2"},
		// the author always reads, guardian or not
		{keptNote{"attendance_note", "2026-10-14:e_child2", []string{"guardians"}}, none, "e_child2,e_parent1,e_parent2"},
		// an author off the roster does not
		{keptNote{"attendance_note", "2026-10-14:e_gone", []string{"guardians"}}, none, "e_parent1,e_parent2"},
		// the subject never does, by audience or as author
		{keptNote{"attendance_note", "2026-10-14:e_child1", []string{"subject"}}, none, ""},
		{keptNote{"attendance_note", "2026-10-14:e_parent1", []string{"opaque_token"}}, none, "e_parent1"},
		// public reaches every roster member, and only them, minus the
		// subject and anyone erased
		{keptNote{"attendance_note", "2026-10-14:e_parent1", []string{"public"}}, none, "e_child2,e_guest1,e_parent1,e_parent2"},
		{keptNote{"attendance_note", "2026-10-14:e_parent1", []string{"public"}}, map[string]bool{"e_guest1": true}, "e_child2,e_parent1,e_parent2"},
		// a member erased earlier reads nothing, as reader or as author
		{keptNote{"attendance_note", "2026-10-14:e_parent1", []string{"guardians"}}, map[string]bool{"e_parent1": true}, "e_parent2"},
		{keptNote{"kid_note", "2026-10-14:e_child2", []string{"household"}}, map[string]bool{"e_child2": true}, "e_parent1,e_parent2"},
	} {
		if got := strings.Join(frozenReaders(ros, "e_child1", c.erased, c.n), ","); got != c.want {
			t.Errorf("%v %v: readers %q, want %q", c.n, c.erased, got, c.want)
		}
	}
	if authorOf("2026-10-14:e_ann") != "e_ann" || authorOf("x") != "" {
		t.Error("authorOf")
	}
}

// End to end, the member still on the roster. Each kept note has its own
// subject under one pseudonym per erasure, its audience, key and text as
// written, and only its frozen readers read it, in State, Events and
// Counts: the guardians and the author for a note with a guardians part,
// the household members for a household note. The erased member, a
// guardian added to the roster later and a guest read none of it.
func TestEraseKeepsNotesForTheirFrozenReaders(t *testing.T) {
	r := newNoteRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-3*time.Hour), true, rec("c1:1", "e_child2", `{"title": "B"}`)))
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "we went to the store"}`)
	r.clock.Set(T0.Add(time.Hour))
	const text = `we went to the store - he was such a grump`
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "`+text+`", "heard_from": "guardian"}`)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-15", `{"what_happened": "p2 on c1"}`)
	r.mustNote("attendance_note", "e_child1", "e_child1", "2026-10-14", `{"what_happened": "my own note"}`)
	r.mustNote("kid_note", "e_child2", "e_child1", "2026-10-14", `{"text": "no guardians part"}`)
	r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "p1 on c2"}`)
	seqs := map[string]string{}
	for _, e := range r.events("e_child1") {
		seqs[e.Type+"/"+e.Key] += fmt.Sprint(e.Seq, ",")
	}
	child2 := r.dump("e_child2")
	r.clock.Set(T0.Add(2 * time.Hour))
	resp, err := r.j.Erase(ctx, "admin", "e_child1")
	if err != nil {
		t.Fatal(err)
	}
	// Deleted: the snapshot, 5 school records with 5 events, the subject's
	// own note. Kept: three notes.
	if resp.NotesPseudonymised != 3 || resp.NotesDeletedAsAuthor != 1 || resp.Snapshots != 1 || resp.Current != 6 || resp.Events != 6 {
		t.Errorf("%+v", resp)
	}
	if got := r.dump("e_child1"); got != "" {
		t.Errorf("rows of the erased subject remain:\n%s", got)
	}
	if got := r.dump("e_child2"); got != child2 {
		t.Errorf("another subject changed:\n%s\nwas\n%s", got, child2)
	}
	att, kid := r.kept("attendance_note"), r.kept("kid_note")
	p1, p2, k := att["2026-10-14:e_parent1"], att["2026-10-15:e_parent2"], kid["2026-10-14:e_child2"]
	if len(att) != 2 || len(kid) != 1 || p1 == "" || p2 == "" || k == "" || p1 == p2 || p1 == k {
		t.Fatalf("kept %v %v", att, kid)
	}
	if keptSubject.FindStringSubmatch(p1)[1] != keptSubject.FindStringSubmatch(k)[1] {
		t.Error("one erasure, two pseudonyms")
	}
	if r.readersOf(p1) != "e_parent1,e_parent2" || r.readersOf(p2) != "e_parent1,e_parent2" || r.readersOf(k) != "e_child2,e_parent1,e_parent2" {
		t.Errorf("readers %q %q %q", r.readersOf(p1), r.readersOf(p2), r.readersOf(k))
	}
	c, _ := r.current("attendance_note", p1, "2026-10-14:e_parent1")
	if string(c.Fields["what_happened"]) != `"`+text+`"` || string(c.Fields["heard_from"]) != `"guardian"` ||
		c.Scope != "source=household/record_type=attendance_note" || strings.Join(c.Audience, ",") != "guardians,subject" {
		t.Errorf("kept note %+v", c)
	}
	if c, _ := r.current("kid_note", k, "2026-10-14:e_child2"); strings.Join(c.Audience, ",") != "household" {
		t.Errorf("kid_note audience %v", c.Audience)
	}
	// The same events, rewritten in place.
	got := map[string]string{}
	for _, s := range []string{p1, p2, k} {
		for _, e := range r.events(s) {
			got[e.Type+"/"+e.Key] += fmt.Sprint(e.Seq, ",")
			if e.Scope != "source=household/record_type="+e.Type || e.GroupKey != "" {
				t.Errorf("kept event %+v", e)
			}
		}
	}
	for _, key := range []string{"attendance_note/2026-10-14:e_parent1", "attendance_note/2026-10-15:e_parent2", "kid_note/2026-10-14:e_child2"} {
		if got[key] != seqs[key] || got[key] == "" {
			t.Errorf("%s: seqs %q, were %q", key, got[key], seqs[key])
		}
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
	all := func(s, typ, key string, events int) []string {
		out := []string{"count:" + s + "/" + typ + "/1", "state:" + s + "/" + key}
		for i := 0; i < events; i++ {
			out = append(out, "event:"+s+"/"+key)
		}
		return out
	}
	parent := append(append(all(p1, "attendance_note", "2026-10-14:e_parent1", 2), all(p2, "attendance_note", "2026-10-15:e_parent2", 1)...),
		all(k, "kid_note", "2026-10-14:e_child2", 1)...)
	sort.Strings(parent)
	want := map[string][]string{
		"e_parent1": parent,
		"e_parent2": parent,
		"e_child2":  all(k, "kid_note", "2026-10-14:e_child2", 1),
		"e_guest1":  {},
		"e_child1":  {}, // erased, still on the roster
	}
	sort.Strings(want["e_child2"])
	for m, w := range want {
		if g := keptReads(r.readsOf(m)); fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("%s reads %v, want %v", m, g, w)
		}
	}
	// A guardian added to the roster afterwards reads none of it.
	r.setRoster(strings.Replace(journaltest.HouseholdV1, `"e_guest1":  {"agent": "agent-guest1"}`,
		`"e_guest1":  {"agent": "agent-guest1"}, "e_parent9": {"agent": "agent-parent9", "guardian_of": ["e_child1", "e_child2"]}`, 1))
	if g := keptReads(r.readsOf("e_parent9")); len(g) != 0 {
		t.Errorf("a later guardian reads %v", g)
	}
	r.noMismatch()
	if got := r.idsEverywhere("e_child1"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
	if strings.Contains(r.readsEverywhere(), "e_child1") {
		t.Error("a read returns the erased id")
	}
}

// The review probe's widening, as a test: e_parent3, a guardian of another
// child only, reads the note about e_child1 neither before nor after the
// erase, and neither does a guardian of e_child1 added later; the note's
// author and e_child1's guardian do after.
func TestEraseKeepsAGuardianOfAnotherChildOut(t *testing.T) {
	r := newNoteRig(t)
	raw := `{"people": {
		"e_parent1": {"agent": "a1", "guardian_of": ["e_child1"]}, "e_parent3": {"agent": "a3", "guardian_of": ["e_child2"]},
		"e_child1": {"agent": "c1", "siblings": ["e_child2"]}, "e_child2": {"agent": "c2", "siblings": ["e_child1"]},
		"e_guest1": {"agent": "g1"}}, "household_members": ["e_parent1", "e_parent3", "e_child1", "e_child2"]}`
	r.setRoster(raw)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "Jerry and I went to the store"}`)
	if g := r.readsOf("e_parent3"); len(g) != 0 {
		t.Fatalf("before: %v", g)
	}
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if g := keptReads(r.readsOf("e_parent3")); len(g) != 0 {
		t.Errorf("after: e_parent3 reads %v", g)
	}
	if g := keptReads(r.readsOf("e_parent1")); len(g) != 3 {
		t.Errorf("after: the author reads %v", g)
	}
	r.setRoster(strings.Replace(raw, `"e_guest1": {"agent": "g1"}`, `"e_guest1": {"agent": "g1"}, "e_parent4": {"agent": "a4", "guardian_of": ["e_child1"]}`, 1))
	if g := keptReads(r.readsOf("e_parent4")); len(g) != 0 {
		t.Errorf("a later guardian reads %v", g)
	}
}

// A note with no frozen reader (no guardian left on the roster, its author
// gone from it) is deleted with the subject's data; one with a reader stays.
func TestEraseDropsANoteWithNoFrozenReader(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.mustNote("kid_note", "e_child2", "e_child1", "2026-10-14", `{"text": "b"}`)
	r.setRoster(rosterWithout(t, journaltest.HouseholdV1, "e_parent1", "e_parent2"))
	resp, err := r.j.Erase(ctx, "admin", "e_child1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotesPseudonymised != 1 || resp.Current != 1 || resp.Events != 1 || len(r.kept("attendance_note")) != 0 {
		t.Errorf("%+v %v", resp, r.kept("attendance_note"))
	}
	if k := r.kept("kid_note")["2026-10-14:e_child2"]; k == "" || r.readersOf(k) != "e_child2" {
		t.Errorf("kid_note kept %v", r.kept("kid_note"))
	}
	if r.count("erasure_reader") != 1 || r.foldBroken() != 0 {
		t.Error("reader rows or fold")
	}
}

// Erasing a member who is a frozen reader of kept notes: they are no reader
// any more; a kept note they were the only reader of goes; one they wrote
// goes (rule 1) with all its reader rows; erasure_reader never names them.
func TestEraseRemovesAnErasedReader(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-15", `{"what_happened": "b"}`)
	r.mustNote("attendance_note", "e_guest1", "e_guest1", "2026-10-16", `{"what_happened": "c"}`)
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	kept := r.kept("attendance_note")
	a, b := kept["2026-10-14:e_parent1"], kept["2026-10-15:e_parent2"]
	// As if the frozen sets had been other: e_guest1 reads a, and is b's
	// only reader.
	r.exec(t, `INSERT INTO erasure_reader VALUES ($1, 'e_guest1'), ($2, 'e_guest1')`, a, b)
	r.exec(t, `DELETE FROM erasure_reader WHERE pseudonym = $1 AND member <> 'e_guest1'`, b)
	if g := keptReads(r.readsOf("e_guest1")); len(g) != 6 {
		t.Fatalf("setup: %v", g)
	}
	resp, err := r.j.Erase(ctx, "admin", "e_guest1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotesDeletedAsAuthor != 1 || resp.Current != 2 || resp.Events != 2 {
		t.Errorf("%+v", resp)
	}
	if kept := r.kept("attendance_note"); len(kept) != 1 || kept["2026-10-14:e_parent1"] != a || r.readersOf(a) != "e_parent1,e_parent2" {
		t.Errorf("kept %v, readers of a %q", kept, r.readersOf(a))
	}
	if got := r.idsEverywhere("e_guest1"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
	// The author of a kept note is erased: the note and all its reader rows
	// go, another reader's (e_parent2) included.
	if _, err := r.j.Erase(ctx, "admin", "e_parent1"); err != nil {
		t.Fatal(err)
	}
	if r.count("record_current") != 0 || r.count("erasure_reader") != 0 || r.foldBroken() != 0 {
		t.Errorf("%d rows, %d reader rows left", r.count("record_current"), r.count("erasure_reader"))
	}
}

// A member erased before is never a frozen reader of a later erasure's
// notes, although still on the roster.
func TestALaterEraseLeavesAnErasedMemberOut(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	if _, err := r.j.Erase(ctx, "admin", "e_parent2"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	if s := r.kept("attendance_note")["2026-10-14:e_parent1"]; r.readersOf(s) != "e_parent1" {
		t.Errorf("readers %q", r.readersOf(s))
	}
	if got := r.idsEverywhere("e_parent2"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
}

// A member who wrote notes and is the subject of others: their own notes
// go everywhere (rule 1), others' notes about them are kept.
func TestEraseAMemberWhoIsAuthorAndSubject(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("family_note", "e_child1", "e_child2", "2026-10-14", `{"text": "c1 on c2"}`)
	r.mustNote("kid_note", "e_child1", "e_child2", "2026-10-14", `{"text": "c1 on c2"}`)
	r.mustNote("attendance_note", "e_child1", "e_child1", "2026-10-14", `{"what_happened": "c1 on c1"}`)
	r.mustNote("family_note", "e_parent1", "e_child1", "2026-10-14", `{"text": "p1 on c1"}`)
	r.mustNote("kid_note", "e_child2", "e_child2", "2026-10-14", `{"text": "c2 on c2"}`)
	resp, err := r.j.Erase(ctx, "admin", "e_child1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotesDeletedAsAuthor != 3 || resp.NotesPseudonymised != 1 {
		t.Errorf("%+v", resp)
	}
	if got := r.rowsWhere(`data_subject = 'e_child2'`); strings.Contains(got, "e_child1") || !strings.Contains(got, "c2 on c2") {
		t.Errorf("e_child2 rows:\n%s", got)
	}
	if kept := r.kept("family_note"); len(kept) != 1 || r.readersOf(kept["2026-10-14:e_parent1"]) != "e_child2,e_parent1,e_parent2" {
		t.Errorf("kept %v %q", kept, r.readersOf(kept["2026-10-14:e_parent1"]))
	}
	if got := r.idsEverywhere("e_child1"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
}

// Every erasure draws its own pseudonym and every kept note its own
// subject: the same author's notes of one date about two erased subjects
// and a household note of that date live side by side; a second erase of a
// subject keeps the ban and draws nothing new; the author's kept notes go
// when the author is erased.
func TestErasuresDrawDistinctPseudonyms(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("family_note", "e_parent1", "e_child1", "2026-10-14", `{"text": "a"}`)
	r.mustNote("family_note", "e_parent1", "e_child2", "2026-10-14", `{"text": "b"}`)
	r.mustNote("family_note", "e_parent1", "", "2026-10-14", `{"text": "household"}`)
	for _, s := range []string{"e_child1", "e_child2", "e_child1"} {
		if _, err := r.j.Erase(ctx, "admin", s); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := r.j.db.Query(`SELECT data_subject FROM record_current WHERE key = '2026-10-14:e_parent1' ORDER BY 1`)
	var subjects []string
	pseudonyms := map[string]bool{}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		subjects = append(subjects, s)
		if m := keptSubject.FindStringSubmatch(s); m != nil {
			pseudonyms[m[1]] = true
		}
	}
	rows.Close()
	if len(subjects) != 3 || subjects[0] != "" || len(pseudonyms) != 2 {
		t.Errorf("subjects %v", subjects)
	}
	if _, rej := r.note("e_parent1", "e_child1", "2026-10-20", `{}`); rej == nil || rej.Status != 410 {
		t.Errorf("after a second erase: %v", rej)
	}
	resp, err := r.j.Erase(ctx, "admin", "e_parent1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotesDeletedAsAuthor != 3 || r.count("record_current") != 0 || r.count("record_event") != 0 || r.count("erasure_reader") != 0 {
		t.Errorf("%+v; %d rows left", resp, r.count("record_current"))
	}
}

// Decision B: an erase is a ban. Every later snapshot (Apply and a held
// confirm), every note about the member and every note by them is refused
// with 410, whatever its time; another subject, and a household note by
// someone else, are not.
func TestEraseBansLaterData(t *testing.T) {
	r := newNoteRig(t)
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	r.clock.Set(T0.Add(30 * 24 * time.Hour))
	gone := func(what string, rej *record.Rejection) {
		t.Helper()
		if rej == nil || rej.Status != 410 || rej.Reason != journalapi.ReasonErasedSubject || !strings.Contains(rej.Detail, "ban") {
			t.Errorf("%s: %v", what, rej)
		}
	}
	gone("snapshot observed after", r.apply(snap(1, "e_child1", "c1", T0.Add(30*24*time.Hour), true, keep(1)...)).Rejection)
	_, rej := r.note("e_parent1", "e_child1", "2026-11-14", `{"what_happened": "x"}`)
	gone("note about them", rej)
	_, rej = r.note("e_child1", "e_child2", "2026-11-14", `{"what_happened": "x"}`)
	gone("note by them", rej)
	n, _ := noteOf(r.reg, "family_note", "e_child1", "", "2026-11-14", `{"text": "x"}`)
	_, rej, err := r.j.WriteNote(ctx, gateway, n)
	if err != nil {
		t.Fatal(err)
	}
	gone("household note by them", rej)
	// A held snapshot of the subject that got in some other way.
	r.storeRawHeld(7, "assignment", scopeKey("e_child1", "c1"), snap(7, "e_child1", "c1", T0, true, keep(1)...))
	out, err := r.j.ConfirmHeld(ctx, uid(7), allowAll)
	if err != nil {
		t.Fatal(err)
	}
	gone("held confirm", out.Rejection)
	if r.count("record_current") != 0 || r.count("record_event") != 0 {
		t.Error("banned data was stored")
	}
	// Not banned: another subject, a household note by someone else.
	if out := r.apply(snap(2, "e_child2", "c1", T0.Add(30*24*time.Hour), true, rec("c1:1", "e_child2", `{"title": "x"}`))); out.Rejection != nil {
		t.Errorf("another subject: %+v", out)
	}
	if _, rej := r.note("e_parent1", "e_child2", "2026-11-14", `{"what_happened": "x"}`); rej != nil {
		t.Errorf("a note about another subject: %v", rej)
	}
	if resp := r.mustNote("family_note", "e_parent1", "", "2026-11-14", `{"text": "x"}`); resp.Status != "created" {
		t.Errorf("a household note by someone else: %+v", resp)
	}
}

// A kept note keeps its whole history: every event, its audience and
// prev_audience as written, so it folds to its fields; prev_scope becomes
// the household scope and group_key is cleared. Events that a type version
// with another fixed audience wrote are kept as well.
func TestEraseRewritesAKeptNotesHistory(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-14", `{"what_happened": "b"}`)
	r.clock.Set(T0.Add(time.Hour))
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-14", `{"what_happened": "b2", "heard_from": "subject"}`)
	r.exec(t, `ALTER TABLE record_event DISABLE TRIGGER USER`)
	r.exec(t, `UPDATE record_event SET audience = '{household,subject}' WHERE kind = 'created'`)
	r.exec(t, `UPDATE record_event SET prev_audience = '{household,subject}',
		prev_scope = 'source=household/record_type=attendance_note/data_subject=e_child1' WHERE kind = 'updated'`)
	r.exec(t, `UPDATE record_event SET group_key = 'data_subject=e_child1/field%3Adate=2026-10-14'`) // as a group_by rule makes it
	r.exec(t, `ALTER TABLE record_event ENABLE TRIGGER USER`)
	resp, err := r.j.Erase(ctx, "admin", "e_child1")
	if err != nil || resp.Events != 0 || resp.NotesPseudonymised != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	s := r.kept("attendance_note")["2026-10-14:e_parent2"]
	var got []string
	for _, e := range r.events(s) {
		got = append(got, e.Kind+"|"+strings.Join(e.Audience, ",")+"|"+strings.Join(e.PrevAudience, ",")+"|"+e.PrevScope+"|"+e.GroupKey)
	}
	if fmt.Sprint(got) != "[created|household,subject||| updated|guardians,subject|household,subject|source=household/record_type=attendance_note|]" {
		t.Errorf("history after erase: %v", got)
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
	// Its frozen reader sees every event, and old values across the
	// audience change: the frozen set, not the audience, decides.
	evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent2", Limit: 100})
	if err != nil || len(evs) != 2 || string(evs[1].Changed["what_happened"].Old) != `"b"` || len(evs[1].Notify) != 0 {
		t.Errorf("%+v %v", evs, err)
	}
	if ids := r.idsEverywhere("e_child1"); len(ids) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(ids, "\n"))
	}
}

// SQL and Go agree on kept notes, whatever their audience ("public" and
// household tokens included): visible exactly to the readers listed for
// each. Frozen-reader grants never reach an ordinary row, "public" rows
// stay public.
func TestKeptNoteVisibilitySQLAndGoAgree(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-10", `{"what_happened": "x"}`)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-11", `{"what_happened": "x"}`)
	r.mustNote("kid_note", "e_child2", "e_child1", "2026-10-12", `{"text": "x"}`)
	x := rec("pub", "e_child2", `{"title": "T"}`)
	x.Audience = []string{"public"}
	r.apply(snap(1, "e_child2", "c1", T0.Add(-time.Hour), true, x))
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, s := range r.kept("attendance_note") {
		subjects = append(subjects, s)
	}
	for _, s := range r.kept("kid_note") {
		subjects = append(subjects, s)
	}
	sort.Strings(subjects)
	if len(subjects) != 3 {
		t.Fatalf("kept %v", subjects)
	}
	// Audiences no roster grant could ever unlock for a pseudonym, and some
	// that would, were the branch missing.
	r.exec(t, `UPDATE record_current SET audience = '{public}' WHERE data_subject = $1`, subjects[0])
	r.exec(t, `UPDATE record_current SET audience = '{guardians,household}' WHERE data_subject = $1`, subjects[1])
	r.exec(t, `INSERT INTO erasure_reader VALUES ($1, 'e_guest1')`, subjects[2])
	// An event of another audience than the frozen one, for every kept note.
	r.exec(t, `ALTER TABLE record_event DISABLE TRIGGER USER`)
	r.exec(t, `INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience, changed,
		observed_at, approximate, actor, importance) SELECT record_type, data_subject, key, 'updated', 1, scope, '{subject}',
		'{}', last_confirmed_at, false, 'x', 'low' FROM record_current WHERE data_subject LIKE 'erased:%'`)
	r.exec(t, `ALTER TABLE record_event ENABLE TRIGGER USER`)
	// The rule written out from the tables: a kept row goes to its author,
	// and to its other listed readers when its audience is the frozen one.
	entitled := func(m, s string, audience []string) bool {
		var author, listed bool
		var frozen []byte
		err := r.j.db.QueryRow(`SELECT r.author, true, to_jsonb(k.audience) FROM erasure_reader r JOIN erasure_kept k USING (pseudonym)
			WHERE r.pseudonym = $1 AND r.member = $2`, s, m).Scan(&author, &listed, &frozen)
		if err != nil {
			return false
		}
		b, _ := json.Marshal(audience)
		return author || listed && string(b) == strings.ReplaceAll(string(frozen), " ", "")
	}
	for _, m := range r.ros.Load().Members() {
		if r.readRefused(m) {
			continue
		}
		grants, _, err := r.j.grantsOf(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"record_current", "record_event"} {
			rows, err := r.j.db.Query(`SELECT data_subject, key, to_jsonb(audience) FROM ` + table + ` ORDER BY 1, 2`)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for rows.Next() {
				var s, k string
				var aud []byte
				_ = rows.Scan(&s, &k, &aud)
				var audience []string
				_ = json.Unmarshal(aud, &audience)
				if access.Visible(grants, s, audience) {
					want = append(want, s+"/"+k)
				}
				if access.IsErased(s) && access.Visible(grants, s, audience) != entitled(m, s, audience) {
					t.Errorf("%s on %s %v: Go rule disagrees with the tables", m, s, audience)
				}
			}
			rows.Close()
			sort.Strings(want)
			got := r.stateKeys(m, StateQuery{})
			if table == "record_event" {
				got = r.eventKeys(m)
			}
			if fmt.Sprint(got) != fmt.Sprint(nonNil(want)) {
				t.Errorf("%s %s: SQL %v, Go %v", m, table, got, want)
			}
		}
	}
	if g := r.stateKeys("e_guest1", StateQuery{}); fmt.Sprint(g) != fmt.Sprint([]string{"e_child2/pub", subjects[2] + "/" + keyOf(r, subjects[2])}) {
		t.Errorf("guest reads %v", g)
	}
	r.noMismatch()
}

func keyOf(r *rig, subject string) string {
	var k string
	_ = r.j.db.QueryRow(`SELECT key FROM record_current WHERE data_subject = $1`, subject).Scan(&k)
	return k
}

// A note by the erased author about a subject Erase has not locked, written
// while Erase runs, cannot land between Erase's two rule-1 DELETEs (events
// first, then current rows) and leave an orphan event naming the author: the
// author lock makes it wait for the erase, and then the ban refuses it with
// 410.
// A statement trigger holds the events DELETE open until the note's row
// exists, or for two seconds. (Review probe TestZZProbeTorn, deterministic.)
func TestEraseIsNotTornByANoteOfTheAuthor(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	r.exec(t, `CREATE TABLE torn_flag (x int)`)
	r.exec(t, `CREATE FUNCTION torn_wait() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE deadline timestamptz := clock_timestamp() + interval '2 seconds';
		BEGIN
			IF EXISTS (SELECT 1 FROM torn_flag) THEN
				DELETE FROM torn_flag;
				WHILE NOT EXISTS (SELECT 1 FROM record_current WHERE key = '2026-10-15:e_parent1')
				      AND clock_timestamp() < deadline LOOP
					PERFORM pg_sleep(0.05);
				END LOOP;
			END IF;
			RETURN NULL;
		END $$`)
	r.exec(t, `CREATE TRIGGER torn_wait BEFORE DELETE ON record_event FOR EACH STATEMENT EXECUTE FUNCTION torn_wait()`)
	r.exec(t, `INSERT INTO torn_flag VALUES (1)`)
	// The erase always spends the trigger's full two seconds (the note waits
	// on the author lock, so its row never appears), so the 5s bounded()
	// budget left a loaded CI runner too little for the rest of the work
	// (pipeline 4069). This test checks the outcome, not blocking: a roomy
	// deadline, still finite so a real deadlock fails the test.
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var noteErr error
	var noteRej *record.Rejection
	r.j.beforeEraseLocks = func() {
		once.Do(func() {
			n, _ := noteOf(r.reg, "attendance_note", "e_parent1", "e_child2", "2026-10-15", `{"what_happened": "late"}`)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, noteRej, noteErr = r.j.WriteNote(opCtx, gateway, n)
			}()
		})
	}
	resp, err := r.j.Erase(opCtx, "admin", "e_parent1")
	wg.Wait()
	if err != nil || noteErr != nil {
		t.Fatalf("erase %v, note %v", err, noteErr)
	}
	if noteRej == nil || noteRej.Status != 410 {
		t.Errorf("the late note by the erased author: %v", noteRej)
	}
	if resp.NotesDeletedAsAuthor != 1 {
		t.Errorf("%+v", resp)
	}
	if r.foldBroken() != 0 {
		t.Errorf("torn erase: fold broken\n%s", r.rowsWhere(`key LIKE '%e_parent1%'`))
	}
	if got := r.rowsWhere(`key LIKE '%e_parent1%'`); got != "" {
		t.Errorf("the author's notes remain:\n%s", got)
	}
}

// Erase decides the frozen readers from one roster snapshot: a reload
// during the erase (here dropping e_parent2 and the subject) changes
// nothing.
func TestEraseUsesOneRosterSnapshot(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-15", `{"what_happened": "b"}`)
	r.j.beforeEraseLocks = func() {
		r.ros.Store(mustParse(t, rosterWithout(t, journaltest.HouseholdV1, "e_parent2", "e_child1")))
	}
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	kept := r.kept("attendance_note")
	for _, k := range []string{"2026-10-14:e_parent1", "2026-10-15:e_parent2"} {
		if r.readersOf(kept[k]) != "e_parent1,e_parent2" {
			t.Errorf("%s: readers %q", k, r.readersOf(kept[k]))
		}
	}
}

func mustParse(t *testing.T, raw string) *roster.Roster {
	t.Helper()
	ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return ros
}

// Erase runs while notes and snapshots are written for the subjects whose
// locks it takes, and while another erase of the same member runs: nothing
// deadlocks or fails, and every record still folds.
func TestEraseConcurrentWithWritersOfItsOtherSubjects(t *testing.T) {
	r := newNoteRig(t)
	for _, a := range []string{"e_parent1", "e_parent2"} {
		for _, s := range []string{"e_child1", "e_child2"} {
			r.mustNote("attendance_note", a, s, "2026-10-01", `{"what_happened": "x"}`)
		}
		r.mustNote("family_note", a, "", "2026-10-01", `{"text": "x"}`)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	wg.Add(1)
	go func() { // notes by e_parent2 about both children and the household
		defer wg.Done()
		for i := 0; i < 15; i++ {
			subject := []string{"e_child1", "e_child2", ""}[i%3]
			typ, fields := "attendance_note", `{"what_happened": "y"}`
			if subject == "" {
				typ, fields = "family_note", `{"text": "y"}`
			}
			n, rej := noteOf(r.reg, typ, "e_parent2", subject, fmt.Sprintf("2026-11-%02d", i+1), fields)
			if rej != nil {
				errs <- fmt.Errorf("note %d: %v", i, rej)
				continue
			}
			if _, rej, err := r.j.WriteNote(ctx, gateway, n); err != nil || rej != nil {
				errs <- fmt.Errorf("note %d: %v %v", i, rej, err)
			}
		}
	}()
	wg.Add(1)
	go func() { // snapshots of e_child2
		defer wg.Done()
		for i := 0; i < 10; i++ {
			s := snap(100+i, "e_child2", "c1", T0.Add(time.Duration(i-20)*time.Minute), true, rec("c1:1", "e_child2", fmt.Sprintf(`{"title": "t%d"}`, i)))
			if _, err := r.applyErr(s); err != nil {
				errs <- fmt.Errorf("apply %d: %w", i, err)
			}
		}
	}()
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				if _, err := r.j.Erase(ctx, "admin", "e_parent1"); err != nil {
					errs <- fmt.Errorf("erase: %w", err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	var all []string
	for err := range errs {
		all = append(all, err.Error())
	}
	if len(all) > 0 {
		t.Fatalf("%d errors:\n%s", len(all), strings.Join(all, "\n"))
	}
	if got := r.rowsWhere(`key LIKE '%:e_parent1'`); got != "" {
		t.Errorf("notes survive their author:\n%s", got)
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
}

// Migration 0003: record_event stays append-only except for scan, and for
// the one rewrite erase makes of a kept note's events, inside a transaction
// that set glovebox_journal.erase_rewrite = on: the row moves from a member
// to a kept-note subject "erased:<32 hex>:<n>", scope and prev_scope are
// replaced by scopes without a subject and group_key is cleared; nothing
// else changes.
func TestRecordEventEraseRewriteIsTheOnlyOtherChange(t *testing.T) {
	db := journaltest.NewDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var seq int64
	insert := func(subject string) {
		if err := db.QueryRow(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, prev_scope,
			audience, prev_audience, observed_at, approximate, actor, importance, changed, group_key) VALUES ('t', $1,
			'2026-10-14:e_parent1', 'updated', 1, 'source=x/data_subject=e_child1', 'source=x/data_subject=e_child1',
			'{guardians,subject}', '{guardians,subject}', now(), false, 'w;on_behalf_of=e_parent1', 'low', '{}',
			'data_subject=e_child1') RETURNING seq`, subject).Scan(&seq); err != nil {
			t.Fatal(err)
		}
	}
	insert("e_child1")
	p := "erased:" + strings.Repeat("ab", 16) + ":1"
	good := `data_subject = '` + p + `', scope = 'source=x', prev_scope = 'source=x', group_key = NULL`
	try := func(flag string, set string) error {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if flag != "-" {
			if _, err := tx.Exec(`SELECT set_config('glovebox_journal.erase_rewrite', $1, true)`, flag); err != nil {
				t.Fatal(err)
			}
		}
		_, err = tx.Exec(`UPDATE record_event SET `+set+` WHERE seq = $1`, seq)
		return err
	}
	for _, flag := range []string{"-", "off", "true", ""} {
		if err := try(flag, good); err == nil {
			t.Errorf("the erase rewrite went through with the flag %q", flag)
		}
	}
	if err := try("on", good); err != nil {
		t.Errorf("the erase rewrite was refused: %v", err)
	}
	if err := try("on", strings.Replace(good, `prev_scope = 'source=x', `, ``, 1)+`, prev_scope = 'source=y'`); err != nil {
		t.Errorf("another subject-free prev_scope was refused: %v", err)
	}
	if err := try("-", `scan = '{"f": {"new": "pass"}}'`); err != nil {
		t.Errorf("a scan update was refused: %v", err)
	}
	for name, set := range map[string]string{
		"kind":                     `kind = 'removed', ` + good,
		"actor":                    `actor = 'x', ` + good,
		"changed":                  `changed = '{"a": [1, 2]}', ` + good,
		"key":                      `key = key || ':x', ` + good,
		"audience":                 `audience = '{guardians}', ` + good,
		"prev_audience":            `prev_audience = '{guardians}', ` + good,
		"to the household":         strings.Replace(good, p, ``, 1),
		"to another member":        strings.Replace(good, p, `e_child2`, 1),
		"pseudonym too short":      strings.Replace(good, p, `erased:ab:1`, 1),
		"pseudonym without n":      strings.Replace(good, p, `erased:`+strings.Repeat("ab", 16), 1),
		"pseudonym with n 0":       strings.Replace(good, p, `erased:`+strings.Repeat("ab", 16)+`:0`, 1),
		"pseudonym upper hex":      strings.Replace(good, p, `erased:`+strings.Repeat("AB", 16)+`:1`, 1),
		"pseudonym with a suffix":  strings.Replace(good, p, p+`x`, 1),
		"group_key kept":           strings.Replace(good, `, group_key = NULL`, ``, 1),
		"scope keeps a subject":    strings.Replace(good, `scope = 'source=x', prev`, `scope = 'source=x/data_subject=e_child1', prev`, 1),
		"prev_scope keeps subject": strings.Replace(good, `prev_scope = 'source=x'`, `prev_scope = 'source=x/data_subject=e_child1'`, 1),
		"prev_scope dropped":       strings.Replace(good, `prev_scope = 'source=x'`, `prev_scope = NULL`, 1),
	} {
		if err := try("on", set); err == nil {
			t.Errorf("%s: allowed under the flag", name)
		}
	}
	// Household rows and kept notes are never rewritten.
	for _, s := range []string{"", p} {
		insert(s)
		if err := try("on", strings.Replace(good, p, "erased:"+strings.Repeat("cd", 16)+":2", 1)); err == nil {
			t.Errorf("an event of %q was rewritten", s)
		}
	}
	// erasure_reader holds kept-note subjects and member ids only: its own
	// CHECKs, tested with the foreign key dropped inside a transaction that
	// is rolled back.
	withoutFK := func(q string) error {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`ALTER TABLE erasure_reader DROP CONSTRAINT erasure_reader_kept`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(q)
		return err
	}
	for _, q := range []string{
		`INSERT INTO erasure_reader VALUES ('e_child1', 'e_parent1')`,
		`INSERT INTO erasure_reader VALUES ('` + p + `', '')`,
		`INSERT INTO erasure_reader VALUES ('` + p + `', '` + p + `')`,
	} {
		if err := withoutFK(q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	if err := withoutFK(`INSERT INTO erasure_reader VALUES ('` + p + `', 'e_parent3')`); err != nil {
		t.Errorf("a valid row was refused without the foreign key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO erasure_reader VALUES ('` + p + `', 'e_parent1')`); err == nil {
		t.Error("a reader row without its kept note")
	}
	if _, err := db.Exec(`INSERT INTO erasure_kept VALUES ('` + p + `', '{}')`); err == nil {
		t.Error("a kept note frozen from no audience")
	}
	if _, err := db.Exec(`INSERT INTO erasure_kept VALUES ('e_child1', '{guardians}')`); err == nil {
		t.Error("a kept note under a member id")
	}
	if _, err := db.Exec(`INSERT INTO erasure_kept VALUES ('` + p + `', '{guardians}')`); err != nil {
		t.Error(err)
	}
	if _, err := db.Exec(`INSERT INTO erasure_reader VALUES ('` + p + `', 'e_parent1')`); err != nil {
		t.Error(err)
	}
}
