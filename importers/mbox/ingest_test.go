package main

import (
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func newTestMatcher(rules []connector.Rule) *connector.RuleMatcher {
	return connector.NewRuleMatcher(rules)
}

func TestBuildItemOptions_RoutesByGmailLabel(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
		{Match: "*", Destination: "general"},
	})

	msg := &Message{
		From:        "alice@example.com",
		Subject:     "Hello",
		Date:        time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC),
		GmailLabels: []string{"INBOX", "Important"},
		ByteOffset:  1024,
	}

	opts, err := BuildItemOptions(msg, matcher, "takeout-2026", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.DestinationAgent != "messaging" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "messaging")
	}
}

func TestBuildItemOptions_FallsThroughToWildcard(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
		{Match: "*", Destination: "general"},
	})

	msg := &Message{
		From:        "bob@example.com",
		Subject:     "Trash me",
		GmailLabels: []string{"Trash"},
		ByteOffset:  2048,
	}

	opts, err := BuildItemOptions(msg, matcher, "takeout-2026", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.DestinationAgent != "general" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "general")
	}
}

func TestBuildItemOptions_ErrorsWhenNoMatchAndNoWildcard(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "label:INBOX", Destination: "messaging"},
	})

	msg := &Message{
		From:        "carol@example.com",
		Subject:     "Orphan",
		GmailLabels: []string{"RandomLabel"},
		ByteOffset:  4096,
	}

	_, err := BuildItemOptions(msg, matcher, "takeout-2026", nil)
	if err == nil {
		t.Fatalf("expected error when no rule matches and no wildcard, got nil")
	}
	if !strings.Contains(err.Error(), "destination_agent") {
		t.Errorf("error %q does not mention destination_agent", err.Error())
	}
}

func TestBuildItemOptions_ErrorsWhenRuleDestinationEmpty(t *testing.T) {
	// Wildcard rule present but destination empty. Ingest would reject,
	// so we should fail early.
	matcher := newTestMatcher([]connector.Rule{
		{Match: "*", Destination: ""},
	})

	msg := &Message{From: "d@example.com", GmailLabels: []string{"X"}}

	_, err := BuildItemOptions(msg, matcher, "src", nil)
	if err == nil {
		t.Fatalf("expected error for empty destination, got nil")
	}
}

func TestBuildItemOptions_OriginArchiveTagFormat(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "*", Destination: "general"},
	})

	msg := &Message{
		From:       "alice@example.com",
		ByteOffset: 12345678,
	}

	opts, err := BuildItemOptions(msg, matcher, "takeout-2026-04-11", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := opts.Tags["origin_archive"]
	if !ok {
		t.Fatalf("origin_archive tag missing; tags=%v", opts.Tags)
	}
	want := "takeout-2026-04-11:12345678"
	if got != want {
		t.Errorf("origin_archive = %q, want %q", got, want)
	}
}

func TestBuildItemOptions_PopulatesStandardFields(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "*", Destination: "general"},
	})

	ts := time.Date(2026, 4, 11, 10, 30, 0, 0, time.UTC)
	msg := &Message{
		From:       "alice@example.com",
		Subject:    "Quarterly report",
		Date:       ts,
		ByteOffset: 42,
	}

	opts, err := BuildItemOptions(msg, matcher, "mbox-src", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Source != "mbox-src" {
		t.Errorf("Source = %q, want %q", opts.Source, "mbox-src")
	}
	if opts.Sender != "alice@example.com" {
		t.Errorf("Sender = %q, want %q", opts.Sender, "alice@example.com")
	}
	if opts.Subject != "Quarterly report" {
		t.Errorf("Subject = %q, want %q", opts.Subject, "Quarterly report")
	}
	if !opts.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", opts.Timestamp, ts)
	}
	if opts.ContentType != "message/rfc822" {
		t.Errorf("ContentType = %q, want %q", opts.ContentType, "message/rfc822")
	}
	if opts.DestinationAgent != "general" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "general")
	}
}

func TestBuildItemOptions_ZeroTimestampAccepted(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "*", Destination: "general"},
	})
	msg := &Message{From: "a@example.com"}

	opts, err := BuildItemOptions(msg, matcher, "src", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.Timestamp.IsZero() {
		t.Errorf("expected zero timestamp, got %v", opts.Timestamp)
	}
}

func TestBuildItemOptions_FixedTagsMergeAndDedup(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{
			Match:       "label:INBOX",
			Destination: "messaging",
			Tags:        map[string]string{"priority": "high", "team": "platform"},
		},
	})

	msg := &Message{
		From:        "a@example.com",
		GmailLabels: []string{"INBOX"},
		ByteOffset:  7,
	}

	fixed := []string{
		"env=prod",
		"team=ops",  // duplicate key: later entries win within fixed tags
		"bare-key",  // no '=' -> key with empty value
		"ignore-me", // another bare key
	}

	opts, err := BuildItemOptions(msg, matcher, "src", fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// RuleTags preserved from the matched rule.
	if opts.RuleTags["priority"] != "high" {
		t.Errorf("RuleTags[priority] = %q, want %q", opts.RuleTags["priority"], "high")
	}
	if opts.RuleTags["team"] != "platform" {
		t.Errorf("RuleTags[team] = %q, want %q", opts.RuleTags["team"], "platform")
	}

	// Fixed tags applied with later-wins semantics for duplicate keys.
	if opts.Tags["env"] != "prod" {
		t.Errorf("Tags[env] = %q, want %q", opts.Tags["env"], "prod")
	}
	if opts.Tags["team"] != "ops" {
		t.Errorf("Tags[team] = %q, want %q (last fixedTag wins)", opts.Tags["team"], "ops")
	}
	if _, ok := opts.Tags["bare-key"]; !ok {
		t.Errorf("Tags missing bare-key; got %v", opts.Tags)
	}

	// origin_archive always present.
	if _, ok := opts.Tags["origin_archive"]; !ok {
		t.Errorf("Tags missing origin_archive; got %v", opts.Tags)
	}
}

func TestBuildItemOptions_RoutesByListID(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "list_id:dev@oldjob.example.com", Destination: "archive"},
		{Match: "*", Destination: "general"},
	})

	msg := &Message{
		From:       "someone@oldjob.example.com",
		ListID:     "dev@oldjob.example.com",
		ByteOffset: 9,
	}

	opts, err := BuildItemOptions(msg, matcher, "src", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.DestinationAgent != "archive" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "archive")
	}
}

func TestBuildItemOptions_RoutesBySender(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{Match: "sender:alerts@example.com", Destination: "alerts"},
		{Match: "*", Destination: "general"},
	})

	msg := &Message{
		From:       "alerts@example.com",
		ByteOffset: 11,
	}

	opts, err := BuildItemOptions(msg, matcher, "src", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.DestinationAgent != "alerts" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "alerts")
	}
}

func TestBuildItemOptions_NilMessageReturnsError(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{{Match: "*", Destination: "general"}})
	if _, err := BuildItemOptions(nil, matcher, "src", nil); err == nil {
		t.Fatal("expected error for nil message")
	}
}

func TestBuildItemOptions_NilMatcherReturnsError(t *testing.T) {
	msg := &Message{From: "a@example.com"}
	if _, err := BuildItemOptions(msg, nil, "src", nil); err == nil {
		t.Fatal("expected error for nil matcher")
	}
}

// TestBuildItemOptions_MergedMetadataPassesValidation verifies that the
// ItemOptions produced here can actually be staged (i.e., the tag keys we
// produce pass internal/staging.Validate). This guards against regressions
// like using ':' in a tag key, which validTagKey rejects.
func TestBuildItemOptions_MergedMetadataPassesValidation(t *testing.T) {
	matcher := newTestMatcher([]connector.Rule{
		{
			Match:       "*",
			Destination: "general",
			Tags:        map[string]string{"priority": "high"},
		},
	})

	msg := &Message{
		From:       "alice@example.com",
		Subject:    "ok",
		Date:       time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC),
		ByteOffset: 123,
	}

	opts, err := BuildItemOptions(msg, matcher, "takeout-2026-04-11", []string{"env=prod"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Every produced tag key must be valid per the metadata rules:
	// alphanumeric, '-', '_', '.', and max 64 chars. The merged map is
	// what buildMetadata will see.
	for k := range opts.Tags {
		if len(k) == 0 {
			t.Errorf("empty tag key in Tags=%v", opts.Tags)
		}
		if len(k) > 64 {
			t.Errorf("tag key %q exceeds 64 chars", k)
		}
		for _, r := range k {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
			if !ok {
				t.Errorf("tag key %q contains invalid char %q", k, r)
			}
		}
	}
	for k := range opts.RuleTags {
		if len(k) == 0 || len(k) > 64 {
			t.Errorf("invalid RuleTag key %q", k)
		}
	}
}
