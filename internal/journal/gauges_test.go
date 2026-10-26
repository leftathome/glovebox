package journal

import (
	"fmt"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func TestGauges(t *testing.T) {
	r := newRig(t)
	var recs []journalapi.Record
	for i := 1; i <= 5; i++ {
		recs = append(recs, rec(fmt.Sprintf("c1:%d", i), "e_child1", `{"title": "A"}`))
	}
	r.clock.Set(T0.Add(-time.Hour))
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, recs...))
	r.clock.Set(T0)
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, recs[:1]...)) // held
	held, last, err := r.j.Gauges(ctx, []string{"source=powerschool/", "source=schoology/"})
	if err != nil {
		t.Fatal(err)
	}
	if held != 1 || !last["source=powerschool/"].Equal(T0) {
		t.Errorf("held %d last %v", held, last)
	}
	if _, ok := last["source=schoology/"]; ok {
		t.Error("a prefix with no snapshots has a timestamp")
	}
}

func TestGaugesCountOnlyHeldAndFollowDiscards(t *testing.T) {
	r := newRig(t)
	five(r, 1, T0.Add(-3*time.Hour))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, keep(1)...)) // held
	held, last, err := r.j.Gauges(ctx, nil)
	if err != nil || held != 1 || len(last) != 0 {
		t.Fatalf("held %d last %v err %v", held, last, err)
	}
	if err := r.j.DiscardHeld(ctx, uid(2)); err != nil {
		t.Fatal(err)
	}
	if held, _, err = r.j.Gauges(ctx, nil); err != nil || held != 0 {
		t.Errorf("after a discard: held %d err %v", held, err)
	}
}
