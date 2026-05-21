package schoology

import (
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func TestReceiptDedup_FirstObservationReturnsTrue(t *testing.T) {
	d := NewReceiptDedup()
	if !d.Observe("feed_body_extractor", "empty_string", "item-1") {
		t.Error("first observation should return true")
	}
	if d.Observe("feed_body_extractor", "empty_string", "item-2") {
		t.Error("second observation should return false")
	}
	if got := d.AffectedCount("feed_body_extractor", "empty_string"); got != 2 {
		t.Errorf("affected count: got %d, want 2", got)
	}
	ids := d.AffectedItemIDs("feed_body_extractor", "empty_string")
	if len(ids) != 2 || ids[0] != "item-1" || ids[1] != "item-2" {
		t.Errorf("affected item ids: got %v", ids)
	}
}

func TestReceiptDedup_EmptyItemIDNotRecorded(t *testing.T) {
	d := NewReceiptDedup()
	d.Observe("parser", "class", "")
	ids := d.AffectedItemIDs("parser", "class")
	if len(ids) != 0 {
		t.Errorf("empty itemID should not be recorded, got %v", ids)
	}
}

func TestReceiptDedup_DistinctParsers(t *testing.T) {
	d := NewReceiptDedup()
	if !d.Observe("feed_body_extractor", "empty_string", "") {
		t.Error("first parser, first observation")
	}
	if !d.Observe("message_body_decoder", "empty_string", "") {
		t.Error("distinct parsers should each get their first-true")
	}
}

func TestBuildReceiptOptions_StructureMatchesSpec(t *testing.T) {
	opts := BuildReceiptOptions(ReceiptInputs{
		Parser:            "feed_body_extractor",
		ErrorClass:        "empty_string",
		ErrorMsg:          "feed body returned empty",
		Trace:             "schoology-go/feed.go:142 in ExtractBody",
		SourceURL:         "https://example.schoology.com/home/feed",
		TargetKid:         "k1",
		TargetContentType: "feed",
		LibVersion:        "v0.1.0",
		AffectedCount:     3,
		AffectedItemIDs:   []string{"100", "101", "102"},
	})
	if opts.Source != "schoology-parse-failure" {
		t.Errorf("Source: got %q", opts.Source)
	}
	if opts.ContentType != "application/octet-stream" {
		t.Errorf("ContentType: got %q", opts.ContentType)
	}
	if opts.DataSubject != "" {
		t.Errorf("DataSubject should be empty per spec §11.3, got %q", opts.DataSubject)
	}
	if opts.Tags["parse_status"] != "failure_receipt" {
		t.Errorf("parse_status tag: got %q", opts.Tags["parse_status"])
	}
	if opts.Tags["parser"] != "feed_body_extractor" {
		t.Errorf("parser tag: got %q", opts.Tags["parser"])
	}
	if opts.Tags["trace"] == "" {
		t.Errorf("trace tag must be present (spec §11.3 line 639)")
	}
	if opts.Tags["affected_count"] != "3" {
		t.Errorf("affected_count tag: got %q", opts.Tags["affected_count"])
	}
	if opts.Tags["affected_item_ids"] != "100,101,102" {
		t.Errorf("affected_item_ids tag: got %q, want \"100,101,102\"", opts.Tags["affected_item_ids"])
	}
	if len(opts.Audience) != 1 || opts.Audience[0] != "guardians" {
		t.Errorf("Audience: got %v", opts.Audience)
	}
	if !strings.Contains(opts.Subject, "[parse-failure]") {
		t.Errorf("Subject: got %q (should contain '[parse-failure]')", opts.Subject)
	}
}

func TestBuildReceiptOptions_TruncatesLongFields(t *testing.T) {
	long := strings.Repeat("x", 2048)
	opts := BuildReceiptOptions(ReceiptInputs{
		Parser:     "p",
		ErrorClass: "c",
		ErrorMsg:   long,
		Trace:      long,
	})
	if got := opts.Tags["error"]; len(got) != 1024 {
		t.Errorf("error tag len: got %d, want 1024", len(got))
	}
	if got := opts.Tags["trace"]; len(got) != 1024 {
		t.Errorf("trace tag len: got %d, want 1024", len(got))
	}
	if !strings.HasSuffix(opts.Tags["error"], "...") {
		t.Errorf("error tag should end with ellipsis")
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

func TestBuildDegradedOptions_DoesNotMutateBase(t *testing.T) {
	base := connector.ItemOptions{
		Source: "schoology",
		Tags: map[string]string{
			"existing": "value",
		},
	}
	_ = BuildDegradedOptions(base, "err", "fields", "id", "type")
	if _, exists := base.Tags["parse_status"]; exists {
		t.Errorf("base.Tags must not be mutated; got parse_status=%q", base.Tags["parse_status"])
	}
	if base.Tags["existing"] != "value" {
		t.Errorf("existing tag must be preserved; got %q", base.Tags["existing"])
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

func TestSummarize_Truncates(t *testing.T) {
	got := summarize("a very long single line that exceeds the cap by quite a bit honestly", 20)
	if len(got) != 20 {
		t.Errorf("summarize len: got %d, want 20", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("summarize should end with ellipsis, got %q", got)
	}
}

func TestSummarize_FirstLineOnly(t *testing.T) {
	got := summarize("line1\nline2\nline3", 100)
	if got != "line1" {
		t.Errorf("summarize should take first line, got %q", got)
	}
}
