package roster_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

func TestHolderConcurrentReloadNeverStoresAnOlderRoster(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "household.json")
	older := journaltest.HouseholdV1
	newer := `{"people": {"e_parent1": {"agent": "agent-parent1"}}, "household_members": ["e_parent1"]}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	h := roster.NewHolder(path, roster.FormatOpenclawHouseholdV1)
	if _, err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = h.Reload() }()
		go func() { defer wg.Done(); _ = h.Current().Has("e_child1") }()
	}
	wg.Wait()
	if h.Current().Has("e_child1") {
		t.Error("the removed member is still in the roster")
	}
}

func TestHolderCountsFailuresAndWatchDedupesErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "household.json")
	if err := os.WriteFile(path, []byte(`broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := roster.NewHolder(path, roster.FormatOpenclawHouseholdV1)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	reports := 0
	h.Watch(ctx, 10*time.Millisecond, func(_ *roster.Roster, _ bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			reports++
		}
	})
	if reports != 1 {
		t.Errorf("%d error reports, want 1", reports)
	}
	if h.Failures() < 3 {
		t.Errorf("failures %d, want every failure counted", h.Failures())
	}
	var got error
	h.Watch(context.Background(), 0, func(_ *roster.Roster, _ bool, err error) { got = err })
	if got == nil {
		t.Error("a zero interval was accepted")
	}
}

func TestHolderFollowsASymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, doc string) {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "household.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("v1", journaltest.HouseholdV1)
	mk("v2", `{"people": {"e_parent1": {"agent": "agent-parent1"}}, "household_members": ["e_parent1"]}`)
	data := filepath.Join(dir, "..data")
	if err := os.Symlink("v1", data); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/household.json", filepath.Join(dir, "household.json")); err != nil {
		t.Fatal(err)
	}
	h := roster.NewHolder(filepath.Join(dir, "household.json"), roster.FormatOpenclawHouseholdV1)
	if _, err := h.Reload(); err != nil || !h.Current().Has("e_child1") {
		t.Fatalf("v1: %v", err)
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink("v2", tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, data); err != nil {
		t.Fatal(err)
	}
	changed, err := h.Reload()
	if err != nil || !changed || h.Current().Has("e_child1") {
		t.Fatalf("after swap: %v %v", changed, err)
	}
}

// Failing is set by every Reload, so it stays true across repeated
// failures with the same error (which Watch reports once) and turns false
// on the first good or unchanged file.
func TestHolderFailingFollowsEveryReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "household.json")
	h := roster.NewHolder(path, roster.FormatOpenclawHouseholdV1)
	if h.Failing() {
		t.Error("failing before any reload")
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"people": {"e_a": {"agent": "a"}}, "household_members": ["e_a"]}`
	write(good)
	if _, err := h.Reload(); err != nil || h.Failing() {
		t.Fatalf("good roster: err %v, failing %v", err, h.Failing())
	}
	write(`broken`)
	for i := 0; i < 5; i++ {
		if _, err := h.Reload(); err == nil || !h.Failing() {
			t.Fatalf("reload %d of a broken roster: err %v, failing %v", i, err, h.Failing())
		}
	}
	if !h.Current().Has("e_a") {
		t.Error("the previous roster was not kept")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Reload(); err == nil || !h.Failing() {
		t.Errorf("missing file: err %v, failing %v", err, h.Failing())
	}
	write(good)
	if _, err := h.Reload(); err != nil || h.Failing() {
		t.Errorf("unchanged good roster again: err %v, failing %v", err, h.Failing())
	}
}
