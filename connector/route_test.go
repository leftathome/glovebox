package connector

import "testing"

func TestRouter_ExactMatch(t *testing.T) {
	r := NewRouter([]Route{
		{Match: "folder:INBOX", Destination: "messaging"},
		{Match: "folder:Calendar", Destination: "calendar"},
	})
	dest, ok := r.Match("folder:INBOX")
	if !ok || dest != "messaging" {
		t.Errorf("Match = (%q, %v), want (messaging, true)", dest, ok)
	}
}

func TestRouter_WildcardMatch(t *testing.T) {
	r := NewRouter([]Route{
		{Match: "folder:INBOX", Destination: "messaging"},
		{Match: "*", Destination: "default-agent"},
	})
	dest, ok := r.Match("folder:Unknown")
	if !ok || dest != "default-agent" {
		t.Errorf("wildcard: Match = (%q, %v), want (default-agent, true)", dest, ok)
	}
}

func TestRouter_FirstMatchWins(t *testing.T) {
	r := NewRouter([]Route{
		{Match: "folder:INBOX", Destination: "first"},
		{Match: "folder:INBOX", Destination: "second"},
	})
	dest, _ := r.Match("folder:INBOX")
	if dest != "first" {
		t.Errorf("first-match-wins: got %q, want first", dest)
	}
}

func TestRouter_NoMatch(t *testing.T) {
	r := NewRouter([]Route{
		{Match: "folder:INBOX", Destination: "messaging"},
	})
	_, ok := r.Match("folder:Spam")
	if ok {
		t.Error("should return false when no route matches")
	}
}

func TestRouter_EmptyRoutes(t *testing.T) {
	r := NewRouter([]Route{})
	_, ok := r.Match("anything")
	if ok {
		t.Error("empty routes should never match")
	}
}

func TestRouter_WildcardOnly(t *testing.T) {
	r := NewRouter([]Route{
		{Match: "*", Destination: "everything"},
	})
	dest, ok := r.Match("literally:anything")
	if !ok || dest != "everything" {
		t.Errorf("wildcard-only: Match = (%q, %v), want (everything, true)", dest, ok)
	}
}
