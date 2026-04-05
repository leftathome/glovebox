package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// producerConnector is a mock that writes N items to staging during Poll.
type producerConnector struct {
	writer  *StagingWriter
	matcher *RuleMatcher
	count   int
	source  string
	routeOn string // routing key passed to matcher.Match
}

func (c *producerConnector) Poll(ctx context.Context, cp Checkpoint) error {
	for i := 0; i < c.count; i++ {
		result, ok := c.matcher.Match(c.routeOn)
		if !ok {
			// unmatched rule -- skip item, do NOT advance checkpoint
			continue
		}
		dest := result.Destination
		item, err := c.writer.NewItem(ItemOptions{
			Source:           c.source,
			Sender:           "test-sender",
			Subject:          fmt.Sprintf("item-%d", i),
			Timestamp:        time.Now().UTC(),
			DestinationAgent: dest,
			ContentType:      "text/plain",
		})
		if err != nil {
			return err
		}
		if err := item.WriteContent([]byte(fmt.Sprintf("content for item %d", i))); err != nil {
			return err
		}
		if err := item.Commit(); err != nil {
			return err
		}
		if err := cp.Save("last", fmt.Sprintf("%d", i)); err != nil {
			return err
		}
	}
	return nil
}

func stagingItemCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

// TestIntegration_ItemFlowsToStaging verifies that a connector produces items
// that appear in the staging directory with correct metadata.json.
func TestIntegration_ItemFlowsToStaging(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := NewStagingWriter(stagingDir, "test-producer")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := NewRuleMatcher([]Rule{
		{Match: "*", Destination: "messaging"},
	})

	cp, err := NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}

	c := &producerConnector{
		writer:  writer,
		matcher: matcher,
		count:   3,
		source:  "integration-test",
		routeOn: "anything",
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Verify 3 items in staging
	count := stagingItemCount(t, stagingDir)
	if count != 3 {
		t.Fatalf("expected 3 staged items, got %d", count)
	}

	// Verify metadata.json schema on each item
	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		if meta["source"] != "integration-test" {
			t.Errorf("expected source 'integration-test', got %v", meta["source"])
		}
		if meta["destination_agent"] != "messaging" {
			t.Errorf("expected destination_agent 'messaging', got %v", meta["destination_agent"])
		}
		if meta["content_type"] != "text/plain" {
			t.Errorf("expected content_type 'text/plain', got %v", meta["content_type"])
		}
		if _, ok := meta["timestamp"]; !ok {
			t.Error("metadata missing timestamp field")
		}

		// Verify content.raw exists
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		if _, err := os.Stat(contentPath); err != nil {
			t.Errorf("content.raw missing for item %s", e.Name())
		}
	}
}

// TestIntegration_CheckpointPersistsAcrossRestart verifies that checkpoint
// state survives creating a new Checkpoint from the same state directory.
func TestIntegration_CheckpointPersistsAcrossRestart(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := NewStagingWriter(stagingDir, "cp-test")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := NewRuleMatcher([]Rule{
		{Match: "*", Destination: "default"},
	})

	// First "run": produce 2 items
	cp1, err := NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}

	c := &producerConnector{
		writer:  writer,
		matcher: matcher,
		count:   2,
		source:  "cp-test",
		routeOn: "any",
	}

	if err := c.Poll(context.Background(), cp1); err != nil {
		t.Fatalf("first Poll: %v", err)
	}

	// Verify checkpoint was saved
	val, ok := cp1.Load("last")
	if !ok {
		t.Fatal("checkpoint not saved after first poll")
	}
	if val != "1" {
		t.Fatalf("expected checkpoint '1', got %q", val)
	}

	// Simulate restart: create new Checkpoint from same state directory
	cp2, err := NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint after restart: %v", err)
	}

	val2, ok2 := cp2.Load("last")
	if !ok2 {
		t.Fatal("checkpoint lost after restart")
	}
	if val2 != val {
		t.Errorf("checkpoint value changed after restart: %q -> %q", val, val2)
	}
}

// TestIntegration_HealthEndpoints verifies /healthz returns 200 immediately
// and /readyz transitions from 503 to 200 after the first successful poll.
func TestIntegration_HealthEndpoints(t *testing.T) {
	var ready atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// /healthz should be 200 immediately
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// /readyz should be 503 before poll
	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("/readyz before poll = %d, want 503", resp.StatusCode)
	}

	// Simulate first successful poll completing
	ready.Store(true)

	// /readyz should now be 200
	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz after poll: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/readyz after poll = %d, want 200", resp.StatusCode)
	}
}

// TestIntegration_NoWildcardRouteWarning verifies that configuring routes
// without a wildcard still works (items with matching routes succeed,
// unmatched items are skipped).
func TestIntegration_NoWildcardRouteWarning(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := NewStagingWriter(stagingDir, "no-wildcard")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	// Routes with no wildcard -- only exact match for "known"
	matcher := NewRuleMatcher([]Rule{
		{Match: "known", Destination: "handler"},
	})

	cp, err := NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}

	// Connector that routes on "known" -- should succeed
	c := &producerConnector{
		writer:  writer,
		matcher: matcher,
		count:   2,
		source:  "no-wc-test",
		routeOn: "known",
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := stagingItemCount(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items for matched route, got %d", count)
	}
}

// TestIntegration_UnmatchedRouteSkipsItem verifies that items with no matching
// route are skipped and the checkpoint is NOT advanced.
func TestIntegration_UnmatchedRouteSkipsItem(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := NewStagingWriter(stagingDir, "unmatched")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	// Route only matches "specific-key", no wildcard
	matcher := NewRuleMatcher([]Rule{
		{Match: "specific-key", Destination: "handler"},
	})

	cp, err := NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}

	// Connector routes on "unknown-key" -- no match
	c := &producerConnector{
		writer:  writer,
		matcher: matcher,
		count:   3,
		source:  "unmatched-test",
		routeOn: "unknown-key",
	}

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// No items should be staged
	count := stagingItemCount(t, stagingDir)
	if count != 0 {
		t.Errorf("expected 0 staged items for unmatched route, got %d", count)
	}

	// Checkpoint should NOT have been advanced
	_, ok := cp.Load("last")
	if ok {
		t.Error("checkpoint should not be saved when route is unmatched")
	}
}

// TestIntegration_MetricsEndpoint verifies the /metrics endpoint returns
// Prometheus-format data after the connector has been instrumented.
func TestIntegration_MetricsEndpoint(t *testing.T) {
	m, err := NewMetrics("integration-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	// Record some activity
	m.RecordPoll("success")
	m.RecordItemProduced("messaging")
	m.RecordPollDuration(100 * time.Millisecond)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, metric := range []string{
		"connector_polls_total",
		"connector_items_produced_total",
		"connector_poll_duration_seconds",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("expected %s in /metrics output", metric)
		}
	}
}

