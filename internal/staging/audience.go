package staging

import "fmt"

// Audience role tokens per spec 11 §3.4. Renamed in v0.5.0: AudienceParents
// was the v0.4.0 name; v0.5.0 uses AudienceGuardians (matches school and
// legal terminology; v0.4.0's §3.4 table already documented the token as
// "parents/guardians" parenthetically). AudienceCaregivers was added in
// v0.5.0 for delegated supervisors (tutors, nannies, AI agents in
// caretaking roles, out-of-household relatives on duty); see spec 11
// v1.2 §3.1 glossary for the guardians-vs-caregivers distinction.
const (
	AudienceSubject    = "subject"
	AudienceGuardians  = "guardians"
	AudienceSiblings   = "siblings"
	AudienceHousehold  = "household"
	AudienceCaregivers = "caregivers"
	AudiencePublic     = "public"
)

const maxAudienceEntries = 16

var validAudienceTokens = map[string]bool{
	AudienceSubject:    true,
	AudienceGuardians:  true,
	AudienceSiblings:   true,
	AudienceHousehold:  true,
	AudienceCaregivers: true,
	AudiencePublic:     true,
}

// householdSubsetTokens are members of `household`; they cannot appear
// alongside `household` itself (would be redundant). Per spec 11 §3.5.
var householdSubsetTokens = map[string]bool{
	AudienceSubject:   true,
	AudienceGuardians: true,
	AudienceSiblings:  true,
}

// subjectRelativeTokens require data_subject to be meaningful. Per spec 11
// v1.2 §3.5: guardians and caregivers may stand alone (household-scope
// interpretation), but subject and siblings still need data_subject.
var subjectRelativeTokens = map[string]bool{
	AudienceSubject:  true,
	AudienceSiblings: true,
}

// ValidateAudience enforces the spec 11 §3.5 cross-field rules on an audience
// slice. A nil slice is treated as "not set" and returns nil. An empty but
// non-nil slice is rejected. Per spec 11 v1.2: `guardians` and `caregivers`
// may stand alone without `data_subject` (household-scope interpretation);
// `subject` and `siblings` still require `data_subject`. The combination
// [household, caregivers] is permitted (caregivers are orthogonal to
// household). Check order: length cap > token recognition > duplicate >
// public-alone > household-subset > subject-relative-requires-subject.
func ValidateAudience(audience []string, hasDataSubject bool) error {
	if audience == nil {
		return nil
	}
	if len(audience) == 0 {
		return fmt.Errorf("audience must be omitted entirely, not empty")
	}
	if len(audience) > maxAudienceEntries {
		return fmt.Errorf("audience has too many entries (max %d)", maxAudienceEntries)
	}

	seen := make(map[string]bool, len(audience))
	hasPublic := false
	hasHousehold := false
	hasHouseholdSubset := false
	hasSubjectRelative := false

	for _, tok := range audience {
		if !validAudienceTokens[tok] {
			return fmt.Errorf("unknown audience token %q", tok)
		}
		if seen[tok] {
			return fmt.Errorf("duplicate audience token %q", tok)
		}
		seen[tok] = true
		switch tok {
		case AudiencePublic:
			hasPublic = true
		case AudienceHousehold:
			hasHousehold = true
		}
		if householdSubsetTokens[tok] {
			hasHouseholdSubset = true
		}
		if subjectRelativeTokens[tok] {
			hasSubjectRelative = true
		}
	}

	if hasPublic && len(audience) > 1 {
		return fmt.Errorf("public must appear alone in audience")
	}
	if hasHousehold && hasHouseholdSubset {
		return fmt.Errorf("household must appear alone with its subsets (subject/guardians/siblings); only caregivers may coexist")
	}
	if !hasDataSubject && hasSubjectRelative {
		return fmt.Errorf("audience token requires data_subject to be set (subject/siblings)")
	}

	return nil
}

// EffectiveAudience returns the audience as consumers should interpret it,
// applying the spec 11 §3.6 default (["household"]) when the field was
// omitted. Callers should use this rather than reading m.Audience directly.
func EffectiveAudience(m ItemMetadata) []string {
	if m.Audience == nil {
		return []string{AudienceHousehold}
	}
	return m.Audience
}
