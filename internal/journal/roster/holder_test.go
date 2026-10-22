package roster_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

func TestHolderKeepsThePreviousRosterWhenANewOneFailsToParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "household.json")
	h := roster.NewHolder(path, roster.FormatOpenclawHouseholdV1)
	if h.Current() != nil {
		t.Fatal("a roster before any load")
	}
	if _, err := h.Reload(); err == nil {
		t.Fatal("a missing file loaded")
	}
	if err := os.WriteFile(path, []byte(journaltest.HouseholdV1), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := h.Reload()
	if err != nil || !changed || !h.Current().Has("e_child1") {
		t.Fatalf("first load: %v %v", changed, err)
	}
	first := h.Current().SHA256()
	if changed, err := h.Reload(); err != nil || changed {
		t.Errorf("an unchanged file reloaded: %v %v", changed, err)
	}
	if err := os.WriteFile(path, []byte(`{"people": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Reload(); err == nil {
		t.Error("a broken roster loaded")
	}
	if h.Current() == nil || h.Current().SHA256() != first {
		t.Error("the previous roster was not kept")
	}
}

func TestHolderWatchReportsChangesAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "household.json")
	if err := os.WriteFile(path, []byte(journaltest.HouseholdV1), 0o600); err != nil {
		t.Fatal(err)
	}
	h := roster.NewHolder(path, roster.FormatOpenclawHouseholdV1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type report struct {
		changed bool
		err     error
	}
	got := make(chan report, 10)
	go h.Watch(ctx, 10*time.Millisecond, func(_ *roster.Roster, changed bool, err error) {
		if changed || err != nil {
			got <- report{changed, err}
		}
	})
	if r := <-got; !r.changed || r.err != nil {
		t.Errorf("first report %+v", r)
	}
	_ = os.WriteFile(path, []byte(`broken`), 0o600)
	if r := <-got; r.err == nil {
		t.Errorf("second report %+v", r)
	}
}
