package connector

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds OTel instruments for connector observability.
type Metrics struct {
	connectorName string

	pollsTotal        metric.Int64Counter
	itemsProduced     metric.Int64Counter
	pollDuration      metric.Float64Histogram
	errorsTotal       metric.Int64Counter
	checkpointAge     metric.Float64Gauge
	itemsDroppedTotal metric.Int64Counter

	provider *sdkmetric.MeterProvider
}

// NewMetrics registers all connector OTel instruments with a Prometheus
// exporter. connectorName is recorded as the "connector" label on every
// metric, not as a namespace prefix.
func NewMetrics(connectorName string) (*Metrics, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	meter := provider.Meter("connector")

	m := &Metrics{
		connectorName: connectorName,
		provider:      provider,
	}

	m.pollsTotal, err = meter.Int64Counter("connector_polls_total",
		metric.WithDescription("Total number of connector polls"))
	if err != nil {
		return nil, err
	}

	m.itemsProduced, err = meter.Int64Counter("connector_items_produced_total",
		metric.WithDescription("Total items produced by a connector"))
	if err != nil {
		return nil, err
	}

	m.pollDuration, err = meter.Float64Histogram("connector_poll_duration_seconds",
		metric.WithDescription("Duration of connector poll in seconds"))
	if err != nil {
		return nil, err
	}

	m.errorsTotal, err = meter.Int64Counter("connector_errors_total",
		metric.WithDescription("Total connector errors by type"))
	if err != nil {
		return nil, err
	}

	m.checkpointAge, err = meter.Float64Gauge("connector_checkpoint_age_seconds",
		metric.WithDescription("Age of the last checkpoint in seconds"))
	if err != nil {
		return nil, err
	}

	m.itemsDroppedTotal, err = meter.Int64Counter("connector_items_dropped_total",
		metric.WithDescription("Total items dropped by a connector"))
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Handler returns an http.Handler that serves the Prometheus /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.Handler()
}

// Shutdown flushes and shuts down the meter provider.
func (m *Metrics) Shutdown() error {
	if m.provider != nil {
		return m.provider.Shutdown(context.Background())
	}
	return nil
}

// RecordPoll increments the connector_polls_total counter.
// status should be "success" or "error".
func (m *Metrics) RecordPoll(status string) {
	m.pollsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("connector", m.connectorName),
		attribute.String("status", status),
	))
}

// RecordPollDuration records a poll duration observation.
func (m *Metrics) RecordPollDuration(d time.Duration) {
	m.pollDuration.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String("connector", m.connectorName),
	))
}

// RecordItemProduced increments the connector_items_produced_total counter.
func (m *Metrics) RecordItemProduced(destination string) {
	m.itemsProduced.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("connector", m.connectorName),
		attribute.String("destination", destination),
	))
}

// RecordItemDropped increments the connector_items_dropped_total counter.
func (m *Metrics) RecordItemDropped(reason string) {
	m.itemsDroppedTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("connector", m.connectorName),
		attribute.String("reason", reason),
	))
}

// RecordError increments the connector_errors_total counter.
// errType should be "transient" or "permanent".
func (m *Metrics) RecordError(errType string) {
	m.errorsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("connector", m.connectorName),
		attribute.String("type", errType),
	))
}

// SetCheckpointAge sets the connector_checkpoint_age_seconds gauge.
func (m *Metrics) SetCheckpointAge(seconds float64) {
	m.checkpointAge.Record(context.Background(), seconds, metric.WithAttributes(
		attribute.String("connector", m.connectorName),
	))
}
