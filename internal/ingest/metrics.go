package ingest

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// IngestMetrics holds OTel instruments for the ingest handler.
type IngestMetrics struct {
	itemsReceived   metric.Int64Counter     // glovebox_items_received_total (source, status)
	receiveDuration metric.Float64Histogram // glovebox_receive_duration_seconds (source)
	receiveBytes    metric.Int64Counter     // glovebox_receive_bytes_total (source)
	stagingDepth    metric.Int64Gauge       // glovebox_staging_queue_depth
}

// NewIngestMetrics creates ingest metrics using the given MeterProvider.
// If provider is nil, it creates a no-op set of metrics that will not
// record anything.
func NewIngestMetrics(provider *sdkmetric.MeterProvider) (*IngestMetrics, error) {
	var meterOpt []metric.MeterOption
	meter := provider.Meter("glovebox.ingest", meterOpt...)

	m := &IngestMetrics{}
	var err error

	m.itemsReceived, err = meter.Int64Counter("glovebox_items_received_total",
		metric.WithDescription("Total items received by the ingest handler"))
	if err != nil {
		return nil, fmt.Errorf("create items_received counter: %w", err)
	}

	m.receiveDuration, err = meter.Float64Histogram("glovebox_receive_duration_seconds",
		metric.WithDescription("Duration of successful receive operations"))
	if err != nil {
		return nil, fmt.Errorf("create receive_duration histogram: %w", err)
	}

	m.receiveBytes, err = meter.Int64Counter("glovebox_receive_bytes_total",
		metric.WithDescription("Total bytes received via successful ingests"))
	if err != nil {
		return nil, fmt.Errorf("create receive_bytes counter: %w", err)
	}

	m.stagingDepth, err = meter.Int64Gauge("glovebox_staging_queue_depth",
		metric.WithDescription("Current depth of the ingest staging queue"))
	if err != nil {
		return nil, fmt.Errorf("create staging_depth gauge: %w", err)
	}

	return m, nil
}
