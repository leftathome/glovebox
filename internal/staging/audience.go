package staging

import "fmt"

// Audience role tokens per spec 11 §3.4.
const (
	AudienceSubject   = "subject"
	AudienceParents   = "parents"
	AudienceSiblings  = "siblings"
	AudienceHousehold = "household"
	AudiencePublic    = "public"
)

const maxAudienceEntries = 16

var validAudienceTokens = map[string]bool{
	AudienceSubject:   true,
	AudienceParents:   true,
	AudienceSiblings:  true,
	AudienceHousehold: true,
	AudiencePublic:    true,
}

// roleRelativeTokens are tokens that require a data_subject to be meaningful
// per spec 11 §3.5.
var roleRelativeTokens = map[string]bool{
	AudienceSubject:  true,
	AudienceParents:  true,
	AudienceSiblings: true,
}

// ValidateAudience enforces the spec 11 §3.5 cross-field rules on an audience
// slice. A nil slice is treated as "not set" and returns nil. An empty but
// non-nil slice is rejected. Check order: length cap > token recognition >
// duplicate > token-specific standalone rules > cross-field.
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
	hasRoleRelative := false

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
		if roleRelativeTokens[tok] {
			hasRoleRelative = true
		}
	}

	if hasPublic && len(audience) > 1 {
		return fmt.Errorf("public must appear alone in audience")
	}
	if hasHousehold && hasRoleRelative {
		return fmt.Errorf("household must appear alone; it already includes subject/parents/siblings")
	}
	if !hasDataSubject && hasRoleRelative {
		return fmt.Errorf("audience token requires data_subject to be set")
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
