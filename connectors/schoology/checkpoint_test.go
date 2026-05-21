package schoology

import (
	"testing"

	"github.com/leftathome/glovebox/connector"
)

func TestCheckpointKey(t *testing.T) {
	if got := CheckpointKey("assignment", "k1"); got != "assignment:k1:last_id" {
		t.Errorf("per-kid key: got %q", got)
	}
	if got := CheckpointKey("message", ""); got != "message:last_id" {
		t.Errorf("parent-level key: got %q", got)
	}
}

func TestShouldStage_Advances(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !ShouldStage(cp, "feed", "k1", 100) {
		t.Errorf("first item should be stageable")
	}
	if err := SaveLastSeenID(cp, "feed", "k1", 100); err != nil {
		t.Fatal(err)
	}
	if ShouldStage(cp, "feed", "k1", 100) {
		t.Errorf("equal id should be skipped")
	}
	if ShouldStage(cp, "feed", "k1", 99) {
		t.Errorf("below-checkpoint id should be skipped")
	}
	if !ShouldStage(cp, "feed", "k1", 101) {
		t.Errorf("higher id should be stageable")
	}
}

func TestShouldStage_ZeroIDRejected(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ShouldStage(cp, "feed", "k1", 0) {
		t.Errorf("zero ID should be rejected")
	}
}
