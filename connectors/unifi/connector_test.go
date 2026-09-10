package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
func TestPoll_StagedItemDeclaresFeedTier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"_id":"e1","key":"EVT_LU_Connected","hostname":"laptop"}]}`))
	}))
	defer srv.Close()

	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Network:       NetworkConfig{Enabled: true},
	}, allowAll())

	if err := c.Poll(context.Background(), memCheckpoint(t)); err != nil {
		t.Fatalf("Poll: %v", err)
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
}

// An SSID broadcast by a rogue AP is settable by anyone in radio range. It has
// to reach content.raw, because content.raw is what the scanner reads.
func TestPoll_RogueAPSSIDIsStagedForScanning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ev := map[string]any{"_id": "e1", "key": "EVT_AP_RogueAp", "essid": injection}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{ev}})
	}))
	defer srv.Close()

	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Network:       NetworkConfig{Enabled: true},
	}, allowAll())

	if err := c.Poll(context.Background(), memCheckpoint(t)); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}
	if !strings.Contains(string(items[0].Content), injection) {
		t.Errorf("rogue-AP SSID text never reached content.raw, so the scanner cannot see it.\ncontent = %s", items[0].Content)
	}
	if got := items[0].Meta.Tags["unifi.untrusted_fields"]; !strings.Contains(got, "essid") {
		t.Errorf("untrusted field inventory should name essid, got %q", got)
	}
	// The subject is what a reviewer reads first; it must not repeat the
	// attacker's sentence back as though glovebox wrote it.
	if strings.Contains(items[0].Meta.Subject, injection) {
		t.Errorf("subject carried attacker text: %q", items[0].Meta.Subject)
	}
}

// Protect returns a bare array rather than a {"data": ...} wrapper.
func TestPoll_ProtectBareArrayIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"p1","type":"smartDetectZone","licensePlate":"AB-123"}]`))
	}))
	defer srv.Close()

	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	if err := c.Poll(context.Background(), memCheckpoint(t)); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}
	if items[0].Meta.Tags["unifi.surface"] != "protect" {
		t.Errorf("surface tag = %q, want protect", items[0].Meta.Tags["unifi.surface"])
	}
}

// A second poll over the same page must not restage what the checkpoint
// already covers, or every poll interval republishes the whole page.
func TestPoll_CheckpointPreventsRestaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"_id":"e1","key":"EVT_LU_Connected"},{"_id":"e2","key":"EVT_LU_Disconnected"}]}`))
	}))
	defer srv.Close()

	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Network:       NetworkConfig{Enabled: true},
	}, allowAll())

	cp := memCheckpoint(t)
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second Poll: %v", err)
	}

	if got := len(readStaged(t, stagingDir)); got != 2 {
		t.Errorf("expected 2 items after two polls over the same page, got %d", got)
	}
}

// With no rule matching, nothing is staged. Routing is the operator's
// decision and an unrouted item has nowhere to go.
func TestPoll_NoMatchingRuleStagesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"_id":"e1","key":"EVT_LU_Connected"}]}`))
	}))
	defer srv.Close()

	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Network:       NetworkConfig{Enabled: true},
	}, []connector.Rule{{Match: "event:something-else", Destination: "messaging"}})

	if err := c.Poll(context.Background(), memCheckpoint(t)); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := len(readStaged(t, stagingDir)); got != 0 {
		t.Errorf("expected nothing staged without a matching rule, got %d", got)
	}
}

// A rejected API key will be rejected on every retry, so it is permanent.
func TestPoll_RejectedKeyIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := newTestConnector(t, Config{
		ControllerURL: srv.URL,
		Network:       NetworkConfig{Enabled: true},
	}, allowAll())

	// Poll tolerates a single surface failing, so call the surface directly.
	err := c.pollSurface(context.Background(), c.networkSurface(), memCheckpoint(t), slogDiscard())
	if err == nil {
		t.Fatal("expected an error for a rejected key")
	}
	if !connector.IsPermanent(err) {
		t.Errorf("a rejected API key should be a permanent error, got %v", err)
	}
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
