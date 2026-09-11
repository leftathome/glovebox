package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
)

type stagedItem struct {
	Meta    staging.ItemMetadata
	Content []byte
}

// newTestConnector wires a connector against a temporary staging directory.
// SetTier stands in for what NewFramework does at boot.
func newTestConnector(t *testing.T, cfg Config, rules []connector.Rule) (*UniFiConnector, string) {
	t.Helper()

	stagingDir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := connector.NewStagingWriter(stagingDir, "unifi")
	if err != nil {
		t.Fatal(err)
	}
	w.SetTier(connector.TierFeed)

	cfg.applyDefaults()
	return &UniFiConnector{
		config:        cfg,
		writer:        w,
		matcher:       connector.NewRuleMatcher(rules),
		fetchCounter:  connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:    http.DefaultClient,
		apiKey:        "test-key",
		webhookSecret: []byte("test-secret"),
	}, stagingDir
}

func readStaged(t *testing.T, stagingDir string) []stagedItem {
	t.Helper()

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatal(err)
	}

	var items []stagedItem
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "content.raw"))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, stagedItem{Meta: meta, Content: content})
	}
	return items
}

func memCheckpoint(t *testing.T) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func allowAll() []connector.Rule {
	return []connector.Rule{{Match: "*", Destination: "messaging"}}
}

// The whole point of the bead: a UniFi item must carry tier "feed" so
// openclaw's triage diverts it to caro instead of writing it into the
// audiences tree that feeds every person-agent's ambient recall.
//
// Delivered through the subscription, which is the only transport that exists
// (glovebox-pn2j).
func TestSubscribe_StagedItemDeclaresFeedTier(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())
	w := newProtectWatcher(c)

	if err := w.handleFrame([]byte(frameEnd), nil, quietLogger()); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}
	if items[0].Meta.Tier != string(connector.TierFeed) {
		t.Errorf("tier = %q, want %q -- without this the item lands in per-agent recall",
			items[0].Meta.Tier, connector.TierFeed)
	}
	if items[0].Meta.ContentType != "application/vnd.unifi.event+json" {
		t.Errorf("content_type = %q, want the narrow unifi event type", items[0].Meta.ContentType)
	}
	if items[0].Meta.Tags["unifi.surface"] != "protect" {
		t.Errorf("surface tag = %q, want protect", items[0].Meta.Tags["unifi.surface"])
	}
}

// Routing is the operator's decision; an unrouted item has nowhere to go.
func TestSubscribe_NoMatchingRuleStagesNothing(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, []connector.Rule{{Match: "event:something-else", Destination: "messaging"}})
	w := newProtectWatcher(c)

	if err := w.handleFrame([]byte(frameEnd), nil, quietLogger()); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if got := len(readStaged(t, stagingDir)); got != 0 {
		t.Errorf("expected nothing staged without a matching rule, got %d", got)
	}
}

// Poll is a liveness check now. Against a controller that answers meta/info it
// succeeds, which is what turns /readyz green.
func TestPoll_ProtectReachabilityCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/protect/integration/v1/meta/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"applicationVersion":"7.3.47"}`))
	}))
	defer srv.Close()

	c, _ := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	if err := c.Poll(context.Background(), memCheckpoint(t)); err != nil {
		t.Fatalf("Poll against a reachable controller: %v", err)
	}
}

// A controller that does not answer must not report ready.
func TestPoll_UnreachableControllerFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	if err := c.Poll(context.Background(), memCheckpoint(t)); err == nil {
		t.Fatal("expected an error from an unreachable controller")
	}
}

// A rejected API key will be rejected on every retry.
func TestPoll_RejectedKeyIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	err := c.Poll(context.Background(), memCheckpoint(t))
	if err == nil {
		t.Fatal("expected an error for a rejected key")
	}
	if !connector.IsPermanent(err) {
		t.Errorf("a rejected API key should be a permanent error, got %v", err)
	}
}

// Enabling the network surface is a configuration mistake on this firmware, and
// it fails loudly rather than leaving an operator staring at an empty staging
// directory.
func TestPoll_NetworkSurfaceIsAnHonestPermanentError(t *testing.T) {
	c, _ := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Network:       NetworkConfig{Enabled: true},
	}, allowAll())

	err := c.Poll(context.Background(), memCheckpoint(t))
	if err == nil {
		t.Fatal("expected enabling the network surface to fail")
	}
	if !connector.IsPermanent(err) {
		t.Errorf("should be permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "network.enabled=false") {
		t.Errorf("the error should tell the operator what to do, got %v", err)
	}
}
