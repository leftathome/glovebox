package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewMetrics_AllInstrumentsRegistered(t *testing.T) {
	m, err := NewMetrics("test-connector")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	if m.pollsTotal == nil {
		t.Error("pollsTotal not registered")
	}
	if m.itemsProduced == nil {
		t.Error("itemsProduced not registered")
	}
	if m.pollDuration == nil {
		t.Error("pollDuration not registered")
	}
	if m.errorsTotal == nil {
		t.Error("errorsTotal not registered")
	}
	if m.checkpointAge == nil {
		t.Error("checkpointAge not registered")
	}
	if m.itemsDroppedTotal == nil {
		t.Error("itemsDroppedTotal not registered")
	}
}

func TestMetrics_PollIncrementsCounter(t *testing.T) {
	m, err := NewMetrics("imap")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	// Simulate a successful poll via runPoll
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())
	if err := runPoll(context.Background(), mock, cp, m, testLogger); err != nil {
		t.Fatalf("runPoll: %v", err)
	}

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, "connector_polls_total") {
		t.Error("missing connector_polls_total in /metrics output")
	}
	if !strings.Contains(body, `connector="imap"`) {
		t.Error("missing connector=imap label")
	}
	if !strings.Contains(body, `status="success"`) {
		t.Error("missing status=success label")
	}
}

func TestMetrics_PollErrorIncrementsCounters(t *testing.T) {
	m, err := NewMetrics("rss")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	// Transient error
	mock := &mockPollConnector{pollErr: fmt.Errorf("timeout")}
	cp, _ := NewCheckpoint(t.TempDir())
	_ = runPoll(context.Background(), mock, cp, m, testLogger)

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, `status="error"`) {
		t.Error("missing status=error label after failed poll")
	}
	if !strings.Contains(body, "connector_errors_total") {
		t.Error("missing connector_errors_total")
	}
	if !strings.Contains(body, `type="transient"`) {
		t.Error("missing type=transient label")
	}
}

func TestMetrics_PermanentErrorLabel(t *testing.T) {
	m, err := NewMetrics("github")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	mock := &mockPollConnector{pollErr: PermanentError(fmt.Errorf("bad token"))}
	cp, _ := NewCheckpoint(t.TempDir())
	_ = runPoll(context.Background(), mock, cp, m, testLogger)

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, `type="permanent"`) {
		t.Error("missing type=permanent label for permanent error")
	}
}

func TestMetrics_ItemProduced(t *testing.T) {
	m, err := NewMetrics("imap")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	m.RecordItemProduced("messaging")

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, "connector_items_produced_total") {
		t.Error("missing connector_items_produced_total")
	}
	if !strings.Contains(body, `destination="messaging"`) {
		t.Error("missing destination=messaging label")
	}
}

func TestMetrics_ItemDropped(t *testing.T) {
	m, err := NewMetrics("rss")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	m.RecordItemDropped("no_route")

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, "connector_items_dropped_total") {
		t.Error("missing connector_items_dropped_total")
	}
	if !strings.Contains(body, `reason="no_route"`) {
		t.Error("missing reason=no_route label")
	}
}

func TestMetrics_PollDuration(t *testing.T) {
	m, err := NewMetrics("imap")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	m.RecordPollDuration(250 * time.Millisecond)

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, "connector_poll_duration_seconds") {
		t.Error("missing connector_poll_duration_seconds")
	}
}

func TestMetrics_CheckpointAge(t *testing.T) {
	m, err := NewMetrics("imap")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	defer m.Shutdown()

	m.SetCheckpointAge(120.5)

	body := scrapeMetrics(t, m)

	if !strings.Contains(body, "connector_checkpoint_age_seconds") {
		t.Error("missing connector_checkpoint_age_seconds")
	}
}

// scrapeMetrics fires a test HTTP request against the Prometheus handler
// and returns the body text.
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("metrics status = %d, want 200", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}
