//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/integrationtest"
)

// TestLive_SemanticScholar drives the semantic-scholar connector against the
// real api.semanticscholar.org and asserts it stages at least one routed
// item. PerPoll=1 keeps it to one paper.
//
// Requires SEMANTIC_SCHOLAR_API_KEY: empirically the keyless public tier
// returns HTTP 429 immediately, and the connector SWALLOWS per-query fetch
// errors (logs + continues) so a 429 surfaces as zero staged items rather
// than a returned error SkipOnRateLimit could catch -- i.e. keyless runs
// fail instead of skipping. A free Semantic Scholar API key raises the limit;
// gating on it makes the test reliable with a key and skip-clean without one.
// (Registry: semantic-scholar is therefore test-account, not none.)
func TestLive_SemanticScholar(t *testing.T) {
	integrationtest.RequireIntegration(t)
	integrationtest.RequireCreds(t, "SEMANTIC_SCHOLAR_API_KEY")

	cfg := Config{
		BaseConfig: connector.BaseConfig{
			Rules:       []connector.Rule{{Match: "query:ml", Destination: "messaging"}},
			FetchLimits: connector.FetchLimits{PerPoll: 1},
		},
		Queries: []QueryConfig{{Name: "ml", Query: "machine learning"}},
	}

	c := &SemanticScholarConnector{
		config:     cfg,
		httpClient: connector.NewHTTPClient(connector.HTTPClientOptions{}),
		apiKey:     os.Getenv("SEMANTIC_SCHOLAR_API_KEY"), // optional; empty = public tier
	}
	w, readback := integrationtest.StageToTempDir(t, "semantic-scholar")
	c.writer = w
	c.matcher = connector.NewRuleMatcher(cfg.Rules)
	c.fetchCounter = connector.NewFetchCounter(cfg.FetchLimits)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Poll(ctx, cp); err != nil {
		integrationtest.SkipOnRateLimit(t, err)
		t.Fatalf("Poll: %v", err)
	}

	items := readback()
	integrationtest.AssertStagedAtLeast(t, items, 1)
	if len(items) == 0 {
		t.FailNow()
	}
	integrationtest.AssertContentNonEmpty(t, items[0])
	integrationtest.AssertRouting(t, items[0], integrationtest.WantRouting{DestinationAgent: "messaging"})
}
