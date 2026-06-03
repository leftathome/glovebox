package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/leftathome/glovebox/internal/staging"
	"github.com/leftathome/glovebox/internal/subject"
)

// ReasonSubjectUnresolved is the quarantine reason recorded when an item
// declares a data subject the known-subjects registry cannot resolve and
// enforcement is on (spec 15 sec 5.3).
const ReasonSubjectUnresolved = "subject_unresolved"

// GateAction is the routing decision produced by the subject-resolution gate.
type GateAction int

const (
	// GatePass means the item may continue to the normal scan-score
	// pass/quarantine decision. Used for resolved items (metadata rewritten
	// to the entity_id), subjectless items (untouched), and unresolved items
	// when enforcement is off.
	GatePass GateAction = iota
	// GateQuarantine means the item declared a subject the registry cannot
	// resolve and enforcement is on; the caller must quarantine with
	// ReasonSubjectUnresolved.
	GateQuarantine
	// GatePassUnresolved means the item declared a subject the registry cannot
	// resolve but enforcement is off; the item proceeds to normal routing and
	// the caller should log a warning. The metadata is left as the original
	// principal.
	GatePassUnresolved
)

// SubjectGate runs the fail-closed subject-resolution gate over a staging item
// before the pass/quarantine routing decision. It resolves a producer-asserted
// principal in item.Metadata.DataSubject to its canonical entity_id and fills
// the registry default audience when the item declared none, mutating
// item.Metadata in place AND persisting the rewritten metadata.json to
// item.DirPath so the destination copy (written verbatim by RoutePass) carries
// the resolved entity_id.
//
// Outcomes:
//   - Subjectless (no data_subject): GatePass, metadata untouched.
//   - Resolved: GatePass, metadata rewritten to entity_id + default audience,
//     persisted to disk.
//   - Unresolved + enforce on: GateQuarantine.
//   - Unresolved + enforce off: GatePassUnresolved, metadata left as the
//     original principal (caller logs a warning).
//   - persistMetadata failure: GateQuarantine (fail-closed -- if the resolved
//     entity_id cannot be persisted the destination copy would carry a stale
//     principal; quarantine is the safe outcome).
//
// reg may be nil; ResolveItem and reg.Enforce() are nil-safe, so a nil registry
// treats any declared subject as unresolved (fail closed when enforce, which a
// nil registry never is).
func SubjectGate(item *staging.StagingItem, reg *subject.SubjectRegistry) (GateAction, error) {
	switch subject.ResolveItem(&item.Metadata, reg) {
	case subject.OutcomeResolved:
		// Persist the rewritten metadata so the destination copy and audit
		// carry the resolved entity_id, not the principal.
		if err := persistMetadata(item); err != nil {
			// Fail-closed: if we cannot persist the resolved entity_id the
			// destination copy would see a stale principal. Quarantine so
			// the caller's err != nil branch (RouteToFailed) handles it
			// regardless, but GateQuarantine is the safe action should a
			// future caller ever check the action without checking the error.
			return GateQuarantine, fmt.Errorf("persist resolved metadata: %w", err)
		}
		return GatePass, nil
	case subject.OutcomeUnresolved:
		if reg.Enforce() {
			return GateQuarantine, nil
		}
		return GatePassUnresolved, nil
	default: // OutcomeSubjectless
		return GatePass, nil
	}
}

// persistMetadata rewrites item.DirPath/metadata.json from item.Metadata using
// an atomic temp+fsync+rename pattern so an observer never sees a partial file.
//
// TODO: switch to internal/atomicfile.WriteFile once that package is tracked (mirrors finalize.go).
func persistMetadata(item *staging.StagingItem) error {
	data, err := json.Marshal(item.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	path := filepath.Join(item.DirPath, "metadata.json")
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("create temp metadata: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp metadata: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp metadata: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp metadata: %w", err)
	}
	return nil
}
