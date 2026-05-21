package connector

import "testing"

func TestRuleMatcher_ExactMatch(t *testing.T) {
	rm := NewRuleMatcher([]Rule{
		{Match: "folder:INBOX", Destination: "messaging", Tags: map[string]string{"priority": "high"}},
		{Match: "folder:Calendar", Destination: "calendar", Tags: map[string]string{"category": "events"}},
	})
	result, ok := rm.Match("folder:INBOX")
	if !ok {
		t.Fatal("expected match, got no match")
	}
	if result.Destination != "messaging" {
		t.Errorf("destination = %q, want %q", result.Destination, "messaging")
	}
	if result.Tags["priority"] != "high" {
		t.Errorf("tags[priority] = %q, want %q", result.Tags["priority"], "high")
	}
}

func TestRuleMatcher_WildcardMatch(t *testing.T) {
	rm := NewRuleMatcher([]Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
		{Match: "*", Destination: "default-agent", Tags: map[string]string{"env": "production"}},
	})
	result, ok := rm.Match("folder:Unknown")
	if !ok {
		t.Fatal("expected wildcard match, got no match")
	}
	if result.Destination != "default-agent" {
		t.Errorf("destination = %q, want %q", result.Destination, "default-agent")
	}
	if result.Tags["env"] != "production" {
		t.Errorf("tags[env] = %q, want %q", result.Tags["env"], "production")
	}
}

func TestRuleMatcher_NoMatch(t *testing.T) {
	rm := NewRuleMatcher([]Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	})
	_, ok := rm.Match("folder:Spam")
	if ok {
		t.Error("expected no match, got a match")
	}
}

func TestRuleMatcher_FirstMatchWins(t *testing.T) {
	rm := NewRuleMatcher([]Rule{
		{Match: "folder:INBOX", Destination: "first", Tags: map[string]string{"order": "1"}},
		{Match: "folder:INBOX", Destination: "second", Tags: map[string]string{"order": "2"}},
	})
	result, ok := rm.Match("folder:INBOX")
	if !ok {
		t.Fatal("expected match, got no match")
	}
	if result.Destination != "first" {
		t.Errorf("first-match-wins: destination = %q, want %q", result.Destination, "first")
	}
	if result.Tags["order"] != "1" {
		t.Errorf("first-match-wins: tags[order] = %q, want %q", result.Tags["order"], "1")
	}
}

func TestRuleMatcher_NoTags(t *testing.T) {
	rm := NewRuleMatcher([]Rule{
		{Match: "folder:INBOX", Destination: "messaging"},
	})
	result, ok := rm.Match("folder:INBOX")
	if !ok {
		t.Fatal("expected match, got no match")
	}
	if result.Destination != "messaging" {
		t.Errorf("destination = %q, want %q", result.Destination, "messaging")
	}
	if len(result.Tags) != 0 {
		t.Errorf("expected empty tags, got %v", result.Tags)
	}
}

func TestRuleMatcher_EmptyRules(t *testing.T) {
	rm := NewRuleMatcher([]Rule{})
	_, ok := rm.Match("anything")
	if ok {
		t.Error("empty rules should never match")
	}
}

func TestRuleMatcher_PropagatesDataSubjectAndAudience(t *testing.T) {
	rules := []Rule{
		{
			Match:       "foo",
			Destination: "agent-a",
			DataSubject: "bee",
			Audience:    []string{"subject", "guardians"},
		},
		{
			Match:       "*",
			Destination: "agent-b",
			Audience:    []string{"household"},
		},
	}
	rm := NewRuleMatcher(rules)

	got, ok := rm.Match("foo")
	if !ok {
		t.Fatal("expected match for 'foo'")
	}
	if got.DataSubject != "bee" {
		t.Errorf("DataSubject: got %q, want %q", got.DataSubject, "bee")
	}
	if len(got.Audience) != 2 || got.Audience[0] != "subject" || got.Audience[1] != "guardians" {
		t.Errorf("Audience: got %v", got.Audience)
	}

	got, ok = rm.Match("anything-else")
	if !ok {
		t.Fatal("expected wildcard match")
	}
	if got.DataSubject != "" {
		t.Errorf("expected empty DataSubject on wildcard rule, got %q", got.DataSubject)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "household" {
		t.Errorf("Audience: got %v", got.Audience)
	}
}

func TestRuleMatcher_AudienceSliceIsCopied(t *testing.T) {
	// Defensive: mutating the returned slice must not affect the rule.
	rules := []Rule{
		{Match: "*", Destination: "a", Audience: []string{"household"}},
	}
	rm := NewRuleMatcher(rules)
	got, _ := rm.Match("x")
	got.Audience[0] = "public"

	got2, _ := rm.Match("x")
	if got2.Audience[0] != "household" {
		t.Errorf("rule audience was mutated via returned slice: got %q", got2.Audience[0])
	}
}
