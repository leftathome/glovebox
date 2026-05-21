package schoology

import (
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func TestReceiptDedup_FirstObservationReturnsTrue(t *testing.T) {
	d := NewReceiptDedup()
	if !d.Observe("feed_body_extractor", "empty_string", []byte("sample 1")) {
		t.Error("first observation should return true")
	}
	if d.Observe("feed_body_extractor", "empty_string", []byte("sample 2")) {
		t.Error("second observation should return false")
	}
	if got := d.AffectedCount("feed_body_extractor", "empty_string"); got != 2 {
		t.Errorf("affected count: got %d, want 2", got)
	}
}

func TestReceiptDedup_DistinctParsers(t *testing.T) {
	d := NewReceiptDedup()
	if !d.Observe("feed_body_extractor", "empty_string", nil) {
		t.Error("first parser, first observation")
	}
	if !d.Observe("message_body_decoder", "empty_string", nil) {
		t.Error("distinct parsers should each get their first-true")
	}
}

func TestBuildReceiptOptions_StructureMatchesSpec(t *testing.T) {
	opts := BuildReceiptOptions(
		"feed_body_extractor",
		"empty_string",
		"feed body returned empty",
		"https://example.schoology.com/home/feed",
		"k1",
		"feed",
		"v0.1.0",
		[]byte("..."),
		3,
	)
	if opts.Source != "schoology-parse-failure" {
		t.Errorf("Source: got %q", opts.Source)
	}
	if opts.ContentType != "application/octet-stream" {
		t.Errorf("ContentType: got %q", opts.ContentType)
	}
	if opts.Tags["parse_status"] != "failure_receipt" {
		t.Errorf("parse_status tag: got %q", opts.Tags["parse_status"])
	}
	if opts.Tags["parser"] != "feed_body_extractor" {
		t.Errorf("parser tag: got %q", opts.Tags["parser"])
	}
	if opts.Tags["affected_count"] != "3" {
		t.Errorf("affected_count tag: got %q", opts.Tags["affected_count"])
	}
	if len(opts.Audience) != 1 || opts.Audience[0] != "guardians" {
		t.Errorf("Audience: got %v", opts.Audience)
	}
	if !strings.Contains(opts.Subject, "[parse-failure]") {
		t.Errorf("Subject: got %q (should contain '[parse-failure]')", opts.Subject)
	}
}

func TestBuildDegradedOptions_AddsTags(t *testing.T) {
	base := connector.ItemOptions{
		Source:           "schoology",
		Sender:           "test",
		Subject:          "test",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "school",
		ContentType:      "text/plain",
	}
	degraded := BuildDegradedOptions(base, "body extractor empty", "body", "12345", "feed")
	if degraded.Tags["parse_status"] != "degraded" {
		t.Errorf("got %q", degraded.Tags["parse_status"])
	}
	if degraded.Tags["parse_missing_fields"] != "body" {
		t.Errorf("got %q", degraded.Tags["parse_missing_fields"])
	}
	if degraded.Tags["schoology_item_id"] != "12345" {
		t.Errorf("got %q", degraded.Tags["schoology_item_id"])
	}
}

func TestSchemaDriftCounter_EscalatesAfterThreshold(t *testing.T) {
	c := NewSchemaDriftCounter(3)
	c.RecordPoll(0, true)
	c.RecordPoll(0, true)
	if c.ShouldEscalate() {
		t.Error("should not escalate yet")
	}
	c.RecordPoll(0, true)
	if !c.ShouldEscalate() {
		t.Error("should escalate at threshold")
	}
}

func TestSchemaDriftCounter_ResetsOnSuccess(t *testing.T) {
	c := NewSchemaDriftCounter(3)
	c.RecordPoll(0, true)
	c.RecordPoll(0, true)
	c.RecordPoll(5, false)
	c.RecordPoll(0, true)
	if c.ShouldEscalate() {
		t.Error("should not escalate after successful poll reset")
	}
}

func TestSchemaDriftCounter_NoIncrementOnQuietPoll(t *testing.T) {
	c := NewSchemaDriftCounter(3)
	c.RecordPoll(0, false)
	c.RecordPoll(0, false)
	c.RecordPoll(0, false)
	if c.ShouldEscalate() {
		t.Error("quiet polls (no errors) should not escalate")
	}
}
