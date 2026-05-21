// Telemetry for the Schoology connector.
//
// Spec 12 §13 defines two layers of observability:
//
//   1. The framework-provided *connector.Metrics, which exposes the
//      cross-connector metrics (connector_polls_total, connector_*).
//   2. Seven schoology-specific OTel instruments plus a tracer for the
//      span structure in §13.2.
//
// Rather than juggle two handles in every processor signature, the
// connector wraps both behind a single *Telemetry value. *connector.Metrics
// is embedded by pointer, so framework methods (RecordPoll, RecordError,
// RecordItemDropped, etc.) are reachable directly on Telemetry through
// Go method promotion. The schoology-specific Record* helpers live on
// *Telemetry directly.
//
// Every Record* method is nil-safe (early-return on t == nil), mirroring
// the recordDrop helper this file replaces: tests that don't wire
// telemetry can pass nil without panicking.
package schoology

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

// Telemetry is the Schoology connector's single observability handle.
// It embeds *connector.Metrics so framework counters (RecordPoll,
// RecordItemDropped, etc.) are available directly on Telemetry via
// method promotion; processors don't juggle two handles.
//
// On top of the framework metrics, Telemetry adds the seven
// schoology-specific OTel instruments named in spec 12 §13.1 and the
// tracer from §13.2.
type Telemetry struct {
	*connector.Metrics

	pollsTotal              metric.Int64Counter
	parseFailuresTotal      metric.Int64Counter
	parseFailureReceipts    metric.Int64Counter
	attachmentsBytesTotal   metric.Int64Counter
	attachmentsSkippedTotal metric.Int64Counter
	triggerRequestsTotal    metric.Int64Counter
	viewChildSwitchesTotal  metric.Int64Counter

	tracer trace.Tracer
}

// NewTelemetry constructs Telemetry. The framework *connector.Metrics
// is owned by the caller (typically the connector struct in Task 13);
// callers shut down the framework Metrics, not the Telemetry.
//
// meterName and tracerName scope the OTel instruments and tracer;
// "schoology" is the conventional value for both in production. Both
// must be supplied -- there is no default -- so this constructor does
// not paper over wiring mistakes that would otherwise emit metrics
// under an empty meter name.
func NewTelemetry(base *connector.Metrics, meterName, tracerName string) (*Telemetry, error) {
	if base == nil {
		return nil, errors.New("schoology.NewTelemetry: base *connector.Metrics is required")
	}
	if meterName == "" {
		return nil, errors.New("schoology.NewTelemetry: meterName is required")
	}
	if tracerName == "" {
		return nil, errors.New("schoology.NewTelemetry: tracerName is required")
	}

	meter := otel.Meter(meterName)
	t := &Telemetry{
		Metrics: base,
		tracer:  otel.Tracer(tracerName),
	}

	var err error
	t.pollsTotal, err = meter.Int64Counter("schoology_polls_total",
		metric.WithDescription("Polls performed, by trigger source"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_polls_total: %w", err)
	}
	t.parseFailuresTotal, err = meter.Int64Counter("schoology_parse_failures_total",
		metric.WithDescription("Parse failures by parser, error_class, target_kid, content_type"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_parse_failures_total: %w", err)
	}
	t.parseFailureReceipts, err = meter.Int64Counter("schoology_parse_failure_receipts_total",
		metric.WithDescription("Parse-failure receipts emitted by parser, error_class"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_parse_failure_receipts_total: %w", err)
	}
	t.attachmentsBytesTotal, err = meter.Int64Counter("schoology_attachments_downloaded_bytes_total",
		metric.WithDescription("Attachment bytes downloaded by kid and content_type"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_attachments_downloaded_bytes_total: %w", err)
	}
	t.attachmentsSkippedTotal, err = meter.Int64Counter("schoology_attachments_skipped_total",
		metric.WithDescription("Attachments skipped by reason"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_attachments_skipped_total: %w", err)
	}
	t.triggerRequestsTotal, err = meter.Int64Counter("schoology_trigger_requests_total",
		metric.WithDescription("Trigger HTTP requests by outcome"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_trigger_requests_total: %w", err)
	}
	t.viewChildSwitchesTotal, err = meter.Int64Counter("schoology_view_child_switches_total",
		metric.WithDescription("View-child cookie switches by kid"))
	if err != nil {
		return nil, fmt.Errorf("construct schoology_view_child_switches_total: %w", err)
	}

	return t, nil
}

// RecordItemDropped is a nil-safe shim over the embedded
// *connector.Metrics method of the same name. Direct method promotion
// would panic when t is nil (the embedded pointer is nil too); processors
// pass tel through optional code paths and rely on uniform nil-safety, so
// every framework counter we expect to call defensively gets its own
// nil-safe wrapper here.
func (t *Telemetry) RecordItemDropped(reason string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordItemDropped(reason)
}

// RecordPoll wraps the framework RecordPoll with nil-safety. See
// RecordItemDropped for the rationale.
func (t *Telemetry) RecordPoll(status string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordPoll(status)
}

// RecordItemProduced wraps the framework RecordItemProduced with
// nil-safety.
func (t *Telemetry) RecordItemProduced(destination string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordItemProduced(destination)
}

// RecordError wraps the framework RecordError with nil-safety.
func (t *Telemetry) RecordError(errType string) {
	if t == nil || t.Metrics == nil {
		return
	}
	t.Metrics.RecordError(errType)
}

// RecordPollByTrigger increments schoology_polls_total{trigger_source}.
// triggerSource is one of "scheduled", "catch_up", "triggered" per spec
// §13.1.
func (t *Telemetry) RecordPollByTrigger(triggerSource string) {
	if t == nil {
		return
	}
	t.pollsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("trigger_source", triggerSource),
	))
}

// RecordParseFailure increments schoology_parse_failures_total. Called
// per occurrence (i.e. once per row that failed to parse, not once per
// receipt). targetKid may be empty for inbox-level failures.
func (t *Telemetry) RecordParseFailure(parser, errorClass, targetKid, contentType string) {
	if t == nil {
		return
	}
	t.parseFailuresTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("parser", parser),
		attribute.String("error_class", errorClass),
		attribute.String("target_kid", targetKid),
		attribute.String("content_type", contentType),
	))
}

// RecordParseFailureReceipt increments schoology_parse_failure_receipts_total.
// Called only on the FIRST observation of a (parser, error_class) per poll,
// i.e. when ReceiptDedup.Observe returned true and a receipt was actually
// staged.
func (t *Telemetry) RecordParseFailureReceipt(parser, errorClass string) {
	if t == nil {
		return
	}
	t.parseFailureReceipts.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("parser", parser),
		attribute.String("error_class", errorClass),
	))
}

// RecordAttachmentBytes increments schoology_attachments_downloaded_bytes_total
// by n. Called per successfully-staged attachment.
func (t *Telemetry) RecordAttachmentBytes(kid, contentType string, n int64) {
	if t == nil {
		return
	}
	t.attachmentsBytesTotal.Add(context.Background(), n, metric.WithAttributes(
		attribute.String("kid", kid),
		attribute.String("content_type", contentType),
	))
}

// RecordAttachmentSkipped increments schoology_attachments_skipped_total{reason}.
// reason matches the Skip* constants in attachments.go (size_exceeded,
// download_error, ...).
func (t *Telemetry) RecordAttachmentSkipped(reason string) {
	if t == nil {
		return
	}
	t.attachmentsSkippedTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("reason", reason),
	))
}

// RecordTriggerRequest increments schoology_trigger_requests_total{outcome}.
// outcome is one of "accepted", "debounced", "unauthorized" per spec §13.1.
func (t *Telemetry) RecordTriggerRequest(outcome string) {
	if t == nil {
		return
	}
	t.triggerRequestsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
	))
}

// RecordViewChildSwitch increments schoology_view_child_switches_total{kid}.
func (t *Telemetry) RecordViewChildSwitch(kid string) {
	if t == nil {
		return
	}
	t.viewChildSwitchesTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("kid", kid),
	))
}

// StartSpan is a thin wrapper over the connector tracer's Start. Callers
// MUST defer span.End(). When the receiver is nil, StartSpan still
// returns a usable (no-op) ctx + span via the OTel global tracer so
// callers can keep their defer chain unconditional.
func (t *Telemetry) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil {
		return otel.Tracer("schoology").Start(ctx, name, trace.WithAttributes(attrs...))
	}
	return t.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}
