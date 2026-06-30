//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/integrationtest"
)

// TestLive_HackerNews drives the hackernews connector against the real HN
// Firebase API and asserts it stages at least one routed item. No
// credentials (public source); skipped unless GLOVEBOX_INTEGRATION=1.
// FetchLimits.PerPoll=1 keeps it to a single story fetch.
func TestLive_HackerNews(t *testing.T) {
	integrationtest.RequireIntegration(t)

	cfg := Config{
		BaseConfig: connector.BaseConfig{
			Rules:       []connector.Rule{{Match: "feed:top", Destination: "messaging"}},
			FetchLimits: connector.FetchLimits{PerPoll: 1},
		},
		Feeds:       []string{"top"},
		MaxComments: 10,
	}

	c := &HNConnector{
		config:     cfg,
		httpClient: connector.NewHTTPClient(connector.HTTPClientOptions{}),
	}
	w, readback := integrationtest.StageToTempDir(t, "hackernews")
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
