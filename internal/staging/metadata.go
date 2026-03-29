package staging

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
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

	if meta.AuthFailure {
		errs = append(errs, ValidationError{"auth_failure", "source authentication failed"})
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
