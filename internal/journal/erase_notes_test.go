package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// Erase and household notes (decision 2026-10-23, spec 4.13): an author's
// notes go with the author; notes others wrote about an erased subject stay,
// moved to the household with the subject replaced by a pseudonym.

// noteTypes are two extra household types: family_note can be written about
// the household itself and keeps a guardians part; kid_note has no
// guardians part, so once its subject is gone nobody is left to read it.
func noteTypes() []record.Type {
	date := record.FieldDef{Type: record.Date}
	text := record.FieldDef{Type: record.Text}
	return []record.Type{
		{Name: "family_note", SchemaVersion: 1, Owner: record.OwnerHousehold, AllowNoSubject: true,
			FixedAudience: []string{"guardians", "household"}, Fields: map[string]record.FieldDef{"date": date, "text": text}},
		{Name: "kid_note", SchemaVersion: 1, Owner: record.OwnerHousehold,
			FixedAudience: []string{"household"}, Fields: map[string]record.FieldDef{"date": date, "text": text}},
	}
}

func newNoteRig(t *testing.T) *rig {
	return newRig(t, func(o *Options) { o.Registry = journaltest.Registry(t, noteTypes()...) })
}

// setRoster loads a roster document into the rig.
func (r *rig) setRoster(raw string) {
	r.t.Helper()
	ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(raw))
	if err != nil {
		r.t.Fatal(err)
	}
	r.ros.Store(ros)
	r.rosRaw = raw
}

// rosterWithout is the roster document raw without the members ids, and
// without every reference to them.
func rosterWithout(t testing.TB, raw string, ids ...string) string {
	t.Helper()
	var d struct {
		People map[string]struct {
			Agent      string   `json:"agent"`
			GuardianOf []string `json:"guardian_of,omitempty"`
			Siblings   []string `json:"siblings,omitempty"`
		} `json:"people"`
		HouseholdMembers []string `json:"household_members"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
		delete(d.People, id)
	}
	keep := func(xs []string) []string {
		out := []string{}
		for _, x := range xs {
			if !gone[x] {
				out = append(out, x)
			}
		}
		return out
	}
	for id, p := range d.People {
		p.GuardianOf, p.Siblings = keep(p.GuardianOf), keep(p.Siblings)
		d.People[id] = p
	}
	d.HouseholdMembers = keep(d.HouseholdMembers)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// eraseOff erases subject the way an operator must: off the roster first.
// The roster in use before is restored afterwards, as if the member were
// added back, so tests can go on writing about them.
func (r *rig) eraseOff(c context.Context, actor, subject string) (journalapi.EraseResponse, error) {
	r.t.Helper()
	before := r.rosRaw
	if before == "" {
		r.t.Fatal("eraseOff needs a roster set through newRig or setRoster")
	}
	if subject != "" {
		r.setRoster(rosterWithout(r.t, before, subject))
	}
	defer r.setRoster(before)
	return r.j.Erase(c, actor, subject)
}

func noteOf(reg *record.Registry, typ, onBehalfOf, subject, date, fields string) (*record.NormNote, *record.Rejection) {
	var f map[string]json.RawMessage
	_ = json.Unmarshal([]byte(fields), &f)
	return record.ValidateNote(reg, journalapi.Note{RecordType: typ, Date: date, DataSubject: subject, Fields: f, OnBehalfOf: onBehalfOf})
}

// mustNote writes a note and fails the test on any refusal.
func (r *rig) mustNote(typ, onBehalfOf, subject, date, fields string) journalapi.NoteResponse {
	r.t.Helper()
	n, rej := noteOf(r.reg, typ, onBehalfOf, subject, date, fields)
	if rej != nil {
		r.t.Fatalf("note %s by %s about %q: %v", typ, onBehalfOf, subject, rej)
	}
	resp, rej, err := r.j.WriteNote(ctx, gateway, n)
	if err != nil || rej != nil {
		r.t.Fatalf("note %s by %s about %q: %v %v", typ, onBehalfOf, subject, rej, err)
	}
	return resp
}

// rowsWhere renders every record_current and record_event row matching cond
// (over alias t), ordered, for before/after comparisons.
func (r *rig) rowsWhere(cond string) string {
	r.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT to_jsonb(t)::text FROM record_current t WHERE ` + cond + ` ORDER BY record_type, data_subject, key`,
		`SELECT to_jsonb(t)::text FROM record_event t WHERE ` + cond + ` ORDER BY seq`,
	} {
		rows, err := r.j.db.Query(q)
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

// idsEverywhere lists every row of every table that contains id anywhere,
// except the data_subject column of the erasure audit rows.
func (r *rig) idsEverywhere(id string) []string {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		r.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		tables = append(tables, s)
	}
	rows.Close()
	if len(tables) < 6 {
		r.t.Fatalf("only %d tables scanned: %v", len(tables), tables)
	}
	var out []string
	for _, table := range tables {
		row := `to_jsonb(t)`
		if table == "erasure" {
			row = `(to_jsonb(t) - 'data_subject')`
		}
		rs, err := r.j.db.Query(`SELECT `+row+`::text FROM `+table+` t WHERE strpos(`+row+`::text, $1) > 0`, id)
		if err != nil {
			r.t.Fatal(err)
		}
		for rs.Next() {
			var s string
			_ = rs.Scan(&s)
			out = append(out, table+": "+s)
		}
		rs.Close()
	}
	return out
}

// readsEverywhere is what every roster member reads through State, Events
// and Counts, as JSON.
func (r *rig) readsEverywhere() string {
	r.t.Helper()
	var b strings.Builder
	for _, m := range r.ros.Load().Members() {
		st, err := r.j.State(ctx, StateQuery{Reader: m, IncludeRemoved: true})
		if err != nil {
			r.t.Fatal(err)
		}
		ev, err := r.j.Events(ctx, EventsQuery{Reader: m, Limit: MaxEventsLimit})
		if err != nil {
			r.t.Fatal(err)
		}
		cs, err := r.j.Counts(ctx, m, nil, T0.Add(-1000*time.Hour))
		if err != nil {
			r.t.Fatal(err)
		}
		for _, v := range []any{st, ev, cs} {
			j, _ := json.Marshal(v)
			b.Write(j)
		}
	}
	return b.String()
}

// foldBroken counts record_current rows whose last_event_seq is not the
// newest event of their key, or whose scope or audience differs from it,
// plus orphan events: events whose key has no record_current row.
func (r *rig) foldBroken() int {
	r.t.Helper()
	var n, orphans int
	if err := r.j.db.QueryRow(`SELECT count(*) FROM record_current c LEFT JOIN LATERAL (
			SELECT e.seq, e.scope, e.audience FROM record_event e
			WHERE e.record_type = c.record_type AND e.data_subject = c.data_subject AND e.key = c.key
			ORDER BY e.seq DESC LIMIT 1) l ON true
		WHERE l.seq IS DISTINCT FROM c.last_event_seq OR l.scope IS DISTINCT FROM c.scope OR l.audience IS DISTINCT FROM c.audience`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	if err := r.j.db.QueryRow(`SELECT count(*) FROM record_event e WHERE NOT EXISTS (SELECT 1 FROM record_current c
		WHERE c.record_type = e.record_type AND c.data_subject = e.data_subject AND c.key = e.key)`).Scan(&orphans); err != nil {
		r.t.Fatal(err)
	}
	return n + orphans + r.fieldsUnfolded()
}

// fieldsUnfolded counts record_current rows whose fields and removed flag
// are not the fold of their events (created/restored set the fields,
// updated applies each [old, new] pair, removed sets the tombstone). Every
// row these tests write has one schema version, so no renames apply.
func (r *rig) fieldsUnfolded() int {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT c.fields, c.removed, coalesce(jsonb_agg(jsonb_build_array(e.kind, e.fields, e.changed)
		ORDER BY e.seq) FILTER (WHERE e.seq IS NOT NULL), '[]')
		FROM record_current c LEFT JOIN record_event e
		ON e.record_type = c.record_type AND e.data_subject = c.data_subject AND e.key = c.key
		GROUP BY c.record_type, c.data_subject, c.key, c.fields, c.removed`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	bad := 0
	for rows.Next() {
		var cur, evs []byte
		var removed bool
		if err := rows.Scan(&cur, &removed, &evs); err != nil {
			r.t.Fatal(err)
		}
		var want map[string]any
		_ = json.Unmarshal(cur, &want)
		var list [][3]json.RawMessage
		_ = json.Unmarshal(evs, &list)
		got, gone := map[string]any{}, false
		for _, e := range list {
			var kind string
			_ = json.Unmarshal(e[0], &kind)
			switch kind {
			case "created", "restored":
				got = map[string]any{}
				_ = json.Unmarshal(e[1], &got)
				gone = false
			case "updated":
				var ch map[string][2]any
				_ = json.Unmarshal(e[2], &ch)
				for f, p := range ch {
					if p[1] == nil {
						delete(got, f)
					} else {
						got[f] = p[1]
					}
				}
			case "removed":
				gone = true
			}
		}
		if !reflect.DeepEqual(got, want) || gone != removed {
			bad++
		}
	}
	return bad
}

// The fold check sees an orphan event.
func TestFoldBrokenCountsOrphanEvents(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	if r.foldBroken() != 0 {
		t.Fatal("setup")
	}
	r.exec(t, `DELETE FROM record_current`)
	if r.foldBroken() != 1 {
		t.Error("an orphan event was not counted")
	}
}

// A household note (no subject, for a type that allows it) needs a
// household grant for the type's fixed audience, like any other note.
func TestHouseholdNoteNeedsAHouseholdGrant(t *testing.T) {
	r := newNoteRig(t)
	if resp := r.mustNote("family_note", "e_child1", "", "2026-10-14", `{"text": "x"}`); resp.Status != "created" {
		t.Errorf("%+v", resp) // e_child1 holds ("", household)
	}
	n, rej := noteOf(r.reg, "family_note", "e_guest1", "", "2026-10-14", `{"text": "x"}`)
	if rej != nil {
		t.Fatal(rej)
	}
	if _, rej, err := r.j.WriteNote(ctx, gateway, n); err != nil || rej == nil || rej.Status != 403 {
		t.Errorf("a member with no household grant wrote a household note: %v %v", rej, err)
	}
	if _, rej := noteOf(r.reg, "attendance_note", "e_parent1", "", "2026-10-14", `{}`); rej == nil {
		t.Error("a household note of a type that needs a subject validated")
	}
}

func TestEraseAnAuthorRemovesEveryNoteTheyWrote(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "p1 on c1"}`)
	r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "p1 on c2"}`)
	r.mustNote("family_note", "e_parent1", "", "2026-10-14", `{"text": "p1 on the household"}`)
	r.clock.Set(T0.Add(time.Hour))
	r.mustNote("family_note", "e_parent1", "", "2026-10-14", `{"text": "p1 on the household, corrected"}`)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-14", `{"what_happened": "p2 on c1"}`)
	r.mustNote("family_note", "e_parent2", "", "2026-10-14", `{"text": "p2 on the household"}`)
	r.mustNote("attendance_note", "e_child1", "e_child1", "2026-10-14", `{"what_happened": "c1 on c1"}`)
	others := r.rowsWhere(`key NOT LIKE '%:e_parent1'`)
	if strings.Count(others, "\n") != 6 {
		t.Fatalf("setup: other authors' rows:\n%s", others)
	}
	attempts := 0
	r.j.beforeEraseLocks = func() { attempts++ }
	resp, err := r.eraseOff(ctx, "admin", "e_parent1")
	if err != nil {
		t.Fatal(err)
	}
	// The look-up before the locks found every subject: no restart.
	if attempts != 1 {
		t.Errorf("%d attempts", attempts)
	}
	if resp.NotesDeletedAsAuthor != 3 || resp.NotesPseudonymised != 0 || resp.Current != 3 || resp.Events != 4 {
		t.Errorf("%+v", resp)
	}
	if got := r.rowsWhere(`key LIKE '%e_parent1%'`); got != "" {
		t.Errorf("the author's notes remain:\n%s", got)
	}
	if got := r.rowsWhere(`key NOT LIKE '%:e_parent1'`); got != others {
		t.Errorf("other authors' notes changed:\n%s\nwas\n%s", got, others)
	}
	if got := r.idsEverywhere("e_parent1"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
	var counts string
	_ = r.j.db.QueryRow(`SELECT rows::text FROM erasure WHERE data_subject = 'e_parent1'`).Scan(&counts)
	if counts != `{"snapshot": 0, "record_event": 4, "record_current": 3, "notes_pseudonymised": 0, "notes_deleted_as_author": 3}` {
		t.Errorf("erasure counts %s", counts)
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
}

// Only household notes have an author in their key: a school record whose
// key happens to end in ":<id>" is not the author's, and stays.
func TestEraseAnAuthorLeavesSchoolRecordsWithALookalikeKey(t *testing.T) {
	r := newNoteRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("2026-10-14:e_parent1", "e_child1", `{"title": "A"}`)))
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	resp, err := r.eraseOff(ctx, "admin", "e_parent1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotesDeletedAsAuthor != 1 {
		t.Errorf("%+v", resp)
	}
	if _, ok := r.current("assignment", "e_child1", "2026-10-14:e_parent1"); !ok {
		t.Error("a school record was taken for the author's note")
	}
	if _, ok := r.current("attendance_note", "e_child1", "2026-10-14:e_parent1"); ok {
		t.Error("the author's note stayed")
	}
}

// Ids that are prefixes of each other: erasing e_ann leaves e_anna's notes,
// as author and as subject.
func TestEraseAnAuthorWhoseIdPrefixesAnother(t *testing.T) {
	r := newNoteRig(t)
	r.setRoster(`{"people": {
		"e_ann": {"agent": "a1", "guardian_of": ["e_child1"]}, "e_anna": {"agent": "a2", "guardian_of": ["e_child1"]},
		"e_child1": {"agent": "c1"}}, "household_members": ["e_ann", "e_anna", "e_child1"]}`)
	r.mustNote("attendance_note", "e_ann", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.mustNote("attendance_note", "e_anna", "e_child1", "2026-10-14", `{"what_happened": "b"}`)
	r.mustNote("family_note", "e_anna", "", "2026-10-14", `{"text": "c"}`)
	keep := r.rowsWhere(`key LIKE '%:e_anna'`)
	resp, err := r.eraseOff(ctx, "admin", "e_ann")
	if err != nil || resp.NotesDeletedAsAuthor != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	if got := r.rowsWhere(`key LIKE '%:e_anna'`); got != keep {
		t.Errorf("e_anna's notes changed:\n%s\nwas\n%s", got, keep)
	}
}

// Erase takes the lock of every subject its author's notes are about, and
// the household's: a writer holding one makes it wait, and a timed-out
// erase changes nothing. A subject the author never wrote about does not
// block it.
func TestEraseTakesTheLocksOfTheAuthorsOtherSubjects(t *testing.T) {
	for _, c := range []struct {
		held    string
		blocked bool
	}{{"e_child2", true}, {"", true}, {"e_parent1", true}, {"e_child1", false}} {
		t.Run(fmt.Sprintf("held=%q", c.held), func(t *testing.T) {
			r := newNoteRig(t)
			r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "x"}`)
			holder, err := r.j.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback() }()
			if err := lockSubject(bounded(t), holder, c.held); err != nil {
				t.Fatal(err)
			}
			cx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
			defer cancel()
			_, err = r.eraseOff(cx, "admin", "e_parent1")
			if blocked := err != nil; blocked != c.blocked {
				t.Errorf("blocked=%v (%v), want %v", blocked, err, c.blocked)
			}
			if c.blocked && (r.count("record_current") != 1 || r.count("erasure") != 0) {
				t.Error("a timed-out erase changed something")
			}
		})
	}
}

// The author lock: Erase of a member and WriteNote on behalf of that member
// both take it, so each waits for the other; another member's author lock
// blocks neither.
func TestEraseAndNotesTakeTheAuthorLock(t *testing.T) {
	for _, c := range []struct {
		held    string
		blocked bool
	}{{"e_parent1", true}, {"e_parent2", false}} {
		t.Run(c.held, func(t *testing.T) {
			r := newNoteRig(t)
			r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "x"}`)
			holder, err := r.j.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback() }()
			if err := lockAuthor(bounded(t), holder, c.held); err != nil {
				t.Fatal(err)
			}
			cx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
			defer cancel()
			n, _ := noteOf(r.reg, "attendance_note", "e_parent1", "e_child1", "2026-10-15", `{"what_happened": "y"}`)
			if _, _, err := r.j.WriteNote(cx, gateway, n); (err != nil) != c.blocked {
				t.Errorf("WriteNote: %v, want blocked=%v", err, c.blocked)
			}
			cx2, cancel2 := context.WithTimeout(ctx, 700*time.Millisecond)
			defer cancel2()
			if _, err := r.eraseOff(cx2, "admin", "e_parent1"); (err != nil) != c.blocked {
				t.Errorf("Erase: %v, want blocked=%v", err, c.blocked)
			}
			if c.blocked && (r.count("record_current") != 1 || r.count("erasure") != 0) {
				t.Error("a timed-out write or erase changed something")
			}
		})
	}
}

// Erase takes its locks in one order: the author lock, then the subject
// locks sorted ("" < e_child1 < e_child2 < e_parent1). Blocked on
// e_child2, it holds the author lock, "" and e_child1, and not yet
// e_parent1.
func TestEraseTakesItsLocksInSortedOrder(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "x"}`)
	holder, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if err := lockSubject(bounded(t), holder, "e_child2"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.eraseOff(bounded(t), "admin", "e_parent1")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		_ = r.j.db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting)
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Erase never waited on the held lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	author := func(c context.Context, tx *sql.Tx) error { return lockAuthor(c, tx, "e_parent1") }
	subj := func(s string) func(context.Context, *sql.Tx) error {
		return func(c context.Context, tx *sql.Tx) error { return lockSubject(c, tx, s) }
	}
	if tryLock(t, r.j, author) {
		t.Error("the author lock is not held")
	}
	for _, s := range []string{"", "e_child1"} {
		if tryLock(t, r.j, subj(s)) {
			t.Errorf("%q, before e_child2 in order, is not held", s)
		}
	}
	if !tryLock(t, r.j, subj("e_parent1")) {
		t.Error("e_parent1, after e_child2 in order, was taken early")
	}
	_ = holder.Rollback()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The rewrite flag is transaction-local: after an erase that used it
// commits, the same pooled connection (the pool is one connection here) and
// any new transaction see it unset, and the trigger refuses a rewrite.
func TestEraseRewriteFlagDoesNotOutliveTheErase(t *testing.T) {
	r := newNoteRig(t)
	r.j.db.SetMaxOpenConns(1)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "b"}`)
	resp, err := r.eraseOff(ctx, "admin", "e_child1")
	if err != nil || resp.NotesPseudonymised != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	var v sql.NullString
	if err := r.j.db.QueryRow(`SELECT current_setting('glovebox_journal.erase_rewrite', true)`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v.String == "on" {
		t.Error("the flag outlived the erase on its connection")
	}
	_, err = r.j.db.Exec(`UPDATE record_event SET data_subject = 'erased:` + strings.Repeat("0", 32) + `:1',
		scope = 'source=household/record_type=attendance_note', group_key = NULL WHERE data_subject = 'e_child2'`)
	if err == nil {
		t.Error("a rewrite went through after the erase")
	}
	tx, err := r.j.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(`SELECT current_setting('glovebox_journal.erase_rewrite', true)`).Scan(&v); err != nil || v.String == "on" {
		t.Errorf("a new transaction sees the flag %q (%v)", v.String, err)
	}
}
