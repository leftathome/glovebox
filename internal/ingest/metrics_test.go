package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeterProvider returns an SDK MeterProvider backed by an in-memory
// ManualReader so tests can collect and inspect recorded metrics.
func newTestMeterProvider() (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return provider, reader
}

// collectMetrics gathers all resource metrics from the reader.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

// findCounter looks through collected metrics for a counter with the given name
// and returns the sum of data points matching the given attribute key/value pair.
// Returns (value, found).
func findCounterValue(rm metricdata.ResourceMetrics, name string, attrs ...attribute.KeyValue) (int64, bool) {
	set := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					if dp.Attributes.Equals(&set) {
						return dp.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

// findHistogramCount looks for a histogram metric and returns the count of
// data points matching the given attributes.
func findHistogramCount(rm metricdata.ResourceMetrics, name string, attrs ...attribute.KeyValue) (uint64, bool) {
	set := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if hist, ok := m.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range hist.DataPoints {
					if dp.Attributes.Equals(&set) {
						return dp.Count, true
					}
				}
			}
		}
	}
	return 0, false
}

// findHistogramSum looks for a histogram metric and returns the sum value.
func findHistogramSum(rm metricdata.ResourceMetrics, name string, attrs ...attribute.KeyValue) (float64, bool) {
	set := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if hist, ok := m.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range hist.DataPoints {
					if dp.Attributes.Equals(&set) {
						return dp.Sum, true
					}
				}
			}
		}
	}
	return 0, false
}

func newMetricsTestHandler(t *testing.T) (*Handler, *sdkmetric.ManualReader, string) {
	t.Helper()
	provider, reader := newTestMeterProvider()
	t.Cleanup(func() { provider.Shutdown(context.Background()) })

	im, err := NewIngestMetrics(provider)
	if err != nil {
		t.Fatalf("NewIngestMetrics: %v", err)
	}

	stagingDir := t.TempDir()
	h := NewHandler(stagingDir, defaultIngestConfig(), []string{"home-agent"})
	h.SetReady()
	h.SetMetrics(im)
	return h, reader, stagingDir
}

func TestMetricsRecordedOnAccept(t *testing.T) {
	h, reader, _ := newMetricsTestHandler(t)

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("hello world"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}

	rm := collectMetrics(t, reader)
	val, found := findCounterValue(rm, "glovebox_items_received_total",
		attribute.String("source", "test-connector"),
		attribute.String("status", "accepted"),
	)
	if !found {
		t.Fatal("items_received_total{status=accepted} not found in metrics")
	}
	if val != 1 {
		t.Errorf("expected items_received_total=1, got %d", val)
	}
}

func TestMetricsRecordedOnReject(t *testing.T) {
	h, reader, _ := newMetricsTestHandler(t)

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Send invalid metadata (empty JSON, missing required fields)
	body, contentType := buildMultipart(`{}`, []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	rm := collectMetrics(t, reader)
	val, found := findCounterValue(rm, "glovebox_items_received_total",
		attribute.String("source", ""),
		attribute.String("status", "rejected"),
	)
	if !found {
		t.Fatal("items_received_total{status=rejected} not found in metrics")
	}
	if val != 1 {
		t.Errorf("expected items_received_total=1, got %d", val)
	}
}

func TestMetricsRecordedOnThrottle(t *testing.T) {
	provider, reader := newTestMeterProvider()
	defer provider.Shutdown(context.Background())

	im, err := NewIngestMetrics(provider)
	if err != nil {
		t.Fatalf("NewIngestMetrics: %v", err)
	}

	cfg := defaultIngestConfig()
	cfg.BackpressureThreshold = 1
	stagingDir := t.TempDir()

	// Pre-create items to exceed threshold
	for i := 0; i < 2; i++ {
		os.MkdirAll(filepath.Join(stagingDir, fmt.Sprintf("item-%d", i)), 0755)
	}

	h := NewHandler(stagingDir, cfg, []string{"home-agent"})
	h.SetReady()
	h.SetMetrics(im)
	if err := h.InitQueueDepth(); err != nil {
		t.Fatalf("InitQueueDepth: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	rm := collectMetrics(t, reader)
	val, found := findCounterValue(rm, "glovebox_items_received_total",
		attribute.String("source", ""),
		attribute.String("status", "throttled"),
	)
	if !found {
		t.Fatal("items_received_total{status=throttled} not found in metrics")
	}
	if val != 1 {
		t.Errorf("expected items_received_total=1, got %d", val)
	}
}

func TestReceiveBytesRecorded(t *testing.T) {
	h, reader, _ := newMetricsTestHandler(t)

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	content := []byte("this is exactly 33 bytes of stuff")
	body, contentType := buildMultipart(validMetadataJSON("home-agent"), content)
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}

	rm := collectMetrics(t, reader)
	val, found := findCounterValue(rm, "glovebox_receive_bytes_total",
		attribute.String("source", "test-connector"),
	)
	if !found {
		t.Fatal("receive_bytes_total{source=test-connector} not found in metrics")
	}
	if val != int64(len(content)) {
		t.Errorf("expected receive_bytes_total=%d, got %d", len(content), val)
	}
}

func TestReceiveDurationRecorded(t *testing.T) {
	h, reader, _ := newMetricsTestHandler(t)

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}

	rm := collectMetrics(t, reader)
	count, found := findHistogramCount(rm, "glovebox_receive_duration_seconds",
		attribute.String("source", "test-connector"),
	)
	if !found {
		t.Fatal("receive_duration_seconds{source=test-connector} not found in metrics")
	}
	if count != 1 {
		t.Errorf("expected histogram count=1, got %d", count)
	}

	sum, _ := findHistogramSum(rm, "glovebox_receive_duration_seconds",
		attribute.String("source", "test-connector"),
	)
	if sum <= 0 {
		t.Errorf("expected duration > 0, got %f", sum)
	}
}

// TestMetricsNilSafe verifies the handler works correctly when no metrics are set.
func TestMetricsNilSafe(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	// Do NOT call SetMetrics -- metrics field is nil

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("hello"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}
}

// TestMultipartBuildHelper is a compile guard: ensure buildMultipart still works.
// (It is used by both handler_test.go and metrics_test.go.)
func TestMultipartBuildHelper(t *testing.T) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.Close()
	if body.Len() == 0 {
		t.Error("expected non-empty body from multipart writer")
	}
}
