package staging

import (
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

func hasError(errs []ValidationError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}
