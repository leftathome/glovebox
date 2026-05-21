package schoology

import (
	"fmt"
	"log/slog"
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

// LastSeenID reads the highest-seen ID for a content surface. Returns 0
// when the checkpoint is fresh (i.e., first poll).
func LastSeenID(cp connector.Checkpoint, surface, scope string) int64 {
	v, ok := cp.Load(CheckpointKey(surface, scope))
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		slog.Warn("schoology checkpoint parse error",
			"key", CheckpointKey(surface, scope), "value", v, "error", err)
		return 0
	}
	return n
}

// SaveLastSeenID advances the checkpoint after a successful Commit().
// MUST be called only after Commit() returns nil per the framework
// per-item-checkpoint discipline (spec 05 §3.2).
func SaveLastSeenID(cp connector.Checkpoint, surface, scope string, id int64) error {
	return cp.Save(CheckpointKey(surface, scope), strconv.FormatInt(id, 10))
}

// ShouldStage returns true when an item's ID is strictly greater than
// the checkpoint. When false (id <= last seen), logs a warning if id
// is non-zero but below the threshold (likely out-of-order arrival).
// The dropped-below-checkpoint metric is incremented by the caller
// since it owns the connector's Metrics handle.
//
// TODO: candidate for extraction to connector primitive base type
// (highest-ID dedup is shared with PowerSchool and future LMS connectors).
func ShouldStage(cp connector.Checkpoint, surface, scope string, id int64) bool {
	if id == 0 {
		return false
	}
	last := LastSeenID(cp, surface, scope)
	if id > last {
		return true
	}
	if last > 0 && id < last {
		slog.Warn("schoology item below checkpoint",
			"surface", surface, "scope", scope,
			"item_id", id, "checkpoint", last)
	}
	return false
}

// FormatID parses a string ID into int64.
func FormatID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	return n, nil
}
