package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/access"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

type genPerson struct {
	Agent      string   `json:"agent"`
	GuardianOf []string `json:"guardian_of,omitempty"`
	Siblings   []string `json:"siblings,omitempty"`
}

type genRoster struct {
	People           map[string]genPerson `json:"people"`
	HouseholdMembers []string             `json:"household_members"`
}

func randomRoster(r *rand.Rand, prefix string) genRoster {
	return randomRosterN(r, prefix, 2+r.Intn(4))
}

// randomRosterN draws n members with random, asymmetric guardian and
// sibling relations; at least the first member is in the household.
func randomRosterN(r *rand.Rand, prefix string, n int) genRoster {
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("%s%d", prefix, i))
	}
	d := genRoster{People: map[string]genPerson{}}
	for i, id := range ids {
		p := genPerson{Agent: "agent-" + id}
		for _, o := range ids {
			if o == id {
				continue
			}
			if r.Intn(3) == 0 {
				p.GuardianOf = append(p.GuardianOf, o)
			}
			if r.Intn(3) == 0 {
				p.Siblings = append(p.Siblings, o)
			}
		}
		d.People[id] = p
		if i == 0 || r.Intn(2) == 0 {
			d.HouseholdMembers = append(d.HouseholdMembers, id)
		}
	}
	return d
}

func (d genRoster) raw() string {
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (d genRoster) parse(t testing.TB) *roster.Roster {
	t.Helper()
	ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(d.raw()))
	if err != nil {
		t.Fatal(err)
	}
	return ros
}

// without is d with the member and every reference to them removed.
func (d genRoster) without(id string) genRoster {
	out := genRoster{People: map[string]genPerson{}}
	drop := func(xs []string) []string {
		var r []string
		for _, x := range xs {
			if x != id {
				r = append(r, x)
			}
		}
		return r
	}
	for m, p := range d.People {
		if m == id {
			continue
		}
		p.GuardianOf, p.Siblings = drop(p.GuardianOf), drop(p.Siblings)
		out.People[m] = p
	}
	out.HouseholdMembers = drop(d.HouseholdMembers)
	return out
}

func (d genRoster) members() []string {
	var out []string
	for m := range d.People {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// entitledBy is spec 11's semantics written from the relationships, not from
// the roster's computed grants. subject "" is the household as a whole: its
// guardians token goes to anyone who guards somebody, its household token to
// household members. It is for rows that are not kept notes.
func entitledBy(d genRoster, reader, subject string, audience []string) bool {
	p := d.People[reader]
	for _, tok := range audience {
		switch tok {
		case "public":
			return true
		case "subject":
			if subject != "" && subject == reader {
				return true
			}
		case "guardians":
			if subject == "" {
				if len(p.GuardianOf) > 0 {
					return true
				}
			} else if slices.Contains(p.GuardianOf, subject) {
				return true
			}
		case "siblings":
			if subject != "" && slices.Contains(p.Siblings, subject) {
				return true
			}
		case "household":
			if slices.Contains(d.HouseholdMembers, reader) && (subject == "" || slices.Contains(d.HouseholdMembers, subject)) {
				return true
			}
		}
	}
	return false
}

var audienceChoices = [][]string{
	{"subject"}, {"guardians"}, {"siblings"}, {"household"}, {"public"}, {"caregivers"},
	{"subject", "guardians"}, {"guardians", "siblings"}, {"household", "subject"},
}

func sortedAud(a []string) []string { b := slices.Clone(a); slices.Sort(b); return slices.Compact(b) }

// visibleByGo is the set of "type/subject/key" of current rows and the seqs
// of events that access.Visible, applied by the test to the raw tables with
// the reader's grants (frozen-reader grants included), allows. It does not
// go through State or Events, so the SQL filter of those is compared with
// the Go rule on every row of the database, kept notes included.
func (r *rig) visibleByGo(reader string) (rows map[string]bool, seqs map[int64]bool, refused bool) {
	r.t.Helper()
	grants, _, err := r.j.grantsOf(ctx, reader)
	if errors.Is(err, ErrUnknownReader) {
		return nil, nil, true
	}
	if err != nil {
		r.t.Fatal(err)
	}
	rows, seqs = map[string]bool{}, map[int64]bool{}
	cur, err := r.j.db.Query(`SELECT record_type, data_subject, key, to_jsonb(audience) FROM record_current`)
	if err != nil {
		r.t.Fatal(err)
	}
	for cur.Next() {
		var typ, subj, key string
		var aud []byte
		if err := cur.Scan(&typ, &subj, &key, &aud); err != nil {
			r.t.Fatal(err)
		}
		var a []string
		_ = json.Unmarshal(aud, &a)
		if access.Visible(grants, subj, a) {
			rows[typ+"/"+subj+"/"+key] = true
		}
	}
	cur.Close()
	ev, err := r.j.db.Query(`SELECT seq, data_subject, to_jsonb(audience) FROM record_event`)
	if err != nil {
		r.t.Fatal(err)
	}
	for ev.Next() {
		var seq int64
		var subj string
		var aud []byte
		if err := ev.Scan(&seq, &subj, &aud); err != nil {
			r.t.Fatal(err)
		}
		var a []string
		_ = json.Unmarshal(aud, &a)
		if access.Visible(grants, subj, a) {
			seqs[seq] = true
		}
	}
	ev.Close()
	return rows, seqs, false
}

// assertSQLGoAgree fails when State or Events returns a row the Go rule
// denies, or omits one it allows. A reader the journal refuses must be
// refused by all three reads.
func (r *rig) assertSQLGoAgree(reader string) {
	r.t.Helper()
	rows, seqs, refused := r.visibleByGo(reader)
	if refused {
		if !r.readRefused(reader) {
			r.t.Fatalf("%s: refused by grantsOf but a read answered", reader)
		}
		return
	}
	st, err := r.j.State(ctx, StateQuery{Reader: reader, IncludeRemoved: true})
	if err != nil {
		r.t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range st {
		got[s.RecordType+"/"+s.DataSubject+"/"+s.Key] = true
	}
	if !maps_equal(got, rows) {
		r.t.Fatalf("%s: State and the Go rule disagree:\n state only: %v\n rule only:  %v", reader, minus(got, rows), minus(rows, got))
	}
	ev, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: MaxEventsLimit})
	if err != nil {
		r.t.Fatal(err)
	}
	gotSeq := map[int64]bool{}
	for _, e := range ev {
		gotSeq[e.Seq] = true
	}
	if len(gotSeq) != len(seqs) {
		r.t.Fatalf("%s: Events returned %d events, the Go rule allows %d", reader, len(gotSeq), len(seqs))
	}
	for s := range seqs {
		if !gotSeq[s] {
			r.t.Fatalf("%s: Events omitted seq %d, which the Go rule allows", reader, s)
		}
	}
}

func maps_equal(a, b map[string]bool) bool { return len(minus(a, b)) == 0 && len(minus(b, a)) == 0 }

func minus(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// pubRec is what the visibility test wrote for one record.
type pubRec struct {
	subject, key           string
	audFirst, audNow       []string
	commentFirst, comment  string // text of the comment field now and at creation
	titleFirst, titleNow   string
	updated                bool // an update event exists
	commentChanged         bool
	audienceChangedInEvent bool
}

// commentOf is what a reader may see of comment text after the scan.
func shown(text string) string {
	if strings.Contains(text, "INJECT") {
		return string(record.Encode(access.WithheldScan))
	}
	return string(record.Encode(text))
}

// runVisibility writes a random journal for roster d (every member, three
// records each, one of which changes audience and values) and checks every
// reader's State, Events and Counts against the oracle. idBase keeps the
// snapshot ids of one call apart from another's on a shared rig; subjects
// the filter prefix does not match are rows of other calls.
func runVisibility(t *testing.T, r *rig, d genRoster, rnd *rand.Rand, prefix string, idBase int, seed int64) {
	t.Helper()
	ros := d.parse(t)
	r.ros.Store(ros)
	var written []*pubRec
	id := idBase
	pick := func() []string { return audienceChoices[rnd.Intn(len(audienceChoices))] }
	texts := []string{"fine", "INJECT-one", "also fine", "INJECT-two"}
	for _, subject := range ros.Members() {
		var recs []journalapi.Record
		var mine []*pubRec
		for k := 0; k < 3; k++ {
			pr := &pubRec{subject: subject, key: fmt.Sprintf("k%d", k), audFirst: pick(), titleFirst: "v1", titleNow: "v1",
				commentFirst: texts[rnd.Intn(len(texts))]}
			pr.audNow, pr.comment = pr.audFirst, pr.commentFirst
			rc := rec(pr.key, subject, fmt.Sprintf(`{"title": "v1", "comment": %q}`, pr.commentFirst))
			rc.Audience = pr.audFirst
			recs = append(recs, rc)
			mine = append(mine, pr)
		}
		r.apply(snap(id, subject, "c1", T0.Add(-2*time.Hour), true, recs...))
		id++
		// k0 changes its audience (maybe to the same one), title and comment.
		pr := mine[0]
		pr.audNow, pr.titleNow, pr.comment = pick(), "v2", texts[rnd.Intn(len(texts))]
		pr.updated = true
		pr.commentChanged = pr.comment != pr.commentFirst
		pr.audienceChangedInEvent = !slices.Equal(sortedAud(pr.audFirst), sortedAud(pr.audNow))
		changed := rec(pr.key, subject, fmt.Sprintf(`{"title": "v2", "comment": %q}`, pr.comment))
		changed.Audience = pr.audNow
		// The update comes from another course's list, so the record also moves
		// scope: the previous scope and observation time are in the event.
		r.apply(snap(id, subject, "c2", T0.Add(-time.Hour), false, changed))
		id++
		written = append(written, mine...)
	}
	ours := func(subject string) bool { return strings.HasPrefix(subject, prefix) }
	for _, reader := range ros.Members() {
		where := fmt.Sprintf("seed %d reader %s", seed, reader)
		r.assertSQLGoAgree(reader)
		st, err := r.j.State(ctx, StateQuery{Reader: reader, Type: "assignment"})
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		gotState := map[string]journalapi.StateRow{}
		for _, s := range st {
			if !ours(s.DataSubject) {
				continue // public rows of other calls
			}
			gotState[s.DataSubject+"/"+s.Key] = s
			if !entitledBy(d, reader, s.DataSubject, s.Audience) {
				t.Fatalf("%s: read %s/%s with audience %v", where, s.DataSubject, s.Key, s.Audience)
			}
		}
		live := 0
		for _, w := range written {
			want := entitledBy(d, reader, w.subject, sortedAud(w.audNow))
			s, ok := gotState[w.subject+"/"+w.key]
			if want != ok {
				t.Fatalf("%s: entitled=%v to %s/%s (%v), returned=%v", where, want, w.subject, w.key, w.audNow, ok)
			}
			if !ok {
				continue
			}
			live++
			if g, e := string(s.Fields["comment"]), shown(w.comment); g != e {
				t.Fatalf("%s: %s/%s comment %s, want %s", where, w.subject, w.key, g, e)
			}
			if g := string(s.Fields["title"]); g != string(record.Encode(w.titleNow)) {
				t.Fatalf("%s: %s/%s title %s", where, w.subject, w.key, g)
			}
		}
		cs, err := r.j.Counts(ctx, reader, nil, T0.Add(-1000*time.Hour))
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		liveCounted := 0
		for _, c := range cs {
			if ours(c.DataSubject) {
				liveCounted += c.Live
			}
		}
		if liveCounted != live {
			t.Fatalf("%s: counts say %d live rows, the oracle %d", where, liveCounted, live)
		}
		events, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 1000})
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		type evKey struct{ subject, key, kind string }
		gotEv := map[evKey]journalapi.Event{}
		for _, e := range events {
			if !ours(e.DataSubject) {
				continue
			}
			if !entitledBy(d, reader, e.DataSubject, e.Audience) {
				t.Fatalf("%s: got event %d about %s with audience %v", where, e.Seq, e.DataSubject, e.Audience)
			}
			gotEv[evKey{e.DataSubject, e.Key, string(e.Kind)}] = e
		}
		wantEv := 0
		for _, w := range written {
			if entitledBy(d, reader, w.subject, sortedAud(w.audFirst)) {
				wantEv++
				if _, ok := gotEv[evKey{w.subject, w.key, "created"}]; !ok {
					t.Fatalf("%s: entitled to the created event of %s/%s (%v) and did not get it", where, w.subject, w.key, w.audFirst)
				}
			}
			if !w.updated || !entitledBy(d, reader, w.subject, sortedAud(w.audNow)) {
				continue
			}
			wantEv++
			e, ok := gotEv[evKey{w.subject, w.key, "updated"}]
			if !ok {
				t.Fatalf("%s: entitled to the update event of %s/%s (%v) and did not get it", where, w.subject, w.key, w.audNow)
			}
			// An old value across an audience change reaches only a reader of
			// the old audience as well; otherwise it is the old value, masked
			// by its scan verdict.
			oldTitle, oldComment := string(record.Encode(w.titleFirst)), shown(w.commentFirst)
			if w.audienceChangedInEvent && !entitledBy(d, reader, w.subject, sortedAud(w.audFirst)) {
				oldTitle, oldComment = string(record.Encode(access.WithheldAudience)), string(record.Encode(access.WithheldAudience))
			}
			if g := string(e.Changed["title"].Old); g != oldTitle {
				t.Fatalf("%s: old title of %s/%s is %s, want %s (audience %v -> %v)", where, w.subject, w.key, g, oldTitle, w.audFirst, w.audNow)
			}
			if g := string(e.Changed["title"].New); g != string(record.Encode(w.titleNow)) {
				t.Fatalf("%s: new title of %s/%s is %s", where, w.subject, w.key, g)
			}
			if w.commentChanged {
				if g := string(e.Changed["comment"].Old); g != oldComment {
					t.Fatalf("%s: old comment of %s/%s is %s, want %s", where, w.subject, w.key, g, oldComment)
				}
				if g := string(e.Changed["comment"].New); g != shown(w.comment) {
					t.Fatalf("%s: new comment of %s/%s is %s, want %s", where, w.subject, w.key, g, shown(w.comment))
				}
			} else if _, ok := e.Changed["comment"]; ok {
				t.Fatalf("%s: %s/%s: an unchanged comment is in the event", where, w.subject, w.key)
			}
			if e.Scope != scopeKey(w.subject, "c2") {
				t.Fatalf("%s: %s/%s: scope %q", where, w.subject, w.key, e.Scope)
			}
			if w.audienceChangedInEvent && !entitledBy(d, reader, w.subject, sortedAud(w.audFirst)) {
				// The previous scope and observation time belong to the old audience.
				if e.PrevScope != "" || e.PrevObservedAt != nil {
					t.Fatalf("%s: %s/%s: the previous scope or time reached a reader outside the old audience", where, w.subject, w.key)
				}
			} else if e.PrevScope != scopeKey(w.subject, "c1") || e.PrevObservedAt == nil {
				t.Fatalf("%s: %s/%s: previous scope %q, time %v", where, w.subject, w.key, e.PrevScope, e.PrevObservedAt)
			}
		}
		n := 0
		for k := range gotEv {
			if ours(k.subject) {
				n++
			}
		}
		if n != wantEv {
			t.Fatalf("%s: %d events of this roster returned, %d expected", where, n, wantEv)
		}
		// Withheld text never leaves the journal, whatever the route.
		for _, v := range []any{st, events, cs} {
			if b, _ := json.Marshal(v); strings.Contains(string(b), "INJECT") {
				t.Fatalf("%s: withheld text in a read: %s", where, b)
			}
		}
	}
	r.noMismatch()
}

func TestPropertyReadsReturnExactlyWhatTheRosterEntitles(t *testing.T) {
	const seeds = 25
	r := newRig(t)
	for seed := int64(0); seed < seeds; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		prefix := fmt.Sprintf("e_r%02d_", seed)
		runVisibility(t, r, randomRoster(rnd, prefix), rnd, prefix, int(seed)*1000+1, seed)
	}
}

// The roster of read_review_test.go: e_guard1 guards e_child1 only, and
// e_outsider1 is on the roster with no relationship. A symmetric or
// all-guarding roster cannot show a leak between children, so the random
// audiences run over this roster too.
func TestPropertyReadsOverAnAsymmetricRoster(t *testing.T) {
	var d genRoster
	if err := json.Unmarshal([]byte(asymHousehold), &d); err != nil {
		t.Fatal(err)
	}
	for seed := int64(100); seed < 104; seed++ {
		r := newRig(t)
		runVisibility(t, r, d, rand.New(rand.NewSource(seed)), "e_", 1, seed)
		// e_guard1 never reads e_child2's guardians rows, whatever the draw.
		st, err := r.j.State(ctx, StateQuery{Reader: "e_guard1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range st {
			if s.DataSubject == "e_child2" && slices.Contains(s.Audience, "guardians") && !slices.Contains(s.Audience, "public") &&
				!slices.Contains(s.Audience, "household") {
				t.Fatalf("seed %d: a guardian of e_child1 read %s/%s with audience %v", seed, s.DataSubject, s.Key, s.Audience)
			}
		}
		// The member in no relationship, outside the household, reads public
		// rows and her own subject rows only.
		out, err := r.j.State(ctx, StateQuery{Reader: "e_outsider1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range out {
			if !slices.Contains(s.Audience, "public") && s.DataSubject != "e_outsider1" {
				t.Fatalf("seed %d: the outsider read %s/%s with audience %v", seed, s.DataSubject, s.Key, s.Audience)
			}
		}
	}
}
