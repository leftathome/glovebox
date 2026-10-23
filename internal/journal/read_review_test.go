package journal

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/access"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// noMismatch fails when the Go re-check dropped anything: an SQL bug that the
// re-check hides would otherwise pass every visibility test.
func (r *rig) noMismatch() {
	r.t.Helper()
	if r.rec.mismatch != 0 {
		r.t.Errorf("the Go re-check disagreed with the SQL filter %d times", r.rec.mismatch)
	}
}

// asymHousehold is not symmetric: e_guard1 guards e_child1 only and is a
// household member; e_outsider1 is on the roster but in no relationship.
const asymHousehold = `{
  "people": {
    "e_parent1":   {"agent": "agent-parent1", "guardian_of": ["e_child1", "e_child2"]},
    "e_guard1":    {"agent": "agent-guard1", "guardian_of": ["e_child1"]},
    "e_child1":    {"agent": "agent-child1", "siblings": ["e_child2"]},
    "e_child2":    {"agent": "agent-child2", "siblings": ["e_child1"]},
    "e_outsider1": {"agent": "agent-outsider1"}
  },
  "household_members": ["e_parent1", "e_guard1", "e_child1", "e_child2"]
}`

func (r *rig) useAsymmetricRoster() *roster.Roster {
	r.t.Helper()
	ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(asymHousehold))
	if err != nil {
		r.t.Fatal(err)
	}
	r.ros.Store(ros)
	return ros
}

// insertHouseholdWideRow writes a record and its created event whose
// data_subject is the empty string (the household as a whole).
func (r *rig) insertHouseholdWideRow(key string, audience string) {
	r.t.Helper()
	if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience,
		fields, observed_at, approximate, actor, importance)
		VALUES ('assignment', '', $1, 'created', 1, 's', $2, '{"title": "x"}', $3, false, 'x', 'low')`, key, audience, T0.Add(-time.Hour)); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
		first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq)
		VALUES ('assignment', '', $1, 's', $2, '{"title": "x"}', $3, $3, $3, '\x00', 1)`, key, audience, T0.Add(-time.Hour)); err != nil {
		r.t.Fatal(err)
	}
}

// A reader who guards one child must never see the other's guardian rows, and
// the grants with an empty subject (the household as a whole) must not match
// rows of a named subject. The symmetric fixture roster cannot show either.
func TestAsymmetricRosterKeepsGuardiansToTheirOwnChild(t *testing.T) {
	r := newRig(t)
	r.useAsymmetricRoster()
	n := 0
	for _, subject := range []string{"e_child1", "e_child2"} {
		for _, token := range []string{"guardians", "subject", "siblings", "household"} {
			n++
			x := rec("k_"+token, subject, `{"title": "T"}`)
			x.Audience = []string{token}
			r.apply(snap(n, subject, "c_"+token, T0.Add(-time.Hour), true, x))
		}
	}
	// A household-wide row: no data subject. Only holders of the "" grants see it.
	r.insertHouseholdWideRow("hh", "{guardians,subject}")

	want := map[string][]string{
		// Guards e_child1 only; a household member, so (m, household) for all.
		"e_guard1": {"/hh", "e_child1/k_guardians", "e_child1/k_household", "e_child2/k_household"},
		"e_parent1": {"/hh", "e_child1/k_guardians", "e_child1/k_household",
			"e_child2/k_guardians", "e_child2/k_household"},
		"e_child1":    {"e_child1/k_household", "e_child1/k_subject", "e_child2/k_household", "e_child2/k_siblings"},
		"e_child2":    {"e_child1/k_household", "e_child1/k_siblings", "e_child2/k_household", "e_child2/k_subject"},
		"e_outsider1": {},
	}
	for reader, keys := range want {
		if got := r.stateKeys(reader, StateQuery{}); !same(got, keys) {
			t.Errorf("state as %s:\n got %v\nwant %v", reader, got, keys)
		}
		if got := r.eventKeys(reader); !same(got, keys) {
			t.Errorf("events as %s:\n got %v\nwant %v", reader, got, keys)
		}
		if got := r.liveCount(reader); got != len(keys) {
			t.Errorf("counts as %s: %d live, want %d", reader, got, len(keys))
		}
	}
	// e_guard1 asking for e_child2 by name sees the household row only.
	if got := r.stateKeys("e_guard1", StateQuery{Subject: strp("e_child2")}); !same(got, []string{"e_child2/k_household"}) {
		t.Errorf("e_guard1 asks for e_child2: %v", got)
	}
	cs, err := r.j.Counts(ctx, "e_guard1", strp("e_child2"), T0.Add(-24*time.Hour))
	if err != nil || len(cs) != 1 || cs[0].Live != 1 || cs[0].Events["low"] != 1 {
		t.Errorf("e_guard1 counts for e_child2: %+v %v", cs, err)
	}
	r.noMismatch()
}

// group_key is stored as written. Even if one carries text (a rule written
// before group_by on a text field was refused), a withheld value must not
// reach a reader through it, nor the sha256 of one.
func TestGroupKeyNeverCarriesWithheldText(t *testing.T) {
	r := newRig(t)
	insert := func(key, kind, groupKey, fields, changed, scan string) {
		t.Helper()
		var f, c any
		if fields != "" {
			f = fields
		}
		if changed != "" {
			c = changed
		}
		if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience,
			fields, changed, scan, observed_at, approximate, actor, importance, rule, group_key)
			VALUES ('assignment', 'e_child1', $1, $2, 1, 's', '{guardians,subject}', $3::jsonb, $4::jsonb, $5::jsonb,
			now(), false, 'x', 'normal', 'r', $6)`, key, kind, f, c, scan, groupKey); err != nil {
			t.Fatal(err)
		}
	}
	const longKey = "sha256=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	insert("failing", "created", "field:comment=SECRET", `{"comment": "SECRET", "title": "A"}`, "",
		`{"comment": {"new": "quarantine", "scanner_version": "s"}}`)
	insert("failing-hash", "created", longKey, `{"comment": "SECRET", "title": "A"}`, "",
		`{"comment": {"new": "error", "scanner_version": "s"}}`)
	insert("unscanned", "created", "field:comment=SECRET", `{"comment": "SECRET"}`, "", `{}`)
	insert("old-failing", "updated", "k=v", "", `{"comment": ["SECRET", "fine"]}`,
		`{"comment": {"old": "quarantine", "new": "pass", "scanner_version": "s"}}`)
	insert("removed-failing", "removed", "k=v", "", `{"comment": "SECRET"}`,
		`{"comment": {"old": "reject", "scanner_version": "s"}}`)
	insert("clean", "created", "k=v", `{"comment": "fine", "title": "A"}`, "",
		`{"comment": {"new": "pass", "scanner_version": "s"}}`)
	insert("no-text", "created", "k=v", `{"title": "A"}`, "", `{}`)
	for _, reader := range []string{"e_parent1", "e_child1"} {
		evs, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 100})
		if err != nil || len(evs) != 7 {
			t.Fatalf("%s: %v %v", reader, evs, err)
		}
		for _, e := range evs {
			clean := e.Key == "clean" || e.Key == "no-text"
			if clean && e.GroupKey != "k=v" {
				t.Errorf("%s/%s: a key without withheld text was dropped: %q", reader, e.Key, e.GroupKey)
			}
			if !clean && e.GroupKey != "" {
				t.Errorf("%s/%s: group_key %q", reader, e.Key, e.GroupKey)
			}
			if strings.Contains(e.GroupKey, "SECRET") {
				t.Errorf("%s/%s: group_key leaks text", reader, e.Key)
			}
		}
	}
}

// Counts check each group again in Go, as State and Events check each row.
func TestCountsRecheckDropsGroupsAndCountsMismatches(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("a", "e_child1", `{"title": "A"}`)))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-time.Hour), true, rec("a", "e_child2", `{"title": "B"}`), rec("b", "e_child2", `{"title": "C"}`)))
	r.j.visible = func([]roster.Grant, string, []string) bool { return false }
	cs, err := r.j.Counts(ctx, "e_parent1", nil, T0.Add(-24*time.Hour))
	if err != nil || len(cs) != 0 || cs == nil {
		t.Errorf("all groups dropped: %v %v", cs, err)
	}
	// Two subjects, each with a live group and an event group.
	if r.rec.mismatch != 4 {
		t.Errorf("mismatches %d, want 4", r.rec.mismatch)
	}
	r.rec.mismatch = 0
	r.j.visible = func(g []roster.Grant, subject string, aud []string) bool {
		return subject != "e_child2" && access.Visible(g, subject, aud)
	}
	cs, err = r.j.Counts(ctx, "e_parent1", nil, T0.Add(-24*time.Hour))
	if err != nil || len(cs) != 1 || cs[0].DataSubject != "e_child1" || cs[0].Live != 1 || cs[0].Events["low"] != 1 {
		t.Errorf("one subject dropped: %+v %v", cs, err)
	}
	if r.rec.mismatch != 2 {
		t.Errorf("mismatches %d, want 2", r.rec.mismatch)
	}
	r.j.visible = nil
	r.rec.mismatch = 0
	cs, _ = r.j.Counts(ctx, "e_parent1", nil, T0.Add(-24*time.Hour))
	if len(cs) != 2 || cs[1].Live != 2 || cs[1].Events["low"] != 2 {
		t.Errorf("agreement: %+v", cs)
	}
	r.noMismatch()
}

// Two audiences of one subject and type are separate groups, summed.
func TestCountsSumGroupsOfDifferentAudiences(t *testing.T) {
	r := newRig(t)
	g := rec("a", "e_child1", `{"title": "A"}`)
	g.Audience = []string{"guardians"}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, g))
	r.apply(snap(2, "e_child1", "c2", T0.Add(-time.Hour), true, rec("b", "e_child1", `{"title": "B"}`)))
	r.apply(snap(3, "e_child1", "c3", T0.Add(-time.Hour), true, rec("c", "e_child1", `{"title": "C"}`)))
	cs, _ := r.j.Counts(ctx, "e_parent1", nil, T0.Add(-24*time.Hour))
	if len(cs) != 1 || cs[0].Live != 3 || cs[0].Events["low"] != 3 {
		t.Errorf("parent: %+v", cs)
	}
	cs, _ = r.j.Counts(ctx, "e_child1", nil, T0.Add(-24*time.Hour))
	if len(cs) != 1 || cs[0].Live != 2 || cs[0].Events["low"] != 2 {
		t.Errorf("child: %+v", cs)
	}
	r.noMismatch()
}

func TestNoRosterIsNotAnUnknownReader(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("a", "e_child1", `{"title": "A"}`)))
	// With a roster, a stranger is an unknown reader.
	_, _, err := r.j.StateWithRoster(ctx, StateQuery{Reader: "e_stranger"})
	if !errors.Is(err, ErrUnknownReader) || errors.Is(err, ErrNoRoster) {
		t.Errorf("stranger: %v", err)
	}
	r.j.o.Roster = func() *roster.Roster { return nil }
	for _, who := range []string{"e_parent1", "e_stranger", ""} {
		rows, sha, err := r.j.StateWithRoster(ctx, StateQuery{Reader: who})
		if !errors.Is(err, ErrNoRoster) || errors.Is(err, ErrUnknownReader) || len(rows) != 0 || rows == nil || sha != "" {
			t.Errorf("state as %q: %v %q %v", who, rows, sha, err)
		}
		evs, err := r.j.Events(ctx, EventsQuery{Reader: who, Limit: 1})
		if !errors.Is(err, ErrNoRoster) || len(evs) != 0 || evs == nil {
			t.Errorf("events as %q: %v %v", who, evs, err)
		}
		cs, err := r.j.Counts(ctx, who, nil, T0.Add(-24*time.Hour))
		if !errors.Is(err, ErrNoRoster) || len(cs) != 0 || cs == nil {
			t.Errorf("counts as %q: %v %v", who, cs, err)
		}
	}
}

// The SHA a read reports is that of the roster that filtered it, even when
// the roster in use changes between two calls to the roster function.
func TestStateReportsTheRosterThatFilteredIt(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("a", "e_child1", `{"title": "A"}`)))
	a := journaltest.Roster(t)
	b, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(asymHousehold))
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256() == b.SHA256() || a.SHA256() == "" {
		t.Fatal("the two rosters must differ")
	}
	calls := 0
	r.j.o.Roster = func() *roster.Roster {
		calls++
		if calls == 1 {
			return a
		}
		return b
	}
	rows, sha, err := r.j.StateWithRoster(ctx, StateQuery{Reader: "e_parent1"})
	if err != nil || len(rows) != 1 || sha != a.SHA256() || calls != 1 {
		t.Errorf("rows %v sha %q (want %q) calls %d err %v", rows, sha, a.SHA256(), calls, err)
	}
	// State is StateWithRoster without the hash.
	r.j.o.Roster = func() *roster.Roster { return b }
	rows, sha, _ = r.j.StateWithRoster(ctx, StateQuery{Reader: "e_parent1"})
	if sha != b.SHA256() || len(rows) != 1 {
		t.Errorf("second roster: %d rows, sha %q", len(rows), sha)
	}
}

// The scope and observation time before an audience change belong to the old
// audience (4.8).
func TestPrevScopeAndTimeFollowThePrevAudience(t *testing.T) {
	r := newRig(t)
	prevObs := T0.Add(-5 * time.Hour)
	ins := func(key, prev string) {
		t.Helper()
		if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope,
			prev_scope, audience, prev_audience, changed, observed_at, prev_observed_at, approximate, actor, importance)
			VALUES ('assignment', 'e_child1', $1, 'updated', 1, 'new-scope', 'old-scope', '{guardians,subject}', `+prev+`,
			'{"title": ["old", "new"]}', now(), $2, false, 'x', 'low')`, key, prevObs); err != nil {
			t.Fatal(err)
		}
	}
	ins("unchanged", "NULL")
	ins("guardians-only", "'{guardians}'")
	ins("empty", "'{}'")
	ins("public", "'{public}'")
	type view struct {
		scope string
		obs   bool
	}
	read := func(reader string) map[string]view {
		evs, err := r.j.Events(ctx, EventsQuery{Reader: reader, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]view{}
		for _, e := range evs {
			m[e.Key] = view{e.PrevScope, e.PrevObservedAt != nil}
			if e.PrevObservedAt != nil && !e.PrevObservedAt.Equal(prevObs) {
				t.Errorf("%s: prev_observed_at %v", e.Key, e.PrevObservedAt)
			}
			if e.Scope != "new-scope" {
				t.Errorf("%s: the current scope is always shown: %q", e.Key, e.Scope)
			}
		}
		return m
	}
	full, none := view{"old-scope", true}, view{"", false}
	for reader, want := range map[string]map[string]view{
		"e_parent1": {"unchanged": full, "guardians-only": full, "empty": none, "public": full},
		"e_child1":  {"unchanged": full, "guardians-only": none, "empty": none, "public": full},
	} {
		got := read(reader)
		if len(got) != 4 {
			t.Errorf("%s sees %v", reader, got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s %s: %+v, want %+v", reader, k, got[k], v)
			}
		}
	}
}

// A stored value the journal cannot decode is an error, never an empty or
// partially masked answer.
func TestUndecodableStoredJSONIsAnError(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("a", "e_child1", `{"title": "A", "comment": "x"}`)))
	if _, err := r.j.State(ctx, StateQuery{Reader: "e_parent1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`UPDATE record_current SET scan = '[]'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := r.j.State(ctx, StateQuery{Reader: "e_parent1"}); err == nil || len(rows) != 0 || rows == nil {
		t.Errorf("state with a bad scan: %v %v", rows, err)
	}
	if _, err := r.j.db.Exec(`UPDATE record_current SET scan = '{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`UPDATE record_event SET scan = '[]'`); err != nil {
		t.Fatal(err)
	}
	if evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10}); err == nil || len(evs) != 0 || evs == nil {
		t.Errorf("events with a bad scan: %v %v", evs, err)
	}
	if _, err := r.j.db.Exec(`UPDATE record_event SET scan = '{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10}); err != nil {
		t.Fatalf("restored: %v", err)
	}
	if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience,
		fields, notify, observed_at, approximate, actor, importance)
		VALUES ('assignment', 'e_child1', 'n', 'created', 1, 's', '{guardians}', '{"title": "x"}', '"not a list"', now(), false, 'x', 'low')`); err != nil {
		t.Fatal(err)
	}
	if evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10}); err == nil || len(evs) != 0 || evs == nil {
		t.Errorf("events with a bad notify: %v %v", evs, err)
	}
}

// A failing verdict on an old text value, across an audience change: a reader
// who held the old audience sees the scan marker; one who did not sees the
// audience marker (the audience reason wins, and says nothing about the scan).
func TestOldTextAcrossAnAudienceChangeKeepsBothReasonsApart(t *testing.T) {
	r := newRig(t)
	g := rec("k", "e_child1", `{"title": "A", "comment": "INJECT one"}`)
	g.Audience = []string{"guardians"}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, g))
	open := rec("k", "e_child1", `{"title": "A", "comment": "fine"}`)
	open.Audience = []string{"guardians", "subject"}
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, open))
	child, _ := r.j.Events(ctx, EventsQuery{Reader: "e_child1", Limit: 10})
	if len(child) != 1 {
		t.Fatalf("%+v", child)
	}
	if c := child[0].Changed["comment"]; string(c.Old) != withheldAudience || string(c.New) != `"fine"` {
		t.Errorf("child: %+v", c)
	}
	parent, _ := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10})
	if len(parent) != 2 {
		t.Fatalf("%+v", parent)
	}
	if c := parent[1].Changed["comment"]; string(c.Old) != withheldScan || string(c.New) != `"fine"` {
		t.Errorf("parent: %+v", c)
	}
	r.noMismatch()
}

// An event kind the journal does not know is an error, not a silently empty
// event. The schema forbids one, so the test drops the CHECK first.
func TestAnEventOfUnknownKindIsAnError(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`ALTER TABLE record_event DROP CONSTRAINT record_event_kind_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.j.db.Exec(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience,
		fields, observed_at, approximate, actor, importance)
		VALUES ('assignment', 'e_child1', 'k', 'mystery', 1, 's', '{guardians}', '{"title": "x"}', now(), false, 'x', 'low')`); err != nil {
		t.Fatal(err)
	}
	if evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: 10}); err == nil || len(evs) != 0 || evs == nil {
		t.Errorf("%v %v", evs, err)
	}
}

func TestEventsLimitHasAnUpperBound(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: MaxEventsLimit}); err != nil {
		t.Errorf("the maximum: %v", err)
	}
	if evs, err := r.j.Events(ctx, EventsQuery{Reader: "e_parent1", Limit: MaxEventsLimit + 1}); !errors.Is(err, ErrBadQuery) || len(evs) != 0 {
		t.Errorf("over the maximum: %v %v", evs, err)
	}
}
