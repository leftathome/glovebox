// Telemetry for the PowerSchool connector.
//
// Same two layers as the Schoology connector: the framework-provided
// *connector.Metrics (connector_polls_total and friends), embedded by
// pointer, plus a handful of powerschool-specific OTel instruments and a
// tracer. Every Record* method is nil-safe so processors and tests that do
// not wire telemetry can pass nil.
package powerschool

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/leftathome/glovebox/connector"
)

// Telemetry is the PowerSchool connector's single observability handle.
type Telemetry struct {
	*connector.Metrics

	pollsTotal         metric.Int64Counter
	itemsTotal         metric.Int64Counter
	surfaceErrorsTotal metric.Int64Counter
	kidsSkippedTotal   metric.Int64Counter

	tracer trace.Tracer
}

// NewTelemetry constructs Telemetry. The framework *connector.Metrics is
// owned by the caller. meterName and tracerName must both be supplied.
func NewTelemetry(base *connector.Metrics, meterName, tracerName string) (*Telemetry, error) {
	if base == nil {
		return nil, errors.New("powerschool.NewTelemetry: base *connector.Metrics is required")
	}
	if meterName == "" {
		return nil, errors.New("powerschool.NewTelemetry: meterName is required")
	}
	if tracerName == "" {
		return nil, errors.New("powerschool.NewTelemetry: tracerName is required")
	}

	// The meter comes from the framework's own provider -- the one behind
	// the /metrics endpoint. otel.Meter() would resolve against the global
	// provider, which connector.Run never sets, and every instrument below
	// would be a silent no-op.
	return newTelemetry(base, base.Provider().Meter(meterName), otel.Tracer(tracerName))
}

// newTelemetry builds Telemetry on an explicit meter, so tests can read the
// recorded values back through their own reader.
func newTelemetry(base *connector.Metrics, meter metric.Meter, tracer trace.Tracer) (*Telemetry, error) {
	t := &Telemetry{
		Metrics: base,
		tracer:  tracer,
	}

	var err error
	t.pollsTotal, err = meter.Int64Counter("powerschool_polls_total",
		metric.WithDescription("Polls performed, by trigger source"))
	if err != nil {
		return nil, fmt.Errorf("construct powerschool_polls_total: %w", err)
	}
	t.itemsTotal, err = meter.Int64Counter("powerschool_items_total",
		metric.WithDescription("Items staged by surface, kid and change (new|updated)"))
	if err != nil {
		return nil, fmt.Errorf("construct powerschool_items_total: %w", err)
	}
	t.surfaceErrorsTotal, err = meter.Int64Counter("powerschool_surface_errors_total",
		metric.WithDescription("Read failures by surface, error_class and kid"))
	if err != nil {
		return nil, fmt.Errorf("construct powerschool_surface_errors_total: %w", err)
	}
	t.kidsSkippedTotal, err = meter.Int64Counter("powerschool_kids_skipped_total",
		metric.WithDescription("Kids skipped in a poll by reason: unknown_student, switch_not_confirmed, "+
			"student_number_mismatch, selection_unknown, selection_mismatch"))
	if err != nil {
		return nil, fmt.Errorf("construct powerschool_kids_skipped_total: %w", err)
	}
	return t, nil
}

// RecordItemProduced wraps the framework RecordItemProduced with
// nil-safety. Direct method promotion would panic on a nil *Telemetry.
func (t *Telemetry) RecordItemProduced(destination string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordItemProduced(destination)
}

// RecordItemDropped wraps the framework RecordItemDropped with nil-safety.
func (t *Telemetry) RecordItemDropped(reason string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordItemDropped(reason)
}

// RecordError wraps the framework RecordError with nil-safety.
func (t *Telemetry) RecordError(errType string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordError(errType)
}

// RecordPollByTrigger increments powerschool_polls_total{trigger_source}.
// triggerSource is "scheduled" or "catch_up".
func (t *Telemetry) RecordPollByTrigger(triggerSource string) {
	if t == nil {
		return
	}
	t.pollsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("trigger_source", triggerSource),
	))
}

// RecordItem increments powerschool_items_total{surface,kid,change}.
func (t *Telemetry) RecordItem(surface, kid string, change Change) {
	if t == nil {
		return
	}
	t.itemsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("surface", surface),
		attribute.String("kid", kid),
		attribute.String("change", change.String()),
	))
}

// RecordSurfaceError increments
// powerschool_surface_errors_total{surface,error_class,kid}. kid is empty
// for the account-level student listing.
func (t *Telemetry) RecordSurfaceError(surface, errorClass, kid string) {
	if t == nil {
		return
	}
	t.surfaceErrorsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("surface", surface),
		attribute.String("error_class", errorClass),
		attribute.String("kid", kid),
	))
}

// RecordKidSkipped increments powerschool_kids_skipped_total{kid,reason}.
func (t *Telemetry) RecordKidSkipped(kid, reason string) {
	if t == nil {
		return
	}
	t.kidsSkippedTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("kid", kid),
		attribute.String("reason", reason),
	))
}

// StartSpan is a thin wrapper over the connector tracer's Start. Callers
// MUST defer span.End(). A nil receiver still returns a usable span.
func (t *Telemetry) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil {
		return otel.Tracer("powerschool").Start(ctx, name, trace.WithAttributes(attrs...))
	}
	return t.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}
