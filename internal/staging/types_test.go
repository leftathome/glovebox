package staging

import (
	"strings"
	"testing"
	"time"
)

func TestParseMetadata_ValidJSON(t *testing.T) {
	input := `{
		"source": "email",
		"sender": "alice@example.com",
		"subject": "Hello",
		"timestamp": "2026-03-28T12:00:00Z",
		"destination_agent": "messaging",
		"content_type": "text/plain",
		"ordered": true
	}`
	meta, err := ParseMetadata(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Source != "email" {
		t.Errorf("source = %q, want %q", meta.Source, "email")
	}
	if meta.Sender != "alice@example.com" {
		t.Errorf("sender = %q, want %q", meta.Sender, "alice@example.com")
	}
	if meta.Subject != "Hello" {
		t.Errorf("subject = %q, want %q", meta.Subject, "Hello")
	}
	if meta.DestinationAgent != "messaging" {
		t.Errorf("destination_agent = %q, want %q", meta.DestinationAgent, "messaging")
	}
	if meta.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want %q", meta.ContentType, "text/plain")
	}
	if !meta.Ordered {
		t.Error("ordered = false, want true")
	}
	expectedTime, _ := time.Parse(time.RFC3339, "2026-03-28T12:00:00Z")
	if !meta.Timestamp.Equal(expectedTime) {
		t.Errorf("timestamp = %v, want %v", meta.Timestamp, expectedTime)
	}
}

func TestParseMetadata_InvalidJSON(t *testing.T) {
	_, err := ParseMetadata(strings.NewReader("{not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseMetadata_UnknownFieldsIgnored(t *testing.T) {
	input := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"messaging","content_type":"text/plain","extra_field":"ignored"}`
	_, err := ParseMetadata(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseMetadata_OrderedDefaultsFalse(t *testing.T) {
	input := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"messaging","content_type":"text/plain"}`
	meta, err := ParseMetadata(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Ordered {
		t.Error("ordered should default to false")
	}
}

func TestParseMetadata_AuthFailureField(t *testing.T) {
	input := `{"source":"email","sender":"a@b.com","timestamp":"2026-03-28T12:00:00Z","destination_agent":"messaging","content_type":"text/plain","auth_failure":true}`
	meta, err := ParseMetadata(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !meta.AuthFailure {
		t.Error("auth_failure = false, want true")
	}
}
