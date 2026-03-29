package metrics

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type Metrics struct {
	ItemsProcessed    metric.Int64Counter
	ProcessingDuration metric.Float64Histogram
	SignalsTriggered   metric.Int64Counter
	StagingQueueDepth  metric.Int64Gauge
	QuarantineQueueDepth metric.Int64Gauge
	PendingItems       metric.Int64Gauge
	ScanWorkersBusy    metric.Int64Gauge
	ScanTimeouts       metric.Int64Counter
	AuditFailures      metric.Int64Counter
	FailedItems        metric.Int64Gauge

	provider *sdkmetric.MeterProvider
}

func New() (*Metrics, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	meter := provider.Meter("glovebox")

	m := &Metrics{provider: provider}

	m.ItemsProcessed, err = meter.Int64Counter("glovebox_items_processed_total",
		metric.WithDescription("Total items processed by verdict"))
	if err != nil {
		return nil, err
	}

	m.ProcessingDuration, err = meter.Float64Histogram("glovebox_processing_duration_seconds",
		metric.WithDescription("Scan processing duration"))
	if err != nil {
		return nil, err
	}

	m.SignalsTriggered, err = meter.Int64Counter("glovebox_signals_triggered_total",
		metric.WithDescription("Total signals triggered by rule"))
	if err != nil {
		return nil, err
	}

	m.StagingQueueDepth, err = meter.Int64Gauge("glovebox_staging_queue_depth",
		metric.WithDescription("Items in staging directory"))
	if err != nil {
		return nil, err
	}

	m.QuarantineQueueDepth, err = meter.Int64Gauge("glovebox_quarantine_queue_depth",
		metric.WithDescription("Items in quarantine directory"))
	if err != nil {
		return nil, err
	}

	m.PendingItems, err = meter.Int64Gauge("glovebox_pending_items",
		metric.WithDescription("Pending scan-in-progress items"))
	if err != nil {
		return nil, err
	}

	m.ScanWorkersBusy, err = meter.Int64Gauge("glovebox_scan_workers_busy",
		metric.WithDescription("Number of busy scan workers"))
	if err != nil {
		return nil, err
	}

	m.ScanTimeouts, err = meter.Int64Counter("glovebox_scan_timeouts_total",
		metric.WithDescription("Total scan timeouts"))
	if err != nil {
		return nil, err
	}

	m.AuditFailures, err = meter.Int64Counter("glovebox_audit_failures_total",
		metric.WithDescription("Total audit log write failures"))
	if err != nil {
		return nil, err
	}

	m.FailedItems, err = meter.Int64Gauge("glovebox_failed_items",
		metric.WithDescription("Items in failed directory"))
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.Handler()
}

func (m *Metrics) Shutdown() error {
	if m.provider != nil {
		return m.provider.Shutdown(context.Background())
	}
	return nil
}
