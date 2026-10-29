package journal

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// Erase under random rosters, notes and snapshots (spec 4.13). A model of
// the journal is kept beside it: which rows exist, who wrote them, and for
// kept notes the frozen readers, computed here from relationships and not by
// the journal's own rule. After every erase the journal must match the model
// exactly, member by member.

// pNote is a household note of the model.
type pNote struct {
	typ, key, author, subject string // subject "" is the household
	aud                       []string
	text                      string
	kept                      bool
	readers                   map[string]bool // frozen readers, kept notes only
	pseudonym                 string          // read back from the journal
}

// pRow is an assignment row of the model.
type pRow struct {
	subject, key string
	aud          []string
}

type erasureModel struct {
	d      genRoster
	erased map[string]bool
	rows   []*pRow
	notes  []*pNote
}

func (m *erasureModel) live() []string {
	var out []string
	for _, id := range m.d.members() {
		if !m.erased[id] {
			out = append(out, id)
		}
	}
	return out
}

// eraseModel applies an erase of e to the model and returns the counts the
// journal should report.
func (m *erasureModel) erase(e string, ex exercised) (kept, deletedAsAuthor int) {
	// Rule 1: the member's notes, kept ones included.
	var notes []*pNote
	for _, n := range m.notes {
		if n.author == e {
			deletedAsAuthor++
			continue
		}
		notes = append(notes, n)
	}
	m.notes = notes
	// The member reads no kept note any more; a kept note with no reader goes.
	notes = nil
	for _, n := range m.notes {
		if n.kept {
			delete(n.readers, e)
			if len(n.readers) == 0 {
				ex["kept-from-earlier-erase-dropped"]++
				continue
			}
		}
		notes = append(notes, n)
	}
	m.notes = notes
	// Rule 2: others' notes about the member are kept for their frozen readers.
	notes = nil
	for _, n := range m.notes {
		if n.kept || n.subject != e {
			notes = append(notes, n)
			continue
		}
		readers := map[string]bool{}
		for _, x := range m.d.members() {
			if x != e && !m.erased[x] && entitledBy(m.d, x, e, n.aud) {
				readers[x] = true
			}
		}
		if a := n.author; a != e && !m.erased[a] && m.d.People[a].Agent != "" {
			readers[a] = true
		}
		if len(readers) == 0 {
			ex["notes-dropped-unread"]++
			continue
		}
		n.kept, n.readers = true, readers
		kept++
		notes = append(notes, n)
	}
	m.notes = notes
	// Rule 3: the rest of the subject.
	var rows []*pRow
	for _, x := range m.rows {
		if x.subject != e {
			rows = append(rows, x)
		}
	}
	m.rows = rows
	m.erased[e] = true
	return kept, deletedAsAuthor
}

// expectedReads is what a member reads, as "type/subject/key" of rows, from
// the model. pseudonyms maps (type, key) of kept notes to their subject.
func (m *erasureModel) expectedReads(member string, pseudonyms map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, x := range m.rows {
		if entitledBy(m.d, member, x.subject, x.aud) {
			out["assignment/"+x.subject+"/"+x.key] = true
		}
	}
	for _, n := range m.notes {
		if n.kept {
			if n.readers[member] {
				out[n.typ+"/"+pseudonyms[n.typ+"/"+n.key]+"/"+n.key] = true
			}
			continue
		}
		if entitledBy(m.d, member, n.subject, n.aud) {
			out[n.typ+"/"+n.subject+"/"+n.key] = true
		}
	}
	return out
}

// noteFields is the JSON of a note's fields for its type.
func noteFields(typ, text string) string {
	if typ == "attendance_note" {
		return fmt.Sprintf(`{"what_happened": %q}`, text)
	}
	return fmt.Sprintf(`{"text": %q}`, text)
}

var noteAudiences = map[string][]string{
	"attendance_note": {"guardians", "subject"},
	"kid_note":        {"household"},
	"family_note":     {"guardians", "household"},
}

func (r *rig) keptSubjects() map[string]string {
	r.t.Helper()
	out := map[string]string{}
	for typ := range noteAudiences {
		for key, subject := range r.kept(typ) {
			out[typ+"/"+key] = subject
		}
	}
	return out
}

func (r *rig) tryNote(typ, author, subject, date, text string) (*record.Rejection, error) {
	n, rej := noteOf(r.reg, typ, author, subject, date, noteFields(typ, text))
	if rej != nil {
		return rej, nil
	}
	_, rej, err := r.j.WriteNote(ctx, gateway, n)
	return rej, err
}

func TestPropertyEraseMatchesTheModel(t *testing.T) {
	const seeds = 30
	ex := exercised{}
	for seed := int64(0); seed < seeds; seed++ {
		runEraseSeed(t, seed, ex)
	}
	requireTally(t, ex, "notes-kept", "notes-deleted-as-author", "notes-dropped-unread", "kept-from-earlier-erase-dropped",
		"erase-with-roster-removal", "erase-on-roster", "refused-410")
}

func runEraseSeed(t *testing.T, seed int64, ex exercised) {
	t.Helper()
	rnd := rand.New(rand.NewSource(seed))
	r := newRig(t, func(o *Options) { o.Registry = journaltest.Registry(t, noteTypes()...) })
	where := func(step string) string { return fmt.Sprintf("seed %d %s", seed, step) }
	n := 3 + rnd.Intn(3)
	m := &erasureModel{d: randomRosterN(rnd, "e_m", n), erased: map[string]bool{}}
	r.setRoster(m.d.raw())
	texts := []string{"alpha", "beta", "INJECT-gamma", "delta"}
	day := 0
	id := int(seed)*1000 + 1
	for _, subject := range m.d.members() {
		var recs []journalapi.Record
		for k := 0; k < 3; k++ {
			rc := rec(fmt.Sprintf("k%d", k), subject, fmt.Sprintf(`{"title": "t%d", "comment": %q}`, k, texts[rnd.Intn(len(texts))]))
			rc.Audience = audienceChoices[rnd.Intn(len(audienceChoices))]
			recs = append(recs, rc)
			m.rows = append(m.rows, &pRow{subject: subject, key: rc.Key, aud: sortedAud(rc.Audience)})
		}
		r.apply(snap(id, subject, "c1", T0.Add(-time.Hour), false, recs...))
		id++
	}
	writeNotes := func(count int) {
		for i := 0; i < count; i++ {
			live := m.live()
			if len(live) == 0 {
				return
			}
			typ := []string{"attendance_note", "kid_note", "family_note"}[rnd.Intn(3)]
			author := live[rnd.Intn(len(live))]
			subject := live[rnd.Intn(len(live))]
			if typ == "family_note" && rnd.Intn(2) == 0 {
				subject = ""
			}
			day++
			if day > 28 {
				return
			}
			date := fmt.Sprintf("2026-08-%02d", day)
			text := texts[rnd.Intn(len(texts))]
			rej, err := r.tryNote(typ, author, subject, date, text)
			if err != nil {
				t.Fatalf("%s: %v", where("note"), err)
			}
			if rej != nil {
				ex["note-refused"]++ // a policy or roster refusal: nothing was written
				continue
			}
			m.notes = append(m.notes, &pNote{typ: typ, key: date + ":" + author, author: author, subject: subject,
				aud: sortedAud(noteAudiences[typ]), text: text})
		}
	}
	writeNotes(10 + rnd.Intn(8))

	// verify compares the journal with the model for every member ever on the roster.
	everyone := m.d.members()
	verify := func(step string) {
		t.Helper()
		w := where(step)
		pseudonyms := r.keptSubjects()
		// the journal keeps exactly the model's kept notes, with exactly its frozen readers
		wantKept := 0
		for _, nt := range m.notes {
			if !nt.kept {
				continue
			}
			wantKept++
			p, ok := pseudonyms[nt.typ+"/"+nt.key]
			if !ok {
				t.Fatalf("%s: kept note %s/%s is missing", w, nt.typ, nt.key)
			}
			nt.pseudonym = p
			var rs []string
			for x := range nt.readers {
				rs = append(rs, x)
			}
			sort.Strings(rs)
			if got := r.readersOf(p); got != strings.Join(rs, ",") {
				t.Fatalf("%s: kept note %s/%s has readers %q, the frozen set is %q", w, nt.typ, nt.key, got, strings.Join(rs, ","))
			}
		}
		if len(pseudonyms) != wantKept {
			t.Fatalf("%s: %d kept notes in the journal, %d in the model", w, len(pseudonyms), wantKept)
		}
		for _, member := range everyone {
			r.assertSQLGoAgree(member)
			if _, onRoster := m.d.People[member]; m.erased[member] || !onRoster {
				if !r.readRefused(member) {
					t.Fatalf("%s: erased or off-roster %s can still read", w, member)
				}
				continue
			}
			want := m.expectedReads(member, pseudonyms)
			st, err := r.j.State(ctx, StateQuery{Reader: member, IncludeRemoved: true})
			if err != nil {
				t.Fatalf("%s: %s: %v", w, member, err)
			}
			got := map[string]bool{}
			for _, s := range st {
				got[s.RecordType+"/"+s.DataSubject+"/"+s.Key] = true
			}
			if !maps_equal(got, want) {
				t.Fatalf("%s: %s reads differ from the model:\n journal only: %v\n model only:   %v", w, member, minus(got, want), minus(want, got))
			}
			evs, err := r.j.Events(ctx, EventsQuery{Reader: member, Limit: MaxEventsLimit})
			if err != nil {
				t.Fatalf("%s: %s: %v", w, member, err)
			}
			gotEv := map[string]bool{}
			for _, e := range evs {
				gotEv[e.RecordType+"/"+e.DataSubject+"/"+e.Key] = true
			}
			if !maps_equal(gotEv, want) {
				t.Fatalf("%s: %s's events differ from the model:\n journal only: %v\n model only:   %v", w, member, minus(gotEv, want), minus(want, gotEv))
			}
			cs, err := r.j.Counts(ctx, member, nil, T0.Add(-1000*time.Hour))
			if err != nil {
				t.Fatalf("%s: %s: %v", w, member, err)
			}
			live := 0
			for _, c := range cs {
				live += c.Live
			}
			if live != len(want) {
				t.Fatalf("%s: %s counts %d live rows, the model %d", w, member, live, len(want))
			}
			for _, v := range []any{st, evs} {
				if b, _ := json.Marshal(v); strings.Contains(string(b), "INJECT") {
					t.Fatalf("%s: %s read withheld text", w, member)
				}
			}
		}
		for e := range m.erased {
			if got := r.idsEverywhere(e); len(got) != 0 {
				t.Fatalf("%s: erased %s survives:\n%s", w, e, strings.Join(got, "\n"))
			}
			if strings.Contains(r.readsEverywhere(), e) {
				t.Fatalf("%s: a read returns the erased id %s", w, e)
			}
		}
		if n := r.foldBroken(); n != 0 {
			t.Fatalf("%s: fold invariant broken (%d)", w, n)
		}
		r.noMismatch()
	}
	// refused checks the ban: nothing about or by an erased member is accepted
	// again, at any time, and a refusal changes nothing.
	refused := func(step string) {
		t.Helper()
		before, snaps := r.rowsWhere(`true`), r.count("snapshot")
		for e := range m.erased {
			for _, at := range []time.Time{T0.Add(-1000 * time.Hour), T0.Add(-time.Minute)} {
				s := snap(900000+rnd.Intn(1000000), e, "c9", at, true, rec("late", e, `{"title": "x"}`))
				out, err := r.applyErr(s)
				if err != nil || out.Rejection == nil || out.Rejection.Status != http.StatusGone {
					t.Fatalf("%s: a snapshot about erased %s answered %+v %v", where(step), e, out, err)
				}
			}
			live := m.live()
			day++
			date := fmt.Sprintf("2026-07-%02d", day%28+1)
			cases := []struct{ author, subject string }{{e, e}}
			if len(live) > 0 {
				other := live[rnd.Intn(len(live))]
				cases = append(cases, struct{ author, subject string }{other, e}, struct{ author, subject string }{e, other})
			}
			for _, c := range cases {
				rej, err := r.tryNote("attendance_note", c.author, c.subject, date, "late")
				if err != nil || rej == nil || rej.Status != http.StatusGone {
					t.Fatalf("%s: a note by %s about %s answered %+v %v", where(step), c.author, c.subject, rej, err)
				}
				ex["refused-410"]++
			}
		}
		if after := r.rowsWhere(`true`); after != before || r.count("snapshot") != snaps {
			t.Fatalf("%s: a refused write changed the journal", where(step))
		}
	}
	// offboard takes a living member off the roster without erasing them
	// (gitops removes a person): they read nothing, their rows stay, and they
	// are no frozen reader of anything erased later.
	offboard := func(step string) {
		t.Helper()
		live := m.live()
		if len(live) < 3 || rnd.Intn(2) == 0 {
			return
		}
		x := live[rnd.Intn(len(live))]
		m.d = m.d.without(x)
		r.setRoster(m.d.raw())
		ex["offboarded"]++
		verify(step + ", " + x + " offboarded")
	}
	verify("before any erase")
	offboard("before any erase")

	erasures := 1 + rnd.Intn(3)
	if erasures > n-1 {
		erasures = n - 1
	}
	order := rnd.Perm(n)
	for i := 0; i < erasures; i++ {
		e := everyone[order[i]]
		step := fmt.Sprintf("erase %d (%s)", i+1, e)
		wantKept, wantAuthored := m.erase(e, ex)
		if wantKept > 0 {
			ex["notes-kept"] += wantKept
		}
		if wantAuthored > 0 {
			ex["notes-deleted-as-author"]++
		}
		resp, err := r.j.Erase(ctx, "admin", e)
		if err != nil {
			t.Fatalf("%s: %v", where(step), err)
		}
		if int(resp.NotesPseudonymised) != wantKept || int(resp.NotesDeletedAsAuthor) != wantAuthored {
			t.Fatalf("%s: erase reported %d kept and %d deleted as author, the model %d and %d", where(step),
				resp.NotesPseudonymised, resp.NotesDeletedAsAuthor, wantKept, wantAuthored)
		}
		verify(step)
		refused(step)
		if rnd.Intn(2) == 0 {
			// gitops removes the member from the roster after the erase
			m.d = m.d.without(e)
			r.setRoster(m.d.raw())
			ex["erase-with-roster-removal"]++
			verify(step + " and off the roster")
			refused(step + " and off the roster")
		} else {
			ex["erase-on-roster"]++
		}
		// life goes on: more notes among the living, then the next erase
		writeNotes(3 + rnd.Intn(5))
		offboard(step)
		verify(step + ", then more notes")
	}
}
