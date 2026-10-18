package diff

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/record"
)

var (
	t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(24 * time.Hour)
	t2 = t1.Add(24 * time.Hour)
)

const scopeA = "source=powerschool/surface=assignments/data_subject=e_child1/course=c1"
const scopeB = "source=powerschool/surface=assignments/data_subject=e_child1/course=c2"

func f(kv ...string) record.Fields {
	out := record.Fields{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = json.RawMessage(kv[i+1])
	}
	return out
}

func cur(key, scope string, fields record.Fields, removed bool, confirmed time.Time, aud ...string) Current {
	if len(aud) == 0 {
		aud = []string{"guardians", "subject"}
	}
	return Current{Key: key, Scope: scope, Audience: aud, Fields: fields, Removed: removed, Hash: fields.Hash(),
		LastSeenAt: confirmed, LastConfirmedAt: confirmed}
}

func in(key string, fields record.Fields, aud ...string) record.NormRecord {
	if len(aud) == 0 {
		aud = []string{"guardians", "subject"}
	}
	return record.NormRecord{Key: key, DataSubject: "e_child1", Audience: aud, Fields: fields, Hash: fields.Hash()}
}

func byKey(cs ...Current) map[string]Current {
	m := map[string]Current{}
	for _, c := range cs {
		m[c.Key] = c
	}
	return m
}

func TestDiffClassifiesEveryRecord(t *testing.T) {
	p := Diff(Input{
		Scope: scopeA, ObservedAt: t2, Complete: true,
		Current: byKey(
			cur("same", scopeA, f("title", `"S"`), false, t1),
			cur("changed", scopeA, f("title", `"Old"`, "score", `1`), false, t1),
			cur("gone", scopeA, f("title", `"G"`), false, t1),
			cur("back", scopeA, f("title", `"B"`), true, t1),
			cur("moved", scopeB, f("title", `"M"`), false, t1),
			cur("audience", scopeA, f("title", `"A"`), false, t1),
			cur("other-scope", scopeB, f("title", `"O"`), false, t1),
		),
		Records: []record.NormRecord{
			in("same", f("title", `"S"`)),
			in("changed", f("title", `"New"`)),
			in("back", f("title", `"B2"`)),
			in("moved", f("title", `"M"`)),
			in("audience", f("title", `"A"`), "guardians"),
			in("new", f("title", `"N"`)),
		},
	})
	kinds := map[string]Kind{}
	for _, e := range p.Events {
		kinds[e.Key] = e.Kind
	}
	want := map[string]Kind{"changed": Updated, "back": Restored, "moved": Updated, "audience": Updated, "new": Created}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s: %s, want %s", k, kinds[k], v)
		}
	}
	if len(p.Events) != len(want) {
		t.Errorf("events %v", kinds)
	}
	if len(p.Confirm) != 1 || p.Confirm[0] != "same" {
		t.Errorf("confirm %v", p.Confirm)
	}
	if len(p.Removals) != 1 || p.Removals[0].Key != "gone" || p.Removals[0].Before["title"] == nil {
		t.Errorf("removals %+v", p.Removals)
	}
	if p.LiveInRange != 4 { // same, changed, gone, audience: live and in scope A
		t.Errorf("LiveInRange = %d", p.LiveInRange)
	}
	for _, e := range p.Events {
		switch e.Key {
		case "changed":
			c := e.Changed
			if len(c) != 2 || string(c["title"].Old) != `"Old"` || string(c["title"].New) != `"New"` || c["score"].New != nil || string(c["score"].Old) != "1" {
				t.Errorf("changed: %+v", c)
			}
			if e.PrevObservedAt == nil || !e.PrevObservedAt.Equal(t1) {
				t.Errorf("prev_observed_at %v", e.PrevObservedAt)
			}
		case "moved":
			if e.PrevScope != scopeB || e.Scope != scopeA || len(e.Changed) != 0 {
				t.Errorf("moved: %+v", e)
			}
		case "audience":
			if len(e.PrevAudience) != 2 || len(e.Audience) != 1 {
				t.Errorf("audience: %+v", e)
			}
		case "back":
			if string(e.After["title"]) != `"B2"` {
				t.Errorf("restored carries full fields: %+v", e)
			}
		case "new":
			if e.PrevObservedAt != nil {
				t.Error("created has no prev_observed_at")
			}
		}
	}
	s := p.Stats()
	if s.Created != 1 || s.Updated != 3 || s.Restored != 1 || s.Removed != 1 || s.Unchanged != 1 {
		t.Errorf("stats %+v", s)
	}
}

func TestIncompleteSnapshotsNeverRemove(t *testing.T) {
	p := Diff(Input{Scope: scopeA, ObservedAt: t2, Complete: false,
		Current: byKey(cur("gone", scopeA, f("title", `"G"`), false, t1))})
	if len(p.Removals) != 0 || p.LiveInRange != 0 {
		t.Errorf("%+v", p)
	}
}

func TestCompleteRangeLimitsRemoval(t *testing.T) {
	rng := &record.Range{Field: "date", From: "2026-09-01", To: "2026-10-15"}
	p := Diff(Input{Scope: scopeA, ObservedAt: t2, Complete: true, Range: rng,
		Current: byKey(
			cur("inside", scopeA, f("date", `"2026-10-01"`), false, t1),
			cur("before", scopeA, f("date", `"2026-06-01"`), false, t1),
			cur("nodate", scopeA, f("title", `"x"`), false, t1),
		)})
	if len(p.Removals) != 1 || p.Removals[0].Key != "inside" || p.LiveInRange != 1 {
		t.Errorf("removals %+v live %d", p.Removals, p.LiveInRange)
	}
}

// A record confirmed by another scope after this snapshot was observed is
// not moved back, updated or removed by it (decision 6).
func TestStaleRecordsAreLeftAlone(t *testing.T) {
	p := Diff(Input{Scope: scopeA, ObservedAt: t1, Complete: true,
		Current: byKey(cur("k", scopeB, f("title", `"newer"`), false, t2)),
		Records: []record.NormRecord{in("k", f("title", `"older"`))}})
	if len(p.Events) != 0 || len(p.Stale) != 1 || p.Stats().Stale != 1 {
		t.Errorf("%+v", p)
	}
}

func TestRemovalsAreSortedByKey(t *testing.T) {
	p := Diff(Input{Scope: scopeA, ObservedAt: t2, Complete: true,
		Current: byKey(cur("b", scopeA, f(), false, t1), cur("a", scopeA, f(), false, t1), cur("c", scopeA, f(), false, t1))})
	if len(p.Removals) != 3 || p.Removals[0].Key != "a" || p.Removals[2].Key != "c" {
		t.Errorf("%+v", p.Removals)
	}
}
