package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// declared lists the field names each schema version of chainRegistry
// declares; the active version is 3.
var declared = map[int][]string{1: {"title", "date"}, 2: {"name", "date"}, 3: {"label", "date"}}

// model is the store's write rule (Task 15) over plain maps. Rows hold
// active (v3) names; events are written in the names of the schema version
// the snapshot arrived at.
type model struct {
	reg    *record.Registry
	rows   map[string]Current
	events []FoldEvent
}

// legacy renames an active field name to the name it had at version v.
func (m *model) legacy(v int, field string) string {
	for _, cand := range declared[v] {
		if m.reg.ForwardName("t", v, cand) == field {
			return cand
		}
	}
	panic("no legacy name for " + field)
}

func (m *model) legacyFields(v int, f record.Fields) record.Fields {
	if f == nil {
		return nil
	}
	out := record.Fields{}
	for k, val := range f {
		out[m.legacy(v, k)] = val
	}
	return out
}

func (m *model) apply(t *testing.T, in Input, p Plan, v int) {
	for _, e := range append(append([]Event(nil), p.Events...), p.Removals...) {
		fe := FoldEvent{ID: Identity{"t", "s", e.Key}, Kind: e.Kind, SchemaVersion: v, Scope: e.Scope, Audience: e.Audience}
		c := m.rows[e.Key]
		switch e.Kind {
		case Created, Restored:
			fe.Fields = m.legacyFields(v, e.After)
			if got := m.reg.Forward("t", v, fe.Fields); !sameFields(got, e.After) {
				t.Fatalf("Forward(v%d, %v) = %v, want %v", v, fe.Fields, got, e.After)
			}
			c = Current{Key: e.Key, Scope: e.Scope, Audience: e.Audience, Fields: e.After, Hash: e.After.Hash(), LastSeenAt: in.ObservedAt}
		case Updated:
			fe.Changed = map[string]Change{}
			for k, ch := range e.Changed {
				fe.Changed[m.legacy(v, k)] = ch
			}
			c = Current{Key: e.Key, Scope: e.Scope, Audience: e.Audience, Fields: e.After, Hash: e.After.Hash(), LastSeenAt: in.ObservedAt}
		case Removed:
			c.Removed = true
		}
		c.LastConfirmedAt = in.ObservedAt
		m.rows[e.Key] = c
		m.events = append(m.events, fe)
	}
	for _, k := range p.Confirm {
		c := m.rows[k]
		c.LastSeenAt, c.LastConfirmedAt = in.ObservedAt, in.ObservedAt
		m.rows[k] = c
	}
}

// perturb changes observation columns the way the real store can leave
// them, without touching event-derived state: a row confirmed later by
// another scope (stale for the next snapshot), and a stored hash that
// disagrees with the stored fields.
func (m *model) perturb(r *rand.Rand, at time.Time) {
	for k, c := range m.rows {
		switch r.Intn(8) {
		case 0:
			c.LastConfirmedAt = at.Add(2 * time.Hour)
		case 1:
			c.Hash = []byte("stale-hash")
		default:
			continue
		}
		m.rows[k] = c
	}
}

func genInput(r *rand.Rand, at time.Time) Input {
	scopes := []string{scopeA, scopeB}
	titles := []string{`"a"`, `"b"`, `"c"`}
	dates := []string{`"2026-09-10"`, `"2026-10-01"`, `"2026-10-20"`}
	auds := [][]string{{"guardians", "subject"}, {"guardians"}}
	in := Input{Scope: scopes[r.Intn(2)], Complete: r.Intn(2) == 0, ObservedAt: at}
	if in.Complete && r.Intn(3) == 0 {
		in.Range = &record.Range{Field: "date", From: "2026-09-01", To: "2026-10-15"}
	}
	for k := 0; k < 6; k++ {
		if r.Intn(2) == 0 {
			continue
		}
		fields := record.Fields{"label": json.RawMessage(titles[r.Intn(3)]), "date": json.RawMessage(dates[r.Intn(3)])}
		if r.Intn(3) == 0 {
			delete(fields, "label")
		}
		in.Records = append(in.Records, record.NormRecord{Key: fmt.Sprintf("k%d", k), Audience: auds[r.Intn(2)], Fields: fields, Hash: fields.Hash()})
	}
	return in
}

func TestPropertyDiffAndFoldAgree(t *testing.T) {
	reg := chainRegistry(t)
	for seed := int64(0); seed < 500; seed++ {
		r := rand.New(rand.NewSource(seed))
		m := &model{reg: reg, rows: map[string]Current{}}
		at := t0
		for step := 0; step < 12; step++ {
			at = at.Add(time.Hour)
			m.perturb(r, at)
			in := genInput(r, at)
			in.Current = m.rows
			v := 1 + r.Intn(3)
			p := Diff(in)
			if !in.Complete && len(p.Removals) > 0 {
				t.Fatalf("seed %d step %d: an incomplete snapshot removed %d records", seed, step, len(p.Removals))
			}
			m.apply(t, in, p, v)
			folded := Fold(m.events, reg.ForwardName)
			if len(folded) != len(m.rows) {
				t.Fatalf("seed %d step %d: fold has %d records, current %d", seed, step, len(folded), len(m.rows))
			}
			for key, c := range m.rows {
				got := folded[Identity{"t", "s", key}]
				want := FoldRow{Scope: c.Scope, Audience: c.Audience, Fields: c.Fields, Removed: c.Removed}
				if got.Removed != want.Removed || got.Scope != want.Scope || !reflect.DeepEqual(got.Audience, want.Audience) || !sameFields(got.Fields, want.Fields) {
					t.Fatalf("seed %d step %d key %s: fold %+v, current %+v", seed, step, key, got, want)
				}
			}
			// The same observation again changes nothing.
			again := in
			again.Current = m.rows
			if p2 := Diff(again); len(p2.Events) != 0 || len(p2.Removals) != 0 {
				t.Fatalf("seed %d step %d: re-diffing planned %+v", seed, step, p2)
			}
		}
	}
}

func sameFields(a, b record.Fields) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}
