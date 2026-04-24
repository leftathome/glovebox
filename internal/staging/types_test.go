package staging

import (
	"encoding/json"
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

func TestItemMetadata_DataSubjectAndAudienceRoundtrip(t *testing.T) {
	original := ItemMetadata{
		Source:           "schoology",
		Sender:           "Mr. Rodriguez",
		Subject:          "Math quiz retakes",
		Timestamp:        time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "school",
		ContentType:      "text/plain",
		DataSubject:      "bee",
		Audience:         []string{"subject", "parents"},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"data_subject":"bee"`) {
		t.Errorf("marshaled JSON missing data_subject: %s", data)
	}
	if !strings.Contains(string(data), `"audience":["subject","parents"]`) {
		t.Errorf("marshaled JSON missing audience: %s", data)
	}

	var roundtripped ItemMetadata
	if err := json.Unmarshal(data, &roundtripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtripped.DataSubject != "bee" {
		t.Errorf("data_subject lost in roundtrip: got %q", roundtripped.DataSubject)
	}
	if len(roundtripped.Audience) != 2 || roundtripped.Audience[0] != "subject" || roundtripped.Audience[1] != "parents" {
		t.Errorf("audience lost in roundtrip: got %v", roundtripped.Audience)
	}
}

func TestItemMetadata_DataSubjectAndAudienceOmitempty(t *testing.T) {
	m := ItemMetadata{
		Source:           "rss",
		Sender:           "feed",
		Subject:          "title",
		Timestamp:        time.Unix(0, 0).UTC(),
		DestinationAgent: "general",
		ContentType:      "text/plain",
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "data_subject") {
		t.Errorf("expected data_subject omitted: %s", data)
	}
	if strings.Contains(string(data), "audience") {
		t.Errorf("expected audience omitted: %s", data)
	}
}
