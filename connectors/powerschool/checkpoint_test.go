package powerschool

import (
	"testing"

	"github.com/leftathome/glovebox/connector"
)

func TestCheckpointKey(t *testing.T) {
	if got := CheckpointKey("grade", "k1", "00100"); got != "grade:k1:00100" {
		t.Errorf("CheckpointKey = %q", got)
	}
}

func TestChangeString(t *testing.T) {
	for c, want := range map[Change]string{ChangeNone: "unchanged", ChangeNew: "new", ChangeUpdated: "updated", Change(99): "unchanged"} {
		if got := c.String(); got != want {
			t.Errorf("Change(%d).String() = %q, want %q", int(c), got, want)
		}
	}
}

func TestCompareAndSave(t *testing.T) {
	dir := t.TempDir()
	cp, err := connector.NewCheckpoint(dir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}

	if change, prev := Compare(cp, "grade", "k1", "00100", "fp1"); change != ChangeNew || prev != "" {
		t.Errorf("fresh record = (%v, %q), want (new, \"\")", change, prev)
	}
	if err := SaveFingerprint(cp, "grade", "k1", "00100", "fp1"); err != nil {
		t.Fatalf("SaveFingerprint: %v", err)
	}
	if change, prev := Compare(cp, "grade", "k1", "00100", "fp1"); change != ChangeNone || prev != "fp1" {
		t.Errorf("same fingerprint = (%v, %q), want (unchanged, fp1)", change, prev)
	}
	if change, prev := Compare(cp, "grade", "k1", "00100", "fp2"); change != ChangeUpdated || prev != "fp1" {
		t.Errorf("new fingerprint = (%v, %q), want (updated, fp1)", change, prev)
	}

	// Scope: another kid, another surface and another id are all fresh.
	for _, other := range [][3]string{{"grade", "k2", "00100"}, {"attendance", "k1", "00100"}, {"grade", "k1", "00200"}} {
		if change, _ := Compare(cp, other[0], other[1], other[2], "fp1"); change != ChangeNew {
			t.Errorf("Compare(%v) = %v, want new", other, change)
		}
	}

	// The fingerprint survives a restart (a new Checkpoint on the same dir).
	reopened, err := connector.NewCheckpoint(dir)
	if err != nil {
		t.Fatalf("reopen checkpoint: %v", err)
	}
	if change, _ := Compare(reopened, "grade", "k1", "00100", "fp1"); change != ChangeNone {
		t.Errorf("after restart = %v, want unchanged", change)
	}
}
