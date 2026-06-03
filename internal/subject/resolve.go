package subject

import "github.com/leftathome/glovebox/internal/staging"

// Outcome is the result of resolving a data subject on a staging item.
type Outcome int

const (
	OutcomeSubjectless Outcome = iota // no data_subject; not our concern
	OutcomeResolved                   // mapped to a known entity_id
	OutcomeUnresolved                 // declared a subject we don't know
)

// ResolveItem rewrites meta.DataSubject from a principal to its entity_id and
// fills meta.Audience from the registry default when the item declared none.
// It never references display (spec 15 sec 5.1).
//
// A nil registry causes any non-empty DataSubject to return OutcomeUnresolved
// (fail closed). Resolve on a nil *SubjectRegistry returns ("", false), which
// propagates naturally without a redundant nil check here.
func ResolveItem(meta *staging.ItemMetadata, reg *SubjectRegistry) Outcome {
	if meta == nil || meta.DataSubject == "" {
		return OutcomeSubjectless
	}
	entityID, ok := reg.Resolve(meta.DataSubject)
	if !ok {
		return OutcomeUnresolved
	}
	meta.DataSubject = entityID
	if len(meta.Audience) == 0 {
		meta.Audience = reg.DefaultAudience(entityID)
	}
	return OutcomeResolved
}
