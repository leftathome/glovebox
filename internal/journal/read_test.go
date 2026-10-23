package journal

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

func strp(s string) *string { return &s }

const withheldScan = `"withheld: failed content scan"`
const withheldAudience = `"withheld: different audience"`

// stateKeys is the sorted list of "subject/key" a reader sees in /v1/state.
func (r *rig) stateKeys(reader string, q StateQuery) []string {
	r.t.Helper()
	q.Reader = reader
	rows, err := r.j.State(ctx, q)
	if err != nil {
		r.t.Fatalf("state as %s: %v", reader, err)
	}
	out := []string{}
	for _, s := range rows {
		out = append(out, s.DataSubject+"/"+s.Key)
	}
	sort.Strings(out)
	return out
}

func (r *rig) eventKeys(reader string) []string {
	r.t.Helper()
	evs, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 1000})
	if err != nil {
		r.t.Fatalf("events as %s: %v", reader, err)
	}
	out := []string{}
	for _, e := range evs {
		out = append(out, e.DataSubject+"/"+e.Key)
	}
	sort.Strings(out)
	return out
}

func (r *rig) liveCount(reader string) int {
	r.t.Helper()
	cs, err := r.j.Counts(ctx, reader, nil, T0.Add(-240*time.Hour))
	if err != nil {
		r.t.Fatalf("counts as %s: %v", reader, err)
	}
	n := 0
	for _, c := range cs {
		n += c.Live
	}
	return n
}

func same(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func attendanceSnap(n int, observed time.Time, recs ...journalapi.Record) journalapi.Snapshot {
	return journalapi.Snapshot{SnapshotID: uid(n), RecordType: "attendance_day", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "attendance_days"}, {"data_subject", "e_child1"}, {"school_year", "2026-27"}},
		Complete: false, ObservedAt: observed, Records: recs}
}

func TestStateIsFilteredByTheReadersGrants(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child2", `{"title": "B"}`)))
	gonly := rec("c1:2", "e_child1", `{"title": "guardians only"}`)
	gonly.Audience = []string{"guardians"}
	r.apply(snap(3, "e_child1", "c2", T0.Add(-time.Hour), true, gonly))
	count := func(member string) int { return len(r.stateKeys(member, StateQuery{})) }
	if got := count("e_parent1"); got != 3 {
		t.Errorf("guardian sees %d rows", got)
	}
	if got := count("e_parent2"); got != 3 {
		t.Errorf("second guardian sees %d rows", got)
	}
	if got := r.stateKeys("e_child1", StateQuery{}); !same(got, []string{"e_child1/c1:1"}) {
		t.Errorf("child1 sees %v: its own, not the sibling's, not the guardians-only one", got)
	}
	if got := r.stateKeys("e_child2", StateQuery{}); !same(got, []string{"e_child2/c1:1"}) {
		t.Errorf("child2 sees %v", got)
	}
	if got := count("e_guest1"); got != 0 {
		t.Errorf("guest sees %d", got)
	}
	rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1", Subject: strp("e_child2")})
	if len(rows) != 1 || rows[0].DataSubject != "e_child2" {
		t.Errorf("subject filter %+v", rows)
	}
	// A subject filter never widens: a child asking for the sibling gets nothing.
	if got := r.stateKeys("e_child1", StateQuery{Subject: strp("e_child2")}); len(got) != 0 {
		t.Errorf("child1 asking for child2: %v", got)
	}
	if got := r.stateKeys("e_parent1", StateQuery{Subject: strp("e_nobody")}); len(got) != 0 {
		t.Errorf("unknown subject: %v", got)
	}
	r.noMismatch()
}

// Every audience token against every reader, in state, events and counts.
func TestAudienceGrantsAcrossEveryReader(t *testing.T) {
	r := newRig(t)
	audiences := map[string][]string{
		"k_guardians": {"guardians"}, "k_subject": {"subject"}, "k_household": {"household"},
		"k_siblings": {"siblings"}, "k_public": {"public"}, "k_unknown": {"opaque_token"},
		"k_two": {"guardians", "siblings"},
	}
	keys := make([]string, 0, len(audiences))
	for k := range audiences {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, key := range keys {
		x := rec(key, "e_child1", `{"title": "T"}`)
		x.Audience = audiences[key]
		r.apply(snap(i+1, "e_child1", "s"+key, T0.Add(-time.Hour), true, x))
	}
	// A second subject proves the subject half of the grant: child2 holds
	// "subject" for itself only.
	x := rec("k_subject", "e_child2", `{"title": "T"}`)
	x.Audience = []string{"subject"}
	r.apply(snap(100, "e_child2", "sk", T0.Add(-time.Hour), true, x))

	want := map[string][]string{
		"e_parent1": {"e_child1/k_guardians", "e_child1/k_household", "e_child1/k_public", "e_child1/k_two"},
		"e_parent2": {"e_child1/k_guardians", "e_child1/k_household", "e_child1/k_public", "e_child1/k_two"},
		"e_child1":  {"e_child1/k_household", "e_child1/k_public", "e_child1/k_subject"},
		"e_child2":  {"e_child1/k_household", "e_child1/k_public", "e_child1/k_siblings", "e_child1/k_two", "e_child2/k_subject"},
		"e_guest1":  {"e_child1/k_public"},
	}
	for reader, keys := range want {
		got := r.stateKeys(reader, StateQuery{})
		if !same(got, keys) {
			t.Errorf("state as %s:\n got %v\nwant %v", reader, got, keys)
		}
		if got := r.eventKeys(reader); !same(got, keys) {
			t.Errorf("events as %s:\n got %v\nwant %v", reader, got, keys)
		}
		if got := r.liveCount(reader); got != len(keys) {
			t.Errorf("counts as %s: %d live, want %d", reader, got, len(keys))
		}
	}
	r.noMismatch()
}

func TestReadersOutsideTheRosterSeeNothing(t *testing.T) {
	r := newRig(t)
	pub := rec("k", "e_child1", `{"title": "A"}`)
	pub.Audience = []string{"public"}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, pub))
	check := func(label string, want error) {
		t.Helper()
		for _, who := range []string{"e_stranger", "", "E_PARENT1", " e_parent1"} {
			rows, err := r.j.State(ctx, StateQuery{Reader: who})
			if !errors.Is(err, want) || len(rows) != 0 || rows == nil {
				t.Errorf("%s: state as %q: %v %v", label, who, rows, err)
			}
			evs, err := r.j.Events(ctx, EventsQuery{Reader: who, Limit: 10})
			if !errors.Is(err, want) || len(evs) != 0 || evs == nil {
				t.Errorf("%s: events as %q: %v %v", label, who, evs, err)
			}
			cs, err := r.j.Counts(ctx, who, nil, T0.Add(-time.Hour*24))
			if !errors.Is(err, want) || len(cs) != 0 || cs == nil {
				t.Errorf("%s: counts as %q: %v %v", label, who, cs, err)
			}
		}
	}
	check("roster loaded", ErrUnknownReader)
	// A public row is visible to members only; the guest is a member.
	if got := r.stateKeys("e_guest1", StateQuery{}); len(got) != 1 {
		t.Errorf("a member sees public rows: %v", got)
	}
	r.j.o.Roster = func() *roster.Roster { return nil }
	check("nil roster", ErrNoRoster)
	for _, who := range []string{"e_parent1", "e_child1"} {
		if _, err := r.j.State(ctx, StateQuery{Reader: who}); !errors.Is(err, ErrNoRoster) {
			t.Errorf("nil roster, member %s: %v", who, err)
		}
	}
}

func TestStateRowObservationFieldsAndMasking(t *testing.T) {
	r := newRig(t)
	t1, t2 := T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	r.apply(snap(1, "e_child1", "c1", t1, false, rec("c1:1", "e_child1", `{"title": "A", "comment": "INJECT"}`), rec("c1:2", "e_child1", `{"title": "B"}`)))
	r.apply(snap(2, "e_child1", "c1", t2, false, rec("c1:1", "e_child1", `{"title": "A", "comment": "INJECT"}`)))
	rows, err := r.j.State(ctx, StateQuery{Reader: "e_parent1", Type: "assignment"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	a, b := rows[0], rows[1]
	if a.Key != "c1:1" || b.Key != "c1:2" {
		t.Fatalf("order %s %s", a.Key, b.Key)
	}
	if string(a.Fields["comment"]) != withheldScan || string(a.Fields["title"]) != `"A"` {
		t.Errorf("masking %s", a.Fields)
	}
	if !a.ConfirmedAt.Equal(t2) || a.InLatestSnapshot == nil || !*a.InLatestSnapshot || !a.FirstSeenAt.Equal(t1) || !a.LastSeenAt.Equal(t2) {
		t.Errorf("row a %+v", a)
	}
	// c1:2 left the window: not removed, not in the latest snapshot.
	if b.Removed || b.InLatestSnapshot == nil || *b.InLatestSnapshot || !b.ConfirmedAt.Equal(t1) {
		t.Errorf("row b %+v", b)
	}
	if a.Scope != scopeKey("e_child1", "c1") || len(a.Audience) != 2 || a.Approximate {
		t.Errorf("row a identity %+v", a)
	}
}

func TestStateApproximateComesFromTheProducingEvent(t *testing.T) {
	r := newRig(t)
	s := snap(1, "e_child1", "c1", T0.Add(-time.Hour), false, rec("k", "e_child1", `{"title": "A"}`))
	s.Approximate = true
	r.apply(s)
	rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if len(rows) != 1 || !rows[0].Approximate {
		t.Errorf("seeded row: %+v", rows)
	}
	r.apply(snap(2, "e_child1", "c1", T0, false, rec("k", "e_child1", `{"title": "B"}`)))
	rows, _ = r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if len(rows) != 1 || rows[0].Approximate {
		t.Errorf("a later exact value is not approximate: %+v", rows)
	}
}

func TestStateHidesRemovedUnlessAsked(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("c1:1", "e_child1", `{"title": "A", "comment": "INJECT"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true))
	if rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1"}); len(rows) != 0 || rows == nil {
		t.Errorf("a removed row was returned: %+v", rows)
	}
	rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1", IncludeRemoved: true})
	if len(rows) != 1 || !rows[0].Removed || rows[0].InLatestSnapshot == nil || *rows[0].InLatestSnapshot {
		t.Fatalf("%+v", rows)
	}
	if string(rows[0].Fields["comment"]) != withheldScan {
		t.Errorf("a removed row's text stays masked: %s", rows[0].Fields)
	}
	// Removal does not widen visibility.
	if got := r.stateKeys("e_child2", StateQuery{IncludeRemoved: true}); len(got) != 0 {
		t.Errorf("child2 sees a removed row of child1: %v", got)
	}
	if r.liveCount("e_parent1") != 0 {
		t.Error("a removed row counts as live")
	}
}

func TestStateTypeAndScopeFilters(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("a", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child1", "c2", T0.Add(-time.Hour), true, rec("b", "e_child1", `{"title": "B"}`)))
	r.apply(attendanceSnap(3, T0.Add(-time.Hour),
		rec("2026-10-14", "e_child1", `{"date": "2026-10-14", "classification": "full_day", "any_unexcused": true}`)))
	if got := r.stateKeys("e_parent1", StateQuery{}); len(got) != 3 {
		t.Errorf("all: %v", got)
	}
	if got := r.stateKeys("e_parent1", StateQuery{Type: "assignment"}); !same(got, []string{"e_child1/a", "e_child1/b"}) {
		t.Errorf("type: %v", got)
	}
	if got := r.stateKeys("e_parent1", StateQuery{Scope: scopeKey("e_child1", "c2")}); !same(got, []string{"e_child1/b"}) {
		t.Errorf("scope: %v", got)
	}
	if got := r.stateKeys("e_parent1", StateQuery{Type: "attendance_day", Scope: scopeKey("e_child1", "c2")}); len(got) != 0 {
		t.Errorf("type and scope: %v", got)
	}
}

func TestEventsCursorFiltersAndImportance(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("c1:1", "e_child1", `{"title": "A"}`)))
	r.apply(attendanceSnap(2, T0.Add(-time.Hour),
		rec("2026-10-14", "e_child1", `{"date": "2026-10-14", "classification": "full_day", "any_unexcused": true}`)))
	all, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if err != nil || len(all) != 2 || all[0].Seq >= all[1].Seq {
		t.Fatalf("%+v %v", all, err)
	}
	high, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", MinImportance: "high", Limit: 10})
	if len(high) != 1 || high[0].Rule != "full_day_absence" || high[0].GroupKey == "" || high[0].Importance != "high" {
		t.Fatalf("high %+v", high)
	}
	if len(high[0].Notify) != 1 || high[0].Notify[0].Token != "guardians" || high[0].Notify[0].Mode != "push" {
		t.Errorf("a guardian is advised only through guardians: %+v", high[0].Notify)
	}
	child, _ := r.j.Events(ctx, EventsQuery{Reader: "e_child1", MinImportance: "high", Limit: 10})
	if len(child) != 1 || len(child[0].Notify) != 1 || child[0].Notify[0].Purpose != "attendance_followup" || child[0].Notify[0].Token != "subject" {
		t.Errorf("the subject gets the prompt: %+v", child)
	}
	normal, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", MinImportance: "normal", Limit: 10})
	if len(normal) != 1 {
		t.Errorf("normal includes high but not low: %+v", normal)
	}
	after, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", After: all[0].Seq, Limit: 10})
	if len(after) != 1 || after[0].Seq != all[1].Seq {
		t.Errorf("cursor %+v", after)
	}
	typed, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Type: "assignment", Limit: 10})
	if len(typed) != 1 || typed[0].RecordType != "assignment" {
		t.Errorf("type filter %+v", typed)
	}
	one, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 1})
	if len(one) != 1 || one[0].Seq != all[0].Seq {
		t.Errorf("limit %+v", one)
	}
	subj, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Subject: strp("e_child2"), Limit: 10})
	if len(subj) != 0 || subj == nil {
		t.Errorf("subject filter %+v", subj)
	}
	subj, _ = r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Subject: strp("e_child1"), Limit: 10})
	if len(subj) != 2 {
		t.Errorf("subject filter %+v", subj)
	}
	other, _ := r.j.Events(ctx, EventsQuery{Reader: "e_child2", Limit: 10})
	if len(other) != 0 {
		t.Errorf("child2 sees %+v", other)
	}
}

func TestEventsRefuseBadBounds(t *testing.T) {
	r := newRig(t)
	for _, q := range []EventsQuery{
		{Reader: "e_parent1", Limit: 0}, {Reader: "e_parent1", Limit: -1},
		{Reader: "e_parent1", Limit: 5, After: -1},
		{Reader: "e_parent1", Limit: 5, MinImportance: "urgent"},
	} {
		if evs, err := r.j.Events(ctx, q); !errors.Is(err, ErrBadQuery) || len(evs) != 0 {
			t.Errorf("%+v: %v %v", q, evs, err)
		}
	}
	if _, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 1}); err != nil {
		t.Errorf("limit 1: %v", err)
	}
	if _, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10000, MinImportance: "normal"}); err != nil {
		t.Errorf("large limit: %v", err)
	}
}

func TestEventsPagingVisitsEverythingOnceAscendingAcrossHiddenGaps(t *testing.T) {
	r := newRig(t)
	// Interleave events child1 may see with events only guardians may see.
	n := 0
	for i := 0; i < 5; i++ {
		x := rec("v", "e_child1", `{"title": "x`+string(rune('a'+i))+`"}`)
		g := rec("g", "e_child1", `{"title": "y`+string(rune('a'+i))+`"}`)
		g.Audience = []string{"guardians"}
		at := T0.Add(time.Duration(i-10) * time.Hour)
		n++
		r.apply(snap(n, "e_child1", "cv", at, false, x))
		n++
		r.apply(snap(n, "e_child1", "cg", at, false, g))
	}
	for _, reader := range []string{"e_child1", "e_parent1"} {
		whole, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 1000})
		if err != nil || len(whole) == 0 {
			t.Fatalf("%s: %v %v", reader, whole, err)
		}
		for i := 1; i < len(whole); i++ {
			if whole[i].Seq <= whole[i-1].Seq {
				t.Fatalf("%s: not ascending at %d: %d then %d", reader, i, whole[i-1].Seq, whole[i].Seq)
			}
		}
		for _, size := range []int{1, 2, 3} {
			var got []int64
			cursor := int64(0)
			for guard := 0; guard < 100; guard++ {
				page, err := r.j.Events(ctx, EventsQuery{Reader: reader, After: cursor, Limit: size})
				if err != nil {
					t.Fatal(err)
				}
				if len(page) > size {
					t.Fatalf("%s: page of %d over limit %d", reader, len(page), size)
				}
				if len(page) == 0 {
					break
				}
				for _, e := range page {
					got = append(got, e.Seq)
				}
				cursor = page[len(page)-1].Seq
			}
			if len(got) != len(whole) {
				t.Errorf("%s size %d: paged %d events, whole is %d", reader, size, len(got), len(whole))
				continue
			}
			for i := range got {
				if got[i] != whole[i].Seq {
					t.Errorf("%s size %d: position %d got %d want %d", reader, size, i, got[i], whole[i].Seq)
				}
			}
		}
		// A cursor at or past the newest visible seq yields an empty list, not null.
		last := whole[len(whole)-1].Seq
		for _, after := range []int64{last, last + 1000} {
			page, err := r.j.Events(ctx, EventsQuery{Reader: reader, After: after, Limit: 5})
			if err != nil || page == nil || len(page) != 0 {
				t.Errorf("%s after %d: %v %v", reader, after, page, err)
			}
		}
	}
	// child1 sees exactly the events of "v"; a guardian sees both keys.
	c, _ := r.j.Events(ctx, EventsQuery{Reader: "e_child1", Limit: 1000})
	p, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 1000})
	if len(c) != 5 || len(p) != 10 {
		t.Errorf("child sees %d, guardian %d", len(c), len(p))
	}
	for _, e := range c {
		if e.Key != "v" {
			t.Errorf("child sees %s", e.Key)
		}
	}
	r.noMismatch()
}

// 4.8: a value from the old audience reaches only readers of both.
func TestEventsWithholdOldValuesAcrossAnAudienceChange(t *testing.T) {
	r := newRig(t)
	g := rec("k", "e_child1", `{"title": "secret plan"}`)
	g.Audience = []string{"guardians"}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, g))
	open := rec("k", "e_child1", `{"title": "public plan"}`)
	open.Audience = []string{"guardians", "subject"}
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, open))
	child, _ := r.j.Events(ctx, EventsQuery{Reader: "e_child1", Limit: 10})
	if len(child) != 1 || child[0].Kind != "updated" {
		t.Fatalf("child sees %+v", child)
	}
	c := child[0].Changed["title"]
	if string(c.New) != `"public plan"` || string(c.Old) != withheldAudience {
		t.Errorf("child: %+v", c)
	}
	// The wire form carries no prev_audience.
	b, _ := json.Marshal(child[0])
	if strings.Contains(string(b), "prev_audience") || strings.Contains(string(b), "secret plan") {
		t.Errorf("leak in %s", b)
	}
	parent, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(parent) != 2 || string(parent[1].Changed["title"].Old) != `"secret plan"` {
		t.Errorf("parent: %+v", parent)
	}
	// The other direction: narrowing the audience hides the event from the
	// reader who loses access, from that event on.
	narrow := rec("k", "e_child1", `{"title": "back to secret"}`)
	narrow.Audience = []string{"guardians"}
	r.apply(snap(3, "e_child1", "c1", T0, true, narrow))
	child, _ = r.j.Events(ctx, EventsQuery{Reader: "e_child1", Limit: 10})
	if len(child) != 1 {
		t.Errorf("child sees the narrowing event: %+v", child)
	}
	parent, _ = r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(parent) != 3 || string(parent[2].Changed["title"].Old) != `"public plan"` {
		t.Errorf("parent after narrowing: %+v", parent)
	}
	// State shows the narrowed row to guardians only.
	if got := r.stateKeys("e_child1", StateQuery{}); len(got) != 0 {
		t.Errorf("child state %v", got)
	}
	r.noMismatch()
}

// An unchanged audience releases old values; a changed one is judged by the
// old audience, including an old audience that is empty.
func TestOldValuesFollowPrevAudienceExactly(t *testing.T) {
	r := newRig(t)
	ins := func(key, prev string) {
		t.Helper()
		if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope,
			audience, prev_audience, changed, observed_at, approximate, actor, importance)
			VALUES ('assignment', 'e_child1', $1, 'updated', 1, 's', '{guardians,subject}', `+prev+`,
			'{"title": ["old", "new"]}', now(), false, 'x', 'low')`, key); err != nil {
			t.Fatal(err)
		}
	}
	ins("unchanged", "NULL")
	ins("empty", "'{}'")
	ins("guardians", "'{guardians}'")
	ins("public", "'{public}'")
	old := func(reader string) map[string]string {
		evs, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, e := range evs {
			m[e.Key] = string(e.Changed["title"].Old)
		}
		return m
	}
	for reader, want := range map[string]map[string]string{
		"e_parent1": {"unchanged": `"old"`, "empty": withheldAudience, "guardians": `"old"`, "public": `"old"`},
		"e_child1":  {"unchanged": `"old"`, "empty": withheldAudience, "guardians": withheldAudience, "public": `"old"`},
	} {
		got := old(reader)
		if len(got) != 4 {
			t.Errorf("%s sees %v", reader, got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s %s: old %s, want %s", reader, k, got[k], v)
			}
		}
	}
}

// A field the update adds has no old value; a reader outside the old
// audience must not learn that from the shape.
func TestAddedFieldUnderAnAudienceChangeIsIndistinguishable(t *testing.T) {
	r := newRig(t)
	g := rec("k", "e_child1", `{"title": "A"}`)
	g.Audience = []string{"guardians"}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, g))
	open := rec("k", "e_child1", `{"title": "A", "score": 3}`)
	open.Audience = []string{"guardians", "subject"}
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, open))
	child, _ := r.j.Events(ctx, EventsQuery{Reader: "e_child1", Limit: 10})
	if len(child) != 1 {
		t.Fatalf("%+v", child)
	}
	if got := string(child[0].Changed["score"].Old); got != withheldAudience {
		t.Errorf("child learns the field was absent: %s", got)
	}
	if got := string(child[0].Changed["score"].New); got != "3" {
		t.Errorf("new value %s", got)
	}
	parent, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if got := parent[1].Changed["score"].Old; got != nil {
		t.Errorf("a guardian sees a value for an absent field: %s", got)
	}
}

func TestEventsMaskTextByEachValuesVerdict(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "fine"}`)))
	ev, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if string(ev[0].Fields["comment"]) != withheldScan {
		t.Errorf("created %s", ev[0].Fields)
	}
	if string(ev[0].Fields["title"]) != `"A"` {
		t.Errorf("a non-text value is released as is: %s", ev[0].Fields)
	}
	c := ev[1].Changed["comment"]
	if string(c.Old) != withheldScan || string(c.New) != `"fine"` {
		t.Errorf("an old failing value must stay withheld after the new one passes: %+v", c)
	}
	// And the reverse: a passing old value with a failing new one.
	r.apply(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT again"}`)))
	ev, _ = r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	c = ev[2].Changed["comment"]
	if string(c.Old) != `"fine"` || string(c.New) != withheldScan {
		t.Errorf("old passes, new fails: %+v", c)
	}
}

// 4.11: only "pass" releases text. Every other verdict, an absent one and
// an unknown one, withholds -- in state, in created, updated and removed
// events, for old and new values separately.
func TestOnlyAPassVerdictReleasesText(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "one"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "two"}`)))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true))
	setScan := func(kind, scan string) {
		t.Helper()
		if _, err := r.j.db.Exec(`UPDATE record_event SET scan = $1::jsonb WHERE kind = $2`, scan, kind); err != nil {
			t.Fatal(err)
		}
	}
	setCurrent := func(scan string) {
		t.Helper()
		if _, err := r.j.db.Exec(`UPDATE record_current SET scan = $1::jsonb`, scan); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name     string
		verdict  string
		released bool
		absent   bool
	}{
		{"pass", "pass", true, false},
		{"quarantine", "quarantine", false, false},
		{"reject", "reject", false, false},
		{"error", "error", false, false},
		{"empty", "", false, false},
		{"upper", "PASS", false, false},
		{"padded", "pass ", false, false},
		{"unknown", "maybe", false, false},
		{"absent", "", false, true},
	}
	for _, c := range cases {
		v := c.verdict
		cur := `{"comment": {"verdict": "` + v + `", "scanner_version": "s"}}`
		created := `{"comment": {"new": "` + v + `", "scanner_version": "s"}}`
		removed := `{"comment": {"old": "` + v + `", "scanner_version": "s"}}`
		oldSide := `{"comment": {"old": "` + v + `", "new": "pass", "scanner_version": "s"}}`
		newSide := `{"comment": {"old": "pass", "new": "` + v + `", "scanner_version": "s"}}`
		if c.absent {
			cur, created, removed = `{}`, `{}`, `{}`
			oldSide, newSide = `{"comment": {"new": "pass"}}`, `{"comment": {"old": "pass"}}`
		}
		check := func(label string, got json.RawMessage, released string) {
			t.Helper()
			want := withheldScan
			if c.released {
				want = released
			}
			if string(got) != want {
				t.Errorf("%s/%s: %s, want %s", c.name, label, got, want)
			}
		}
		setCurrent(cur)
		setScan("created", created)
		setScan("removed", removed)
		rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1", IncludeRemoved: true})
		if len(rows) != 1 {
			t.Fatalf("%+v", rows)
		}
		check("state", rows[0].Fields["comment"], `"two"`)
		if string(rows[0].Fields["title"]) != `"A"` {
			t.Errorf("%s: title %s", c.name, rows[0].Fields["title"])
		}
		evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
		if err != nil || len(evs) != 3 {
			t.Fatalf("%v %v", evs, err)
		}
		check("created", evs[0].Fields["comment"], `"one"`)
		check("removed", evs[2].LastFields["comment"], `"two"`)
		setScan("updated", oldSide)
		evs, _ = r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
		check("updated old", evs[1].Changed["comment"].Old, `"one"`)
		if string(evs[1].Changed["comment"].New) != `"two"` {
			t.Errorf("%s: the passing new value: %s", c.name, evs[1].Changed["comment"].New)
		}
		setScan("updated", newSide)
		evs, _ = r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
		check("updated new", evs[1].Changed["comment"].New, `"two"`)
		if string(evs[1].Changed["comment"].Old) != `"one"` {
			t.Errorf("%s: the passing old value: %s", c.name, evs[1].Changed["comment"].Old)
		}
	}
}

// Fail closed (4.11): a value that carries a verdict is masked by it even
// where the active type does not call the field text.
func TestValuesWithAVerdictAreMaskedWhateverTheActiveType(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	if _, err := r.j.db.Exec(`UPDATE record_current SET scan = '{"title": {"verdict": "quarantine", "scanner_version": "old"}}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`UPDATE record_event SET scan = '{"title": {"new": "quarantine", "scanner_version": "old"}}'`); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if len(rows) != 1 || string(rows[0].Fields["title"]) != withheldScan {
		t.Errorf("state %+v", rows)
	}
	ev, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(ev) != 1 || string(ev[0].Fields["title"]) != withheldScan {
		t.Errorf("events %+v", ev)
	}
}

// A row whose record type is no longer in the registry has no field kinds:
// every value is treated as text, so only a passing verdict releases it.
func TestRowsOfAnUnknownTypeFailClosed(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`INSERT INTO record_type (name, schema_version, owner, fields) VALUES ('ghost', 1, 'powerschool', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
		first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq, scan)
		VALUES ('ghost', 'e_child1', 'k', 's', '{guardians}', '{"a": "x", "b": "y"}', now(), now(), now(), '\x00', 1,
		'{"b": {"verdict": "pass", "scanner_version": "s"}}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience,
		fields, scan, observed_at, approximate, actor, importance)
		VALUES ('ghost', 'e_child1', 'k', 'created', 1, 's', '{guardians}', '{"a": "x", "b": "y"}',
		'{"b": {"new": "pass", "scanner_version": "s"}}', now(), false, 'x', 'low')`); err != nil {
		t.Fatal(err)
	}
	rows, err := r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if err != nil || len(rows) != 1 || string(rows[0].Fields["a"]) != withheldScan || string(rows[0].Fields["b"]) != `"y"` {
		t.Errorf("state %+v %v", rows, err)
	}
	ev, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if err != nil || len(ev) != 1 || string(ev[0].Fields["a"]) != withheldScan || string(ev[0].Fields["b"]) != `"y"` {
		t.Errorf("events %+v %v", ev, err)
	}
}

func TestEventsMapOldNamesForward(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "x"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "B", "comment": "x"}`)))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-30*time.Minute), true))
	r.withVersion(t, assignmentV2())
	ev, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(ev) != 3 {
		t.Fatalf("%+v", ev)
	}
	if ev[0].SchemaVersion != 1 || string(ev[0].Fields["name"]) != `"A"` || ev[0].Fields["title"] != nil {
		t.Errorf("event read under active names: %+v", ev[0])
	}
	// The text rename keeps the verdict: the value is released under the new name.
	if string(ev[0].Fields["teacher_comment"]) != `"x"` || ev[0].Fields["comment"] != nil {
		t.Errorf("text field: %+v", ev[0].Fields)
	}
	if _, old := ev[1].Changed["title"]; old || string(ev[1].Changed["name"].Old) != `"A"` || string(ev[1].Changed["name"].New) != `"B"` {
		t.Errorf("updated: %+v", ev[1].Changed)
	}
	if ev[2].LastFields["title"] != nil || string(ev[2].LastFields["name"]) != `"B"` || string(ev[2].LastFields["teacher_comment"]) != `"x"` {
		t.Errorf("removed: %+v", ev[2].LastFields)
	}
}

func TestCountsHaveNoValuesAndFollowGrants(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child1", `{"title": "SECRETTITLE"}`), rec("c1:2", "e_child1", `{"title": "B"}`)))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-time.Hour), true, rec("c1:1", "e_child2", `{"title": "C"}`)))
	counts, err := r.j.Counts(ctx, "e_child1", nil, T0.Add(-24*time.Hour))
	if err != nil || len(counts) != 1 || counts[0].DataSubject != "e_child1" || counts[0].RecordType != "assignment" ||
		counts[0].Live != 2 || counts[0].Events["low"] != 2 {
		t.Errorf("%+v %v", counts, err)
	}
	counts, _ = r.j.Counts(ctx, "e_parent1", nil, T0.Add(-24*time.Hour))
	if len(counts) != 2 || counts[0].DataSubject != "e_child1" || counts[1].DataSubject != "e_child2" || counts[1].Live != 1 {
		t.Errorf("%+v", counts)
	}
	b, _ := json.Marshal(journalapi.CountsResponse{Counts: counts})
	for _, leak := range []string{"SECRETTITLE", "title", "fields", "key", "c1:1"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("counts expose %q: %s", leak, b)
		}
	}
	counts, _ = r.j.Counts(ctx, "e_parent1", strp("e_child2"), T0.Add(-24*time.Hour))
	if len(counts) != 1 || counts[0].DataSubject != "e_child2" {
		t.Errorf("subject filter %+v", counts)
	}
	counts, _ = r.j.Counts(ctx, "e_child1", strp("e_child2"), T0.Add(-24*time.Hour))
	if len(counts) != 0 || counts == nil {
		t.Errorf("child1 counting child2 %+v", counts)
	}
	if counts, _ := r.j.Counts(ctx, "e_guest1", nil, T0.Add(-24*time.Hour)); len(counts) != 0 {
		t.Errorf("guest %+v", counts)
	}
	r.noMismatch()
}

func TestCountsWindowAndRemoval(t *testing.T) {
	r := newRig(t)
	old := T0.Add(-48 * time.Hour)
	r.apply(snap(1, "e_child1", "c1", old, true, rec("a", "e_child1", `{"title": "A"}`), rec("b", "e_child1", `{"title": "B"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("a", "e_child1", `{"title": "A2"}`)))
	// The window starts at the old snapshot's observed_at: inclusive.
	cs, _ := r.j.Counts(ctx, "e_parent1", nil, old)
	if len(cs) != 1 || cs[0].Live != 1 || cs[0].Events["low"] != 4 {
		t.Errorf("inclusive window: %+v", cs)
	}
	cs, _ = r.j.Counts(ctx, "e_parent1", nil, old.Add(time.Second))
	if len(cs) != 1 || cs[0].Events["low"] != 2 {
		t.Errorf("window excludes the old events: %+v", cs)
	}
	// Live counts do not depend on the window.
	cs, _ = r.j.Counts(ctx, "e_parent1", nil, T0)
	if len(cs) != 1 || cs[0].Live != 1 || len(cs[0].Events) != 0 || cs[0].Events == nil {
		t.Errorf("no events in the window: %+v", cs)
	}
}

func TestCountsSplitEventsByImportance(t *testing.T) {
	r := newRig(t)
	r.apply(attendanceSnap(1, T0.Add(-time.Hour),
		rec("2026-10-14", "e_child1", `{"date": "2026-10-14", "classification": "full_day", "any_unexcused": true}`),
		rec("2026-10-15", "e_child1", `{"date": "2026-10-15", "classification": "partial_day", "any_unexcused": true}`),
		rec("2026-10-16", "e_child1", `{"date": "2026-10-16", "classification": "present", "any_unexcused": false}`)))
	cs, err := r.j.Counts(ctx, "e_parent1", nil, T0.Add(-time.Hour*24))
	if err != nil || len(cs) != 1 {
		t.Fatalf("%+v %v", cs, err)
	}
	if cs[0].Live != 3 || cs[0].Events["high"] != 1 || cs[0].Events["normal"] != 1 || cs[0].Events["low"] != 1 {
		t.Errorf("%+v", cs[0])
	}
}

// Erase (4.13) deletes rows; reads then see nothing of the subject and
// cursors stay valid. (Erase itself arrives in a later task; this is the
// state it leaves.)
func TestReadsAfterASubjectIsDeleted(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-time.Hour), true, rec("k", "e_child2", `{"title": "B"}`)))
	before, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	for _, q := range []string{`DELETE FROM record_current WHERE data_subject = 'e_child1'`, `DELETE FROM record_event WHERE data_subject = 'e_child1'`} {
		if _, err := r.j.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.stateKeys("e_parent1", StateQuery{}); !same(got, []string{"e_child2/k"}) {
		t.Errorf("state %v", got)
	}
	if got := r.eventKeys("e_parent1"); !same(got, []string{"e_child2/k"}) {
		t.Errorf("events %v", got)
	}
	if got := r.stateKeys("e_child1", StateQuery{IncludeRemoved: true}); len(got) != 0 {
		t.Errorf("erased child sees %v", got)
	}
	cs, _ := r.j.Counts(ctx, "e_parent1", nil, T0.Add(-24*time.Hour))
	if len(cs) != 1 || cs[0].DataSubject != "e_child2" {
		t.Errorf("counts %+v", cs)
	}
	// The old cursor still works: the survivor's event comes after it.
	after, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", After: before[0].Seq, Limit: 10})
	if len(after) != 1 || after[0].DataSubject != "e_child2" {
		t.Errorf("cursor %+v", after)
	}
}

func TestEventsRestoredAndRemovedShapes(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	ev, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(ev) != 3 || ev[0].Kind != "created" || ev[1].Kind != "removed" || ev[2].Kind != "restored" {
		t.Fatalf("%+v", ev)
	}
	if ev[0].Changed != nil || ev[0].LastFields != nil || string(ev[0].Fields["title"]) != `"A"` {
		t.Errorf("created shape %+v", ev[0])
	}
	if ev[1].Fields != nil || ev[1].Changed != nil || string(ev[1].LastFields["title"]) != `"A"` {
		t.Errorf("removed shape %+v", ev[1])
	}
	if ev[2].LastFields != nil || ev[2].Changed != nil || string(ev[2].Fields["title"]) != `"A"` {
		t.Errorf("restored shape %+v", ev[2])
	}
	// A reader without access sees none of the three, so no last fields.
	if got := r.eventKeys("e_child2"); len(got) != 0 {
		t.Errorf("child2 %v", got)
	}
}

func TestEventsOrderBySeqNotObservedTime(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0, true, rec("late", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child1", "c2", T0.Add(-24*time.Hour), true, rec("early", "e_child1", `{"title": "B"}`)))
	ev, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(ev) != 2 || ev[0].Key != "late" || ev[1].Key != "early" || ev[0].Seq >= ev[1].Seq {
		t.Errorf("%+v", ev)
	}
}

// The Go re-check drops, and counts, a row the SQL clause let through.
func TestAGoRecheckFailureDropsTheRowAndIsCounted(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	r.j.visible = func([]roster.Grant, string, []string) bool { return false }
	rows, err := r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if err != nil || len(rows) != 0 || rows == nil || r.rec.mismatch != 1 {
		t.Errorf("state %v %v mismatch=%d", rows, err, r.rec.mismatch)
	}
	evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if err != nil || len(evs) != 0 || evs == nil || r.rec.mismatch != 2 {
		t.Errorf("events %v %v mismatch=%d", evs, err, r.rec.mismatch)
	}
	r.j.visible = nil
	rows, _ = r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if len(rows) != 1 || r.rec.mismatch != 2 {
		t.Errorf("agreement is not a mismatch: %v %d", rows, r.rec.mismatch)
	}
}

func TestReadResultsEncodeListsAsArrays(t *testing.T) {
	r := newRig(t)
	rows, _ := r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	evs, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 1})
	cs, _ := r.j.Counts(ctx, "e_parent1", nil, T0)
	for name, v := range map[string]any{"state": rows, "events": evs, "counts": cs} {
		if b, _ := json.Marshal(v); string(b) != "[]" {
			t.Errorf("%s encodes %s", name, b)
		}
	}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	rows, _ = r.j.State(ctx, StateQuery{Reader: "e_parent1"})
	if b, _ := json.Marshal(rows); !strings.Contains(string(b), `"audience":["guardians","subject"]`) {
		t.Errorf("%s", b)
	}
	cs, _ = r.j.Counts(ctx, "e_parent1", nil, T0)
	if b, _ := json.Marshal(cs); !strings.Contains(string(b), `"events":{}`) {
		t.Errorf("%s", b)
	}
}

// withVersion activates an extra record-type version the way a rollout
// does, and points the rig at it.
func (r *rig) withVersion(t *testing.T, v record.Type) {
	t.Helper()
	reg := journaltest.Registry(t, v)
	r.j.o.Registry = reg
	r.reg = reg
	if err := r.j.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
}
