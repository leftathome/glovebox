package schoology

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

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

// seenKey is the checkpoint key holding the set of item IDs already staged
// for a surface/scope.
func seenKey(surface, scope string) string {
	if scope == "" {
		return surface + ":seen"
	}
	return surface + ":" + scope + ":seen"
}

// maxSeenIDs bounds the seen-set. When it overflows the smallest IDs are
// dropped; Schoology IDs grow over time, so those are the oldest items and
// the least likely to be listed again.
const maxSeenIDs = 5000

// seenIDs loads the set of already-staged IDs. A missing key is an empty
// set; an unparseable entry is an error (same contract as LastSeenID).
func seenIDs(cp connector.Checkpoint, surface, scope string) (map[int64]struct{}, error) {
	key := seenKey(surface, scope)
	set := map[int64]struct{}{}
	v, ok := cp.Load(key)
	if !ok || v == "" {
		return set, nil
	}
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("checkpoint parse error key=%s value=%q: %w", key, part, err)
		}
		set[n] = struct{}{}
	}
	return set, nil
}

// SaveLastSeenID records an item as staged after a successful Commit().
// MUST be called only after Commit() returns nil per the framework
// per-item-checkpoint discipline (spec 05 §3.2).
//
// It adds the ID to the surface's seen-set and keeps the legacy
// "<surface>:<scope>:last_id" key at the highest ID seen, which LastSeenID
// and existing dashboards read.
func SaveLastSeenID(cp connector.Checkpoint, surface, scope string, id int64) error {
	set, err := seenIDs(cp, surface, scope)
	if err != nil {
		// A corrupt set must not block progress forever: start a new one.
		set = map[int64]struct{}{}
	}
	set[id] = struct{}{}
	ids := make([]int64, 0, len(set))
	for n := range set {
		ids = append(ids, n)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > maxSeenIDs {
		ids = ids[len(ids)-maxSeenIDs:]
	}
	parts := make([]string, len(ids))
	for i, n := range ids {
		parts[i] = strconv.FormatInt(n, 10)
	}
	if err := cp.Save(seenKey(surface, scope), strings.Join(parts, ",")); err != nil {
		return err
	}
	last, err := LastSeenID(cp, surface, scope)
	if err != nil || id > last {
		return cp.Save(CheckpointKey(surface, scope), strconv.FormatInt(id, 10))
	}
	return nil
}

// StageDecision is the outcome of ShouldStage. Callers can label
// metrics by Decision.String() and decide whether to emit the item
// (only StageAccept means emit).
type StageDecision int

const (
	StageAccept        StageDecision = iota // id not staged before; emit
	StageSkipZero                           // item id was 0; ignore
	StageSkipDuplicate                      // id already staged
	StageSkipBelow                          // retired: no longer returned (kept for label stability)
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
	// Validate the legacy high-water key too, so a corrupt value is still
	// surfaced to the caller as before.
	if _, err := LastSeenID(cp, surface, scope); err != nil {
		return 0, err
	}
	seen, err := seenIDs(cp, surface, scope)
	if err != nil {
		return 0, err
	}
	if _, ok := seen[id]; ok {
		return StageSkipDuplicate, nil
	}
	// Not "id > highest seen": Schoology lists items in page order (the
	// feed is newest first, overdue work by due date), not ID order. A
	// single high-water mark accepted the first item and rejected every
	// lower ID after it -- on a live account, one feed post in ten -- and
	// could never emit an older assignment that became overdue later.
	return StageAccept, nil
}

// ParseID parses a string ID into int64.
func ParseID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	return n, nil
}
