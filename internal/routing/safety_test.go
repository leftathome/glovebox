package routing

import (
	"strings"
	"testing"
)

var testAllowlist = []string{"messaging", "media", "calendar", "itinerary"}

func TestValidateDestination_Valid(t *testing.T) {
	path, err := ValidateDestination("messaging", "/data/agents", testAllowlist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(path, "messaging") {
		t.Errorf("path should contain 'messaging': %q", path)
	}
}

func TestValidateDestination_NotInAllowlist(t *testing.T) {
	_, err := ValidateDestination("hacking", "/data/agents", testAllowlist)
	if err == nil {
		t.Fatal("expected error for agent not in allowlist")
	}
	if !strings.Contains(err.Error(), "unknown_destination") {
		t.Errorf("error should mention unknown_destination: %v", err)
	}
}

func TestValidateDestination_PathTraversal(t *testing.T) {
	_, err := ValidateDestination("../../etc", "/data/agents", []string{"../../etc"})
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "path_traversal") {
		t.Errorf("error should mention path_traversal: %v", err)
	}
}

func TestValidateDestination_CaseSensitive(t *testing.T) {
	_, err := ValidateDestination("Messaging", "/data/agents", testAllowlist)
	if err == nil {
		t.Fatal("allowlist should be case-sensitive")
	}
}
