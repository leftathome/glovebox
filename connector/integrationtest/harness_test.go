package integrationtest

import (
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func TestStageToTempDir_RoundTrip(t *testing.T) {
	w, readback := StageToTempDir(t, "unit")

	item, err := w.NewItem(connector.ItemOptions{
		Source:           "unit-source",
		Sender:           "tester",
		Subject:          "hello",
		Timestamp:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		DataSubject:      "k1",
		Audience:         []string{"household"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	items := readback()
	if len(items) != 1 {
		t.Fatalf("want 1 staged item, got %d", len(items))
	}
	if items[0].Meta.DestinationAgent != "messaging" {
		t.Errorf("DestinationAgent = %q", items[0].Meta.DestinationAgent)
	}
}

// stageOne commits a single valid item ("hi", routed k1/household/messaging)
// and returns it read back, for the assertion-helper happy-path tests.
func stageOne(t *testing.T) StagedItem {
	t.Helper()
	w, readback := StageToTempDir(t, "unit")
	item, err := w.NewItem(connector.ItemOptions{
		Source:           "unit-source",
		Sender:           "tester",
		Subject:          "hello",
		Timestamp:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		DataSubject:      "k1",
		Audience:         []string{"household"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}
	items := readback()
	if len(items) != 1 {
		t.Fatalf("want 1 staged item, got %d", len(items))
	}
	return items[0]
}

func TestAssertStagedAtLeast(t *testing.T) {
	items := []StagedItem{stageOne(t)}
	AssertStagedAtLeast(t, items, 1) // passes for the happy path (does not fail t)
}

func TestAssertContentNonEmpty(t *testing.T) {
	AssertContentNonEmpty(t, stageOne(t))
}

func TestAssertRouting(t *testing.T) {
	AssertRouting(t, stageOne(t), WantRouting{
		DataSubject:      "k1",
		Audience:         []string{"household"},
		DestinationAgent: "messaging",
	})
}
