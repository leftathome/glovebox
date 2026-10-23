package diff

import "testing"

func TestGuard(t *testing.T) {
	g := DefaultGuard // 0.3, 3
	for _, c := range []struct {
		live, removals int
		hold           bool
	}{
		{0, 0, false},
		{1, 1, false},  // decision 1: small scopes are never held ...
		{3, 1, false},  // ... although 1/3 > 0.3
		{3, 3, false},  // "every key of a scope that had more than min_keys"
		{4, 4, true},   // every key, more than min_keys
		{4, 1, false},  // 0.25
		{4, 2, true},   // 0.5
		{10, 3, false}, // exactly 0.3 is not "more than"
		{10, 4, true},
		{100, 0, false},
		{13, 4, true}, // 0.307: kills a MaxFraction of 0.31
		{20, 7, true}, // 0.35: kills a MaxFraction of 0.39
	} {
		if got := g.Hold(c.live, c.removals); got != c.hold {
			t.Errorf("Hold(%d, %d) = %v", c.live, c.removals, got)
		}
	}
	// removals == live holds even when the fraction cannot.
	all := Guard{MaxFraction: 1, MinKeys: 3}
	if !all.Hold(4, 4) || all.Hold(3, 3) {
		t.Error("removals == live branch")
	}
}
