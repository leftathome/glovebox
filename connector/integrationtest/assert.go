package integrationtest

import (
	"os"
	"slices"
	"testing"
)

// AssertStagedAtLeast fails the test when fewer than n items were committed.
func AssertStagedAtLeast(t *testing.T, items []StagedItem, n int) {
	t.Helper()
	if len(items) < n {
		t.Errorf("staged %d items, want >= %d", len(items), n)
	}
}

// AssertContentNonEmpty fails when the item's content.raw is missing or empty.
func AssertContentNonEmpty(t *testing.T, item StagedItem) {
	t.Helper()
	fi, err := os.Stat(item.ContentPath)
	if err != nil || fi.Size() == 0 {
		t.Errorf("content.raw missing or empty in %s (err=%v)", item.Dir, err)
	}
}

// WantRouting is the resolved routing to assert on the committed metadata.
type WantRouting struct {
	DataSubject      string
	Audience         []string
	DestinationAgent string
}

// AssertRouting does field equality on the ALREADY-RESOLVED metadata (the
// RuleMatcher ran during Commit); it does not re-run the matcher.
func AssertRouting(t *testing.T, item StagedItem, want WantRouting) {
	t.Helper()
	if item.Meta.DestinationAgent != want.DestinationAgent {
		t.Errorf("DestinationAgent = %q, want %q", item.Meta.DestinationAgent, want.DestinationAgent)
	}
	if item.Meta.DataSubject != want.DataSubject {
		t.Errorf("DataSubject = %q, want %q", item.Meta.DataSubject, want.DataSubject)
	}
	if !slices.Equal(item.Meta.Audience, want.Audience) {
		t.Errorf("Audience = %v, want %v", item.Meta.Audience, want.Audience)
	}
}

// AssertHasSidecar asserts an enrichment sidecar FILE (e.g.
// "content.extracted.md") was produced on disk by the real Commit pipeline
// (runEnrichmentPipeline). It intentionally checks the file, not the
// metadata Enrichments[] record (simpler, and proves the artifact exists).
func AssertHasSidecar(t *testing.T, item StagedItem, name string) {
	t.Helper()
	for _, s := range item.Sidecars {
		if s == name {
			return
		}
	}
	t.Errorf("sidecar %q not found in %s (have %v)", name, item.Dir, item.Sidecars)
}
