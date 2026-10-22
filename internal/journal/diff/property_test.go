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

// model is the store's write rule (Task 15) over plain maps.
type model struct {
	rows   map[string]Current
	events []FoldEvent
}

func (m *model) apply(in Input, p Plan) {
	for _, e := range append(append([]Event(nil), p.Events...), p.Removals...) {
		fe := FoldEvent{ID: Identity{"t", "s", e.Key}, Kind: e.Kind, SchemaVersion: 1, Scope: e.Scope, Audience: e.Audience}
		c := m.rows[e.Key]
		switch e.Kind {
		case Created, Restored:
			fe.Fields = e.After
			c = Current{Key: e.Key, Scope: e.Scope, Audience: e.Audience, Fields: e.After, Hash: e.After.Hash(), LastSeenAt: in.ObservedAt}
		case Updated:
			fe.Changed = e.Changed
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
		fields := record.Fields{"title": json.RawMessage(titles[r.Intn(3)]), "date": json.RawMessage(dates[r.Intn(3)])}
		if r.Intn(3) == 0 {
			delete(fields, "title")
		}
		in.Records = append(in.Records, record.NormRecord{Key: fmt.Sprintf("k%d", k), Audience: auds[r.Intn(2)], Fields: fields, Hash: fields.Hash()})
	}
	return in
}

func TestPropertyDiffAndFoldAgree(t *testing.T) {
	rename := func(_ string, _ int, f string) string { return f }
	for seed := int64(0); seed < 500; seed++ {
		r := rand.New(rand.NewSource(seed))
		m := &model{rows: map[string]Current{}}
		at := t0
		for step := 0; step < 12; step++ {
			at = at.Add(time.Hour)
			in := genInput(r, at)
			in.Current = m.rows
			p := Diff(in)
			if !in.Complete && len(p.Removals) > 0 {
				t.Fatalf("seed %d step %d: an incomplete snapshot removed %d records", seed, step, len(p.Removals))
			}
			m.apply(in, p)
			folded := Fold(m.events, rename)
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
			again.ObservedAt = at
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
