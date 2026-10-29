package diff

// Guard is the removal guard (4.5).
type Guard struct {
	// MaxFraction: hold when more than this fraction of the live keys
	// (in range) would be removed.
	MaxFraction float64
	// MinKeys: scopes with this many live keys (in range) or fewer are
	// never held. The spec states it for the "every key" test; this plan
	// applies it to both tests (decision 1), otherwise removing one of one
	// or two -- a corrected absence day early in a term, section 5.1 --
	// would always be held.
	MinKeys int
}

// DefaultGuard is the spec's default (0.3, 3).
var DefaultGuard = Guard{MaxFraction: 0.3, MinKeys: 3}

// Hold reports whether a complete snapshot that would remove removals of
// live keys must be held for an operator.
func (g Guard) Hold(live, removals int) bool {
	if removals == 0 || live <= g.MinKeys {
		return false
	}
	return removals == live || float64(removals) > g.MaxFraction*float64(live)
}
