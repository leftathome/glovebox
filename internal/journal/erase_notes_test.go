package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
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
// newest event of their key, or whose scope or audience differs from it.
func (r *rig) foldBroken() int {
	r.t.Helper()
	var n int
	if err := r.j.db.QueryRow(`SELECT count(*) FROM record_current c LEFT JOIN LATERAL (
			SELECT e.seq, e.scope, e.audience FROM record_event e
			WHERE e.record_type = c.record_type AND e.data_subject = c.data_subject AND e.key = c.key
			ORDER BY e.seq DESC LIMIT 1) l ON true
		WHERE l.seq IS DISTINCT FROM c.last_event_seq OR l.scope IS DISTINCT FROM c.scope OR l.audience IS DISTINCT FROM c.audience`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

var pseudonymKey = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}:[A-Za-z0-9._-]+):(erased:[0-9a-f]{32})$`)

// erasedRows is the household rows whose key carries a pseudonym, as
// original key -> pseudonym.
func (r *rig) erasedRows(typ string) map[string]string {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT key FROM record_current WHERE record_type = $1 AND data_subject = '' AND key LIKE '%:erased:%'`, typ)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		m := pseudonymKey.FindStringSubmatch(k)
		if m == nil {
			r.t.Fatalf("pseudonymised key %q has the wrong shape", k)
		}
		out[m[1]] = m[2]
	}
	return out
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
	resp, err := r.j.Erase(ctx, "admin", "e_parent1")
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
	resp, err := r.j.Erase(ctx, "admin", "e_parent1")
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

func TestEraseASubjectKeepsOthersNotesPseudonymised(t *testing.T) {
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
	before := map[string][]int64{}
	for _, e := range r.events("e_child1") {
		if e.Type == "attendance_note" && !strings.HasSuffix(e.Key, ":e_child1") {
			before[e.Key] = append(before[e.Key], e.Seq)
		}
	}
	child2 := r.dump("e_child2")
	r.clock.Set(T0.Add(2 * time.Hour))
	resp, err := r.j.Erase(ctx, "admin", "e_child1")
	if err != nil {
		t.Fatal(err)
	}
	// Deleted: the snapshot, 5 school records with 5 events, the subject's
	// own note and the kid_note (no guardians part), 1 event each.
	if resp.NotesPseudonymised != 2 || resp.NotesDeletedAsAuthor != 1 || resp.Snapshots != 1 || resp.Current != 7 || resp.Events != 7 {
		t.Errorf("%+v", resp)
	}
	if got := r.dump("e_child1"); got != "" {
		t.Errorf("rows of the erased subject remain:\n%s", got)
	}
	if got := r.dump("e_child2"); got != child2 {
		t.Errorf("another subject changed:\n%s\nwas\n%s", got, child2)
	}
	kept := r.erasedRows("attendance_note")
	p := kept["2026-10-14:e_parent1"]
	if len(kept) != 2 || p == "" || kept["2026-10-15:e_parent2"] != p {
		t.Fatalf("kept notes %v", kept)
	}
	if n := len(r.erasedRows("kid_note")); n != 0 {
		t.Errorf("a note with no guardians part was kept: %d", n)
	}
	c, ok := r.current("attendance_note", "", "2026-10-14:e_parent1:"+p)
	if !ok {
		t.Fatal("kept note not found")
	}
	if string(c.Fields["what_happened"]) != `"`+text+`"` || string(c.Fields["heard_from"]) != `"guardian"` ||
		c.Scope != "source=household/record_type=attendance_note" || strings.Join(c.Audience, ",") != "guardians" {
		t.Errorf("kept note %+v", c)
	}
	// Its events are the same events, rewritten in place: same seqs.
	var after []int64
	for _, e := range r.events("") {
		if e.Key == "2026-10-14:e_parent1:"+p {
			after = append(after, e.Seq)
			if e.Scope != c.Scope || (e.PrevScope != "" && e.PrevScope != c.Scope) || e.GroupKey != "" ||
				strings.Join(e.Audience, ",") != "guardians" || e.Actor != gateway+";on_behalf_of=e_parent1" {
				t.Errorf("kept event %+v", e)
			}
		}
	}
	if fmt.Sprint(after) != fmt.Sprint(before["2026-10-14:e_parent1"]) || len(after) != 2 {
		t.Errorf("events of the kept note: %v, were %v", after, before["2026-10-14:e_parent1"])
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
	// Guardians read the kept notes, pseudonym in place of the subject.
	want := "/2026-10-14:e_parent1:" + p
	for _, g := range []string{"e_parent1", "e_parent2"} {
		if !contains(r.stateKeys(g, StateQuery{}), want) || !contains(r.eventKeys(g), want) {
			t.Errorf("guardian %s does not read the kept note: %v", g, r.stateKeys(g, StateQuery{}))
		}
	}
	rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent2", Type: "attendance_note"})
	for _, s := range rows {
		if s.Key == want[1:] && string(s.Fields["what_happened"]) != `"`+text+`"` {
			t.Errorf("a guardian reads %s", s.Fields["what_happened"])
		}
	}
	// The erased subject (still in this roster), a sibling and a guest
	// never do.
	for _, m := range []string{"e_child1", "e_child2", "e_guest1"} {
		for _, k := range append(r.stateKeys(m, StateQuery{}), r.eventKeys(m)...) {
			if strings.Contains(k, "erased:") {
				t.Errorf("%s reads %s", m, k)
			}
		}
	}
	if got := r.idsEverywhere("e_child1"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
	if strings.Contains(r.readsEverywhere(), "e_child1") {
		t.Error("a read returns the erased id")
	}
	// The refusal guard is unchanged for the subject.
	if out := r.apply(snap(3, "e_child1", "c1", T0.Add(time.Hour), true, keep(1)...)); out.Rejection == nil || out.Rejection.Status != 410 {
		t.Errorf("snapshot observed before the erase: %+v", out)
	}
	if out := r.apply(snap(4, "e_child1", "c1", T0.Add(3*time.Hour), true, keep(1)...)); out.Rejection != nil {
		t.Errorf("snapshot observed after the erase: %+v", out)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// A kept note's events keep only the guardians part of prev_audience too
// (an audience change, e.g. a type version with another fixed audience):
// an old audience without it becomes empty, matching nobody.
func TestEraseNarrowsAKeptNotesPrevAudience(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.mustNote("attendance_note", "e_parent2", "e_child1", "2026-10-14", `{"what_happened": "b"}`)
	r.exec(t, `ALTER TABLE record_event DISABLE TRIGGER USER`)
	r.exec(t, `UPDATE record_event SET prev_audience = '{guardians,household,subject}' WHERE key = '2026-10-14:e_parent1'`)
	r.exec(t, `UPDATE record_event SET prev_audience = '{household,subject}' WHERE key = '2026-10-14:e_parent2'`)
	r.exec(t, `UPDATE record_event SET group_key = 'data_subject=e_child1/field%3Adate=2026-10-14'`) // as a group_by rule makes it
	r.exec(t, `ALTER TABLE record_event ENABLE TRIGGER USER`)
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range r.events("") {
		got[strings.SplitN(e.Key, ":erased:", 2)[0]] = strings.Join(e.PrevAudience, ",") + fmt.Sprint(e.PrevAudience != nil)
	}
	if got["2026-10-14:e_parent1"] != "guardianstrue" || got["2026-10-14:e_parent2"] != "true" {
		t.Errorf("prev_audience after erase: %v", got)
	}
	for _, e := range r.events("") {
		if e.GroupKey != "" {
			t.Errorf("group_key kept: %+v", e)
		}
	}
	if ids := r.idsEverywhere("e_child1"); len(ids) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(ids, "\n"))
	}
}

// A member who wrote notes and is the subject of others: their own notes
// go everywhere (rule 1), others' notes about them stay pseudonymised.
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
	if kept := r.erasedRows("family_note"); len(kept) != 1 || kept["2026-10-14:e_parent1"] == "" {
		t.Errorf("kept %v", kept)
	}
	if got := r.idsEverywhere("e_child1"); len(got) != 0 {
		t.Errorf("the erased id survives:\n%s", strings.Join(got, "\n"))
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
}

// Every erasure draws its own pseudonym: the same author's notes of one
// date about two erased subjects, and a household note of that date, live
// side by side; erasing a subject again uses yet another pseudonym, and
// the kept notes of an author go when the author is erased.
func TestErasuresDrawDistinctPseudonyms(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("family_note", "e_parent1", "e_child1", "2026-10-14", `{"text": "a"}`)
	r.mustNote("family_note", "e_parent1", "e_child2", "2026-10-14", `{"text": "b"}`)
	for _, s := range []string{"e_child1", "e_child2"} {
		if _, err := r.j.Erase(ctx, "admin", s); err != nil {
			t.Fatal(err)
		}
	}
	r.mustNote("family_note", "e_parent1", "", "2026-10-14", `{"text": "household"}`)
	r.clock.Set(T0.Add(time.Hour))
	r.mustNote("family_note", "e_parent1", "e_child1", "2026-10-14", `{"text": "c"}`)
	if _, err := r.j.Erase(ctx, "admin", "e_child1"); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows, _ := r.j.db.Query(`SELECT key FROM record_current WHERE data_subject = '' ORDER BY key`)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	seen := map[string]bool{}
	for _, k := range keys {
		if m := pseudonymKey.FindStringSubmatch(k); m != nil {
			seen[m[2]] = true
		}
	}
	if len(keys) != 4 || len(seen) != 3 || !contains(keys, "2026-10-14:e_parent1") {
		t.Errorf("household keys %v", keys)
	}
	resp, err := r.j.Erase(ctx, "admin", "e_parent1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotesDeletedAsAuthor != 4 || r.count("record_current") != 0 || r.count("record_event") != 0 {
		t.Errorf("%+v; %d rows left", resp, r.count("record_current"))
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
			_, err = r.j.Erase(cx, "admin", "e_parent1")
			if blocked := err != nil; blocked != c.blocked {
				t.Errorf("blocked=%v (%v), want %v", blocked, err, c.blocked)
			}
			if c.blocked && (r.count("record_current") != 1 || r.count("erasure") != 0) {
				t.Error("a timed-out erase changed something")
			}
		})
	}
}

// A note the author writes about a new subject between Erase's look-up and
// its locks is found under the locks and makes Erase start again with that
// subject's lock too: a writer still holding it makes Erase wait.
func TestEraseRechecksTheAuthorsSubjectsUnderTheLocks(t *testing.T) {
	r := newNoteRig(t)
	r.mustNote("attendance_note", "e_parent1", "e_child1", "2026-10-14", `{"what_happened": "x"}`)
	var holder interface{ Rollback() error }
	var once sync.Once
	attempts := 0
	r.j.beforeEraseLocks = func() {
		attempts++
		once.Do(func() {
			r.mustNote("attendance_note", "e_parent1", "e_child2", "2026-10-14", `{"what_happened": "late"}`)
			tx, err := r.j.db.BeginTx(ctx, nil)
			if err != nil {
				t.Error(err)
				return
			}
			holder = tx
			if err := lockSubject(bounded(t), tx, "e_child2"); err != nil {
				t.Error(err)
			}
		})
	}
	defer func() {
		if holder != nil {
			_ = holder.Rollback()
		}
	}()
	cx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	if _, err := r.j.Erase(cx, "admin", "e_parent1"); err == nil {
		t.Error("Erase did not wait for the late subject's lock")
	}
	if r.count("record_current") != 2 || r.count("erasure") != 0 {
		t.Error("a timed-out erase changed something")
	}
	// One restart, which then waits on the late subject's lock.
	if attempts != 2 {
		t.Errorf("%d attempts, want 2", attempts)
	}
	_ = holder.Rollback()
	resp, err := r.j.Erase(ctx, "admin", "e_parent1")
	if err != nil || resp.NotesDeletedAsAuthor != 2 || r.count("record_current") != 0 {
		t.Errorf("%+v %v", resp, err)
	}
}

// Erase runs while notes and snapshots are written for the subjects whose
// locks it takes, and while another erase takes an overlapping set: nothing
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
	for w, a := range []string{"e_parent1", "e_parent2"} {
		w, a := w, a
		wg.Add(1)
		go func() { // notes by both authors about both children and the household
			defer wg.Done()
			for i := 0; i < 15; i++ {
				subject := []string{"e_child1", "e_child2", ""}[i%3]
				typ := "attendance_note"
				fields := `{"what_happened": "y"}`
				if subject == "" {
					typ, fields = "family_note", `{"text": "y"}`
				}
				n, rej := noteOf(r.reg, typ, a, subject, fmt.Sprintf("2026-11-%02d", i+1), fields)
				if rej != nil {
					errs <- fmt.Errorf("note %d/%d: %v", w, i, rej)
					continue
				}
				if _, _, err := r.j.WriteNote(ctx, gateway, n); err != nil {
					errs <- fmt.Errorf("note %d/%d: %w", w, i, err)
				}
			}
		}()
	}
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
	for _, a := range []string{"e_parent1", "e_parent2"} {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := r.j.Erase(ctx, "admin", a); err != nil {
					errs <- fmt.Errorf("erase %s: %w", a, err)
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
	sort.Strings(all)
	if len(all) > 0 {
		t.Fatalf("%d errors:\n%s", len(all), strings.Join(all, "\n"))
	}
	if r.foldBroken() != 0 {
		t.Error("fold invariant broken")
	}
	for _, a := range []string{"e_parent1", "e_parent2"} {
		if _, err := r.j.Erase(ctx, "admin", a); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.rowsWhere(`record_type <> 'assignment'`); got != "" {
		t.Errorf("notes survive their authors:\n%s", got)
	}
}

// Migration 0002: record_event stays append-only except for scan, and for
// the one rewrite erase makes of a kept note's events, inside a transaction
// that set glovebox_journal.erase_rewrite: the row moves to the household
// (data_subject ”), its key gains a pseudonym suffix, scope and prev_scope
// are replaced by scopes without a subject, the audience can only shrink and
// group_key is cleared.
func TestRecordEventEraseRewriteIsTheOnlyOtherChange(t *testing.T) {
	db := journaltest.NewDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := db.QueryRow(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, prev_scope,
		audience, prev_audience, observed_at, approximate, actor, importance, changed, group_key) VALUES ('t', 'e_child1',
		'2026-10-14:e_parent1', 'updated', 1, 'source=x/data_subject=e_child1', 'source=x/data_subject=e_child1',
		'{guardians,subject}', '{guardians,subject}', now(), false, 'w;on_behalf_of=e_parent1', 'low', '{}',
		'data_subject=e_child1') RETURNING seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	const good = `data_subject = '', key = key || ':erased:00', scope = 'source=x', prev_scope = 'source=x', ` +
		`audience = '{guardians}', prev_audience = '{guardians}', group_key = NULL`
	try := func(flag bool, set string) error {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if flag {
			if _, err := tx.Exec(`SELECT set_config('glovebox_journal.erase_rewrite', 'on', true)`); err != nil {
				t.Fatal(err)
			}
		}
		_, err = tx.Exec(`UPDATE record_event SET `+set+` WHERE seq = $1`, seq)
		return err
	}
	if err := try(false, good); err == nil {
		t.Error("the erase rewrite went through without the flag")
	}
	if err := try(true, good); err != nil {
		t.Errorf("the erase rewrite was refused: %v", err)
	}
	if err := try(true, `scan = '{"f": {"new": "pass"}}'`); err != nil {
		t.Errorf("a scan update under the flag was refused: %v", err)
	}
	for name, set := range map[string]string{
		"kind":                     `kind = 'removed', ` + good,
		"actor":                    `actor = 'x', ` + good,
		"changed":                  `changed = '{"a": [1, 2]}', ` + good,
		"another subject":          strings.Replace(good, `data_subject = ''`, `data_subject = 'e_child2'`, 1),
		"subject only":             `data_subject = ''`,
		"key not suffixed":         strings.Replace(good, `key || ':erased:00'`, `'2026-10-14:e_parent2:erased:00'`, 1),
		"key without pseudonym":    strings.Replace(good, `key || ':erased:00'`, `key || ':x'`, 1),
		"key with empty pseudonym": strings.Replace(good, `key || ':erased:00'`, `key || ':erased:'`, 1),
		"audience grows":           strings.Replace(good, `audience = '{guardians}', prev`, `audience = '{guardians,public}', prev`, 1),
		"prev_audience grows":      strings.Replace(good, `prev_audience = '{guardians}'`, `prev_audience = '{guardians,public}'`, 1),
		"prev_audience dropped":    strings.Replace(good, `prev_audience = '{guardians}'`, `prev_audience = NULL`, 1),
		"group_key kept":           strings.Replace(good, `, group_key = NULL`, ``, 1),
		"scope keeps a subject":    strings.Replace(good, `scope = 'source=x', prev`, `scope = 'source=x/data_subject=e_child1', prev`, 1),
		"prev_scope keeps subject": strings.Replace(good, `prev_scope = 'source=x'`, `prev_scope = 'source=x/data_subject=e_child1'`, 1),
		"prev_scope dropped":       strings.Replace(good, `prev_scope = 'source=x'`, `prev_scope = NULL`, 1),
	} {
		if err := try(true, set); err == nil {
			t.Errorf("%s: allowed under the flag", name)
		}
	}
	// A household row (data_subject '') is never rewritten.
	if err := db.QueryRow(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope,
		audience, observed_at, approximate, actor, importance, fields) VALUES ('t', '', '2026-10-14:e_parent1', 'created', 1,
		'source=x', '{guardians}', now(), false, 'w', 'low', '{}') RETURNING seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if err := try(true, `key = key || ':erased:00'`); err == nil {
		t.Error("a household event was rewritten")
	}
}
