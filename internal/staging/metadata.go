package staging

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// validTagKey matches alphanumeric characters, hyphens, underscores, and dots.
var validTagKey = regexp.MustCompile(`^[a-zA-Z0-9\-_.]+$`)

type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// RejectReasonFromError maps a ReadStagingItem error to a spec-defined reject reason.
func RejectReasonFromError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "auth_failure") {
		return "source_auth_failure"
	}
	if strings.Contains(msg, "not in allowlist") {
		return "unknown_destination"
	}
	if strings.Contains(msg, "metadata.json") {
		return "malformed_metadata"
	}
	if strings.Contains(msg, "content") {
		return "content_unreadable"
	}
	return "malformed_metadata"
}

// RejectReason returns the spec-defined reject reason for a set of validation errors.
func RejectReason(errs []ValidationError) string {
	for _, e := range errs {
		if e.Field == "auth_failure" {
			return "source_auth_failure"
		}
	}
	for _, e := range errs {
		if e.Field == "destination_agent" && e.Message != "required" {
			return "unknown_destination"
		}
	}
	return "malformed_metadata"
}

func Validate(meta ItemMetadata, allowlist []string) []ValidationError {
	var errs []ValidationError

	if meta.Source == "" {
		errs = append(errs, ValidationError{"source", "required"})
	}
	if meta.Sender == "" {
		errs = append(errs, ValidationError{"sender", "required"})
	}
	if meta.Timestamp.IsZero() {
		errs = append(errs, ValidationError{"timestamp", "required"})
	}
	if meta.DestinationAgent == "" {
		errs = append(errs, ValidationError{"destination_agent", "required"})
	}
	if meta.ContentType == "" {
		errs = append(errs, ValidationError{"content_type", "required"})
	}

	if meta.DestinationAgent != "" && !slices.Contains(allowlist, meta.DestinationAgent) {
		errs = append(errs, ValidationError{"destination_agent", fmt.Sprintf("not in allowlist: %q", meta.DestinationAgent)})
	}

	if len(meta.Source) > 1024 {
		errs = append(errs, ValidationError{"source", "exceeds 1024 characters"})
	}
	if len(meta.Sender) > 1024 {
		errs = append(errs, ValidationError{"sender", "exceeds 1024 characters"})
	}
	if len(meta.Subject) > 1024 {
		errs = append(errs, ValidationError{"subject", "exceeds 1024 characters"})
	}
	if len(meta.DestinationAgent) > 64 {
		errs = append(errs, ValidationError{"destination_agent", "exceeds 64 characters"})
	}
	if len(meta.ContentType) > 64 {
		errs = append(errs, ValidationError{"content_type", "exceeds 64 characters"})
	}

	if hasControlChars(meta.Source) {
		errs = append(errs, ValidationError{"source", "contains control characters"})
	}
	if hasControlChars(meta.Sender) {
		errs = append(errs, ValidationError{"sender", "contains control characters"})
	}
	if hasControlChars(meta.DestinationAgent) {
		errs = append(errs, ValidationError{"destination_agent", "contains control characters"})
	}
	if hasControlChars(meta.ContentType) {
		errs = append(errs, ValidationError{"content_type", "contains control characters"})
	}

	// Validate identity sub-fields when present.
	if meta.Identity != nil {
		errs = append(errs, validateIdentity(meta.Identity)...)
	}

	// Validate tags when present.
	if len(meta.Tags) > 0 {
		errs = append(errs, validateTags(meta.Tags)...)
	}

	// data_subject validation (spec 11 §6).
	if meta.DataSubject != "" {
		if len(meta.DataSubject) > 256 {
			errs = append(errs, ValidationError{"data_subject", "exceeds 256 characters"})
		}
		if hasControlChars(meta.DataSubject) {
			errs = append(errs, ValidationError{"data_subject", "contains control characters"})
		}
	}

	// audience validation (spec 11 §3.5). Delegates to the audience primitive
	// and converts any single error into a ValidationError entry.
	if err := ValidateAudience(meta.Audience, meta.DataSubject != ""); err != nil {
		errs = append(errs, ValidationError{"audience", err.Error()})
	}

	if meta.AuthFailure {
		errs = append(errs, ValidationError{"auth_failure", "source authentication failed"})
	}

	return errs
}

func validateIdentity(id *ItemIdentity) []ValidationError {
	var errs []ValidationError

	if len(id.Provider) > 64 {
		errs = append(errs, ValidationError{"identity.provider", "exceeds 64 characters"})
	}
	if hasControlChars(id.Provider) {
		errs = append(errs, ValidationError{"identity.provider", "contains control characters"})
	}

	if len(id.AuthMethod) > 64 {
		errs = append(errs, ValidationError{"identity.auth_method", "exceeds 64 characters"})
	}
	if hasControlChars(id.AuthMethod) {
		errs = append(errs, ValidationError{"identity.auth_method", "contains control characters"})
	}

	if len(id.AccountID) > 1024 {
		errs = append(errs, ValidationError{"identity.account_id", "exceeds 1024 characters"})
	}
	if hasControlChars(id.AccountID) {
		errs = append(errs, ValidationError{"identity.account_id", "contains control characters"})
	}

	if len(id.Tenant) > 256 {
		errs = append(errs, ValidationError{"identity.tenant", "exceeds 256 characters"})
	}
	if hasControlChars(id.Tenant) {
		errs = append(errs, ValidationError{"identity.tenant", "contains control characters"})
	}

	if len(id.Scopes) > 32 {
		errs = append(errs, ValidationError{"identity.scopes", "exceeds 32 entries"})
	}
	for i, scope := range id.Scopes {
		if len(scope) > 64 {
			errs = append(errs, ValidationError{"identity.scopes", fmt.Sprintf("scope[%d] exceeds 64 characters", i)})
		}
	}

	return errs
}

func validateTags(tags map[string]string) []ValidationError {
	var errs []ValidationError

	if len(tags) > 32 {
		errs = append(errs, ValidationError{"tags", fmt.Sprintf("exceeds 32 tags (has %d)", len(tags))})
	}

	for k, v := range tags {
		if len(k) > 64 {
			errs = append(errs, ValidationError{"tags", fmt.Sprintf("key %q exceeds 64 characters", k)})
		}
		if !validTagKey.MatchString(k) {
			errs = append(errs, ValidationError{"tags", fmt.Sprintf("key %q contains invalid characters (allowed: alphanumeric, -, _, .)", k)})
		}
		if len(v) > 1024 {
			errs = append(errs, ValidationError{"tags", fmt.Sprintf("value for key %q exceeds 1024 characters", k)})
		}
		if hasControlChars(v) {
			errs = append(errs, ValidationError{"tags", fmt.Sprintf("value for key %q contains control characters", k)})
		}
	}

	return errs
}

// IsUnsafeControl returns true for control characters that are not
// permitted in metadata fields (everything except \n, \r, \t).
func IsUnsafeControl(r rune) bool {
	return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
}

// StripSubjectControlChars removes control characters from the subject field.
// Subject is treated differently: control chars are stripped, not rejected.
func StripSubjectControlChars(subject string) string {
	return strings.Map(func(r rune) rune {
		if IsUnsafeControl(r) {
			return -1
		}
		return r
	}, subject)
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if IsUnsafeControl(r) {
			return true
		}
	}
	return false
}

// HasControlChars is the exported wrapper around the package-internal
// control-char predicate. Used by the connector package's config-load
// validator. Whitelists \n \r \t per the internal policy.
func HasControlChars(s string) bool {
	return hasControlChars(s)
}
