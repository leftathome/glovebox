package staging

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var testAllowlist = []string{"messaging", "media", "calendar", "itinerary"}

func validMeta() ItemMetadata {
	return ItemMetadata{
		Source:           "email",
		Sender:           "alice@example.com",
		Timestamp:        time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	}
}

func TestValidate_AllValid(t *testing.T) {
	errs := Validate(validMeta(), testAllowlist)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidate_MissingSource(t *testing.T) {
	m := validMeta()
	m.Source = ""
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "source") {
		t.Error("expected error for missing source")
	}
}

func TestValidate_MissingSender(t *testing.T) {
	m := validMeta()
	m.Sender = ""
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "sender") {
		t.Error("expected error for missing sender")
	}
}

func TestValidate_MissingTimestamp(t *testing.T) {
	m := validMeta()
	m.Timestamp = time.Time{}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "timestamp") {
		t.Error("expected error for missing timestamp")
	}
}

func TestValidate_MissingDestinationAgent(t *testing.T) {
	m := validMeta()
	m.DestinationAgent = ""
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "destination_agent") {
		t.Error("expected error for missing destination_agent")
	}
}

func TestValidate_MissingContentType(t *testing.T) {
	m := validMeta()
	m.ContentType = ""
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "content_type") {
		t.Error("expected error for missing content_type")
	}
}

func TestValidate_InvalidDestinationAgent(t *testing.T) {
	m := validMeta()
	m.DestinationAgent = "hacking"
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "destination_agent") {
		t.Error("expected error for invalid destination_agent")
	}
}

func TestValidate_MultipleErrorsReported(t *testing.T) {
	m := ItemMetadata{}
	errs := Validate(m, testAllowlist)
	if len(errs) < 3 {
		t.Errorf("expected multiple errors, got %d: %v", len(errs), errs)
	}
}

func TestValidate_FieldLengthLimits(t *testing.T) {
	m := validMeta()
	m.Sender = string(make([]byte, 1025))
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "sender") {
		t.Error("expected error for sender exceeding 1024 chars")
	}
}

func TestValidate_ControlCharsRejected(t *testing.T) {
	m := validMeta()
	m.Sender = "attacker\x00@evil.com"
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "sender") {
		t.Error("expected error for control chars in sender")
	}
}

func TestValidate_AuthFailure(t *testing.T) {
	m := validMeta()
	m.AuthFailure = true
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "auth_failure") {
		t.Error("expected error for auth_failure")
	}
}

func TestStripSubjectControlChars(t *testing.T) {
	result := StripSubjectControlChars("hello\x00world\x01test")
	if result != "helloworldtest" {
		t.Errorf("got %q, want %q", result, "helloworldtest")
	}
}

func TestStripSubjectControlChars_PreservesNewlines(t *testing.T) {
	result := StripSubjectControlChars("hello\nworld\ttab")
	if result != "hello\nworld\ttab" {
		t.Errorf("got %q, want %q", result, "hello\nworld\ttab")
	}
}

// --- Identity validation tests ---

func TestValidate_ValidIdentity(t *testing.T) {
	m := validMeta()
	m.Identity = &ItemIdentity{
		AccountID:  "steve@github",
		Provider:   "github",
		AuthMethod: "oauth",
		Scopes:     []string{"repo", "read:org"},
		Tenant:     "steve",
	}
	errs := Validate(m, testAllowlist)
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid identity, got %v", errs)
	}
}

func TestValidate_NilIdentityPasses(t *testing.T) {
	m := validMeta()
	m.Identity = nil
	errs := Validate(m, testAllowlist)
	if len(errs) != 0 {
		t.Errorf("expected no errors for nil identity, got %v", errs)
	}
}

func TestValidate_OversizedAccountID(t *testing.T) {
	m := validMeta()
	m.Identity = &ItemIdentity{
		AccountID:  string(make([]byte, 1025)),
		Provider:   "github",
		AuthMethod: "oauth",
	}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "identity.account_id") {
		t.Error("expected error for oversized account_id")
	}
}

func TestValidate_OversizedProvider(t *testing.T) {
	m := validMeta()
	m.Identity = &ItemIdentity{
		Provider:   string(make([]byte, 65)),
		AuthMethod: "oauth",
	}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "identity.provider") {
		t.Error("expected error for oversized provider")
	}
}

func TestValidate_ControlCharsInAuthMethod(t *testing.T) {
	m := validMeta()
	m.Identity = &ItemIdentity{
		Provider:   "github",
		AuthMethod: "oauth\x00injection",
	}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "identity.auth_method") {
		t.Error("expected error for control chars in auth_method")
	}
}

func TestValidate_TooManyScopes(t *testing.T) {
	m := validMeta()
	scopes := make([]string, 33)
	for i := range scopes {
		scopes[i] = "scope"
	}
	m.Identity = &ItemIdentity{
		Provider:   "github",
		AuthMethod: "oauth",
		Scopes:     scopes,
	}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "identity.scopes") {
		t.Error("expected error for >32 scopes")
	}
}

func TestValidate_OversizedTenant(t *testing.T) {
	m := validMeta()
	m.Identity = &ItemIdentity{
		Provider:   "github",
		AuthMethod: "oauth",
		Tenant:     string(make([]byte, 257)),
	}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "identity.tenant") {
		t.Error("expected error for oversized tenant")
	}
}

// --- Tags validation tests ---

func TestValidate_NilTagsPasses(t *testing.T) {
	m := validMeta()
	m.Tags = nil
	errs := Validate(m, testAllowlist)
	if len(errs) != 0 {
		t.Errorf("expected no errors for nil tags, got %v", errs)
	}
}

func TestValidate_InvalidTagKeyWithSpace(t *testing.T) {
	m := validMeta()
	m.Tags = map[string]string{"bad key": "value"}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "tags") {
		t.Error("expected error for tag key containing space")
	}
}

func TestValidate_TagKeyTooLong(t *testing.T) {
	m := validMeta()
	key65 := strings.Repeat("a", 65)
	m.Tags = map[string]string{key65: "value"}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "tags") {
		t.Error("expected error for tag key >64 chars")
	}
}

func TestValidate_TagValueTooLong(t *testing.T) {
	m := validMeta()
	m.Tags = map[string]string{"key": string(make([]byte, 1025))}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "tags") {
		t.Error("expected error for tag value >1024 chars")
	}
}

func TestValidate_TooManyTags(t *testing.T) {
	m := validMeta()
	m.Tags = make(map[string]string)
	for i := 0; i < 33; i++ {
		m.Tags[fmt.Sprintf("key%d", i)] = "value"
	}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "tags") {
		t.Error("expected error for >32 tags")
	}
}

func TestValidate_TagValueControlChars(t *testing.T) {
	m := validMeta()
	m.Tags = map[string]string{"key": "value\x00injected"}
	errs := Validate(m, testAllowlist)
	if !hasError(errs, "tags") {
		t.Error("expected error for control chars in tag value")
	}
}

func TestValidate_ValidTags(t *testing.T) {
	m := validMeta()
	m.Tags = map[string]string{
		"team":        "platform",
		"env":         "production",
		"my-tag_v1.0": "some value",
	}
	errs := Validate(m, testAllowlist)
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid tags, got %v", errs)
	}
}

func hasError(errs []ValidationError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}
