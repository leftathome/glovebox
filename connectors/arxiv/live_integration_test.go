//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/integrationtest"
)

// TestLive_Arxiv drives the arxiv connector against the real export.arxiv.org
// API and asserts it stages at least one routed item. No credentials (public
// source); skipped unless GLOVEBOX_INTEGRATION=1. MaxResults/PerPoll=1 keep it
// to a single result.
func TestLive_Arxiv(t *testing.T) {
	integrationtest.RequireIntegration(t)

	cfg := Config{
		BaseConfig: connector.BaseConfig{
			Rules:       []connector.Rule{{Match: "query:ml", Destination: "messaging"}},
			FetchLimits: connector.FetchLimits{PerPoll: 1},
		},
		Queries: []QueryConfig{{Name: "ml", Query: "cat:cs.LG", MaxResults: 1}},
	}

	c := &ArxivConnector{
		config:     cfg,
		httpClient: connector.NewHTTPClient(connector.HTTPClientOptions{}),
	}
	w, readback := integrationtest.StageToTempDir(t, "arxiv")
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
