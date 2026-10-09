package powerschool

import (
	"github.com/leftathome/glovebox/connector"
)

// Change is the outcome of comparing a record's fingerprint with the one
// stored for it. Its String form is the `change` tag on staged items and
// a metric label, so it is stable across releases.
type Change int

const (
	ChangeNone    Change = iota // fingerprint matches; do not stage
	ChangeNew                   // never seen; stage
	ChangeUpdated               // seen with a different fingerprint; stage
)

// String returns the tag / metric label for a Change.
func (c Change) String() string {
	switch c {
	case ChangeNew:
		return "new"
	case ChangeUpdated:
		return "updated"
	}
	return "unchanged"
}

// CheckpointKey builds the framework Checkpoint key for one record:
// "<surface>:<kid>:<id>". Schoology checkpoints one highest-seen id per
// surface because its items are append-only; PowerSchool records are
// mutable (a grade is re-scored, an absence is added), so each record
// keeps its own fingerprint and a changed one is staged again.
func CheckpointKey(surface, kid, id string) string {
	return surface + ":" + kid + ":" + id
}

// Compare classifies a record against its stored fingerprint and returns
// the stored value (empty when new) so a processor can describe what
// changed.
func Compare(cp connector.Checkpoint, surface, kid, id, fingerprint string) (Change, string) {
	prev, ok := cp.Load(CheckpointKey(surface, kid, id))
	switch {
	case !ok:
		return ChangeNew, ""
	case prev == fingerprint:
		return ChangeNone, prev
	default:
		return ChangeUpdated, prev
	}
}

// SaveFingerprint records a record's fingerprint. MUST be called only
// after Commit() returns nil per the framework per-item-checkpoint
// discipline (spec 05 section 3.2), or when a record is deliberately
// baselined without being staged.
func SaveFingerprint(cp connector.Checkpoint, surface, kid, id, fingerprint string) error {
	return cp.Save(CheckpointKey(surface, kid, id), fingerprint)
}
