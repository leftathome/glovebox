package schoology

import (
	"strings"
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

func TestStageDecision_String(t *testing.T) {
	cases := map[StageDecision]string{
		StageAccept:        "accept",
		StageSkipZero:      "skip_zero_id",
		StageSkipDuplicate: "skip_duplicate",
		StageSkipBelow:     "skip_below_checkpoint",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("%d: got %q want %q", int(d), got, want)
		}
	}
}

func TestShouldStage_Advances(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	d, err := ShouldStage(cp, "feed", "k1", 100)
	if err != nil || d != StageAccept {
		t.Errorf("first item: got d=%v err=%v, want StageAccept nil", d, err)
	}
	if err := SaveLastSeenID(cp, "feed", "k1", 100); err != nil {
		t.Fatal(err)
	}

	d, err = ShouldStage(cp, "feed", "k1", 100)
	if err != nil || d != StageSkipDuplicate {
		t.Errorf("equal id: got d=%v err=%v, want StageSkipDuplicate nil", d, err)
	}

	d, err = ShouldStage(cp, "feed", "k1", 99)
	if err != nil || d != StageSkipBelow {
		t.Errorf("below-checkpoint id: got d=%v err=%v, want StageSkipBelow nil", d, err)
	}

	d, err = ShouldStage(cp, "feed", "k1", 101)
	if err != nil || d != StageAccept {
		t.Errorf("higher id: got d=%v err=%v, want StageAccept nil", d, err)
	}
}

func TestShouldStage_ZeroIDRejected(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := ShouldStage(cp, "feed", "k1", 0)
	if err != nil {
		t.Errorf("zero ID: unexpected err %v", err)
	}
	if d != StageSkipZero {
		t.Errorf("zero ID: got %v want StageSkipZero", d)
	}
	if d.Accept() {
		t.Errorf("zero ID: Accept() must be false")
	}
}

func TestShouldStage_CorruptedCheckpointReturnsError(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Write an unparseable value directly.
	if err := cp.Save(CheckpointKey("feed", "k1"), "not-a-number"); err != nil {
		t.Fatal(err)
	}
	d, err := ShouldStage(cp, "feed", "k1", 100)
	if err == nil {
		t.Fatalf("expected error on corrupted checkpoint, got d=%v", d)
	}
	if !strings.Contains(err.Error(), "checkpoint parse error") {
		t.Errorf("error message missing expected prefix: %v", err)
	}
}

func TestLastSeenID_FreshReturnsZero(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := LastSeenID(cp, "message", "")
	if err != nil || n != 0 {
		t.Errorf("fresh checkpoint: got n=%d err=%v, want 0 nil", n, err)
	}
}

func TestParseID(t *testing.T) {
	n, err := ParseID("12345")
	if err != nil || n != 12345 {
		t.Errorf("valid: got n=%d err=%v", n, err)
	}
	if _, err := ParseID("nope"); err == nil {
		t.Errorf("invalid: expected error")
	}
}
