package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func TestNew_AllMetricsRegistered(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Shutdown()

	if m.ItemsProcessed == nil {
		t.Error("ItemsProcessed not registered")
	}
	if m.ProcessingDuration == nil {
		t.Error("ProcessingDuration not registered")
	}
	if m.SignalsTriggered == nil {
		t.Error("SignalsTriggered not registered")
	}
	if m.StagingQueueDepth == nil {
		t.Error("StagingQueueDepth not registered")
	}
	if m.QuarantineQueueDepth == nil {
		t.Error("QuarantineQueueDepth not registered")
	}
	if m.PendingItems == nil {
		t.Error("PendingItems not registered")
	}
	if m.ScanWorkersBusy == nil {
		t.Error("ScanWorkersBusy not registered")
	}
	if m.ScanTimeouts == nil {
		t.Error("ScanTimeouts not registered")
	}
	if m.AuditFailures == nil {
		t.Error("AuditFailures not registered")
	}
	if m.FailedItems == nil {
		t.Error("FailedItems not registered")
	}
}

func TestMetrics_Endpoint(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Shutdown()

	ctx := context.Background()

	attrs := metric.WithAttributes(
		attribute.String("verdict", "pass"),
		attribute.String("destination", "messaging"),
		attribute.String("source", "email"),
	)
	m.ItemsProcessed.Add(ctx, 1, attrs)
	m.ScanTimeouts.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "email")))

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "glovebox_items_processed_total") {
		t.Error("missing glovebox_items_processed_total")
	}
	if !strings.Contains(text, "glovebox_scan_timeouts_total") {
		t.Error("missing glovebox_scan_timeouts_total")
	}
}

func TestMetrics_Labels(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Shutdown()

	ctx := context.Background()

	m.ItemsProcessed.Add(ctx, 1, metric.WithAttributes(
		attribute.String("verdict", "quarantine"),
		attribute.String("destination", "calendar"),
		attribute.String("source", "webhook"),
	))

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, `verdict="quarantine"`) {
		t.Error("missing verdict=quarantine label")
	}
	if !strings.Contains(text, `destination="calendar"`) {
		t.Error("missing destination=calendar label")
	}
}
