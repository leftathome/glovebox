//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
	"github.com/leftathome/glovebox/connector/integrationtest"
)

// TestLive_RSS drives the rss connector against a real public Atom feed and
// asserts it stages at least one routed item. No credentials needed (public
// source); skipped unless GLOVEBOX_INTEGRATION=1. Network required.
//
// Note: rss Poll swallows per-feed fetch/parse errors (logs + continues), so
// an unreachable feed surfaces as zero staged items (AssertStagedAtLeast
// failure) rather than a returned error; a stable public feed is used to
// minimize that. The nightly stage re-runs on transient failure.
func TestLive_RSS(t *testing.T) {
	integrationtest.RequireIntegration(t)

	cfg := Config{
		BaseConfig: connector.BaseConfig{
			Rules: []connector.Rule{{Match: "feed:goblog", Destination: "messaging"}},
		},
		Feeds: []FeedConfig{{Name: "goblog", URL: "https://go.dev/blog/feed.atom"}},
	}

	httpClient := connector.NewHTTPClient(connector.HTTPClientOptions{})
	c := &RSSConnector{
		config:        cfg,
		linkPolicy:    content.NewLinkPolicy(cfg.LinkPolicy),
		httpClient:    httpClient,
		robotsChecker: connector.NewRobotsChecker(httpClient),
	}
	w, readback := integrationtest.StageToTempDir(t, "rss")
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
