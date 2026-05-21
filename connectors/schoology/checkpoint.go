package schoology

import (
	"fmt"
	"strconv"

	"github.com/leftathome/glovebox/connector"
)

// CheckpointKey builds the framework Checkpoint key for a given content
// surface. Returns "<surface>:<scope>:last_id" -- scope is the kid
// label for per-kid surfaces, or empty for parent-level (messages,
// message attachments).
func CheckpointKey(surface, scope string) string {
	if scope == "" {
		return surface + ":last_id"
	}
	return surface + ":" + scope + ":last_id"
}

// LastSeenID reads the highest-seen ID for a content surface.
// Returns (0, nil) when the checkpoint is fresh (i.e. first poll).
// Returns (0, err) when the stored value is unparseable. The processor
// convention (see attachments.go) is to skip the affected item with a
// slog.Error + metric increment and continue the poll, rather than
// abort the whole surface. Returning the error lets callers record the
// failure precisely.
func LastSeenID(cp connector.Checkpoint, surface, scope string) (int64, error) {
	key := CheckpointKey(surface, scope)
	v, ok := cp.Load(key)
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("checkpoint parse error key=%s value=%q: %w", key, v, err)
	}
	return n, nil
}

// SaveLastSeenID advances the checkpoint after a successful Commit().
// MUST be called only after Commit() returns nil per the framework
// per-item-checkpoint discipline (spec 05 §3.2).
func SaveLastSeenID(cp connector.Checkpoint, surface, scope string, id int64) error {
	return cp.Save(CheckpointKey(surface, scope), strconv.FormatInt(id, 10))
}

// StageDecision is the outcome of ShouldStage. Callers can label
// metrics by Decision.String() and decide whether to emit the item
// (only StageAccept means emit).
type StageDecision int

const (
	StageAccept        StageDecision = iota // id > last seen; emit
	StageSkipZero                           // item id was 0; ignore
	StageSkipDuplicate                      // id == last seen; likely a retry
	StageSkipBelow                          // id < last seen; likely out-of-order arrival
)

// String returns a snake_case label suitable for metric labels and log
// fields. Stable across releases (callers may key dashboards on it).
func (d StageDecision) String() string {
	switch d {
	case StageAccept:
		return "accept"
	case StageSkipZero:
		return "skip_zero_id"
	case StageSkipDuplicate:
		return "skip_duplicate"
	case StageSkipBelow:
		return "skip_below_checkpoint"
	}
	return "unknown"
}

// Accept reports whether the decision means "emit the item". Convenience
// for callsites that only care about the binary outcome.
func (d StageDecision) Accept() bool { return d == StageAccept }

// ShouldStage classifies an item's ID against the checkpoint for its
// surface. Returns an error when the checkpoint store can't be read
// (corrupted value); the processor convention is to skip the item
// with a slog.Error + metric increment and continue (see attachments.go),
// not abort the whole poll. The error is returned rather than swallowed
// so the caller can record it precisely.
//
// TODO: candidate for extraction to connector primitive base type
// (highest-ID dedup is shared with PowerSchool and future LMS connectors).
func ShouldStage(cp connector.Checkpoint, surface, scope string, id int64) (StageDecision, error) {
	if id == 0 {
		return StageSkipZero, nil
	}
	last, err := LastSeenID(cp, surface, scope)
	if err != nil {
		return 0, err
	}
	switch {
	case id > last:
		return StageAccept, nil
	case id == last:
		return StageSkipDuplicate, nil
	default:
		return StageSkipBelow, nil
	}
}

// ParseID parses a string ID into int64.
func ParseID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	return n, nil
}
