package records

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metric names (spec 4.14, plus answers_total for the 4.3 "alert" rows).
// Labels are only: connector, dir, reason, code, action -- each from a
// fixed set. No subject id, key, scope value or field value is ever a
// label (metrics_test.go pins this).
const (
	metricFiles       = "glovebox_journal_spool_files"
	metricBytes       = "glovebox_journal_spool_bytes"
	metricOldest      = "glovebox_journal_spool_oldest_seconds"
	metricDropped     = "glovebox_journal_spool_dropped_total"
	metricRejected    = "glovebox_journal_spool_rejected_total"
	metricAnswers     = "glovebox_journal_spool_answers_total"
	metricWriteErrors = "glovebox_journal_spool_write_errors_total"
	metricCut         = "glovebox_journal_record_values_cut_total"
)

// Dropped reasons.
const (
	DropMaxAge                = "max_age"
	DropMaxBytes              = "max_bytes"
	DropUnknownSubjectExpired = "unknown_subject_expired"
	DropErasedSubject         = "erased_subject"
)

// RejectUnreadable labels a spool file whose name or content is not a
// snapshot's (moved to rejected/ when found in spool/). A file whose read
// failed is not one: it is kept and read again.
const RejectUnreadable = "unreadable"

// Metrics records the spool's metrics. A nil *Metrics records nothing.
type Metrics struct {
	conn attribute.KeyValue

	files       metric.Int64Gauge
	bytes       metric.Int64Gauge
	oldest      metric.Float64Gauge
	dropped     metric.Int64Counter
	rejected    metric.Int64Counter
	answers     metric.Int64Counter
	writeErrors metric.Int64Counter
	cut         metric.Int64Counter
}

// NewMetrics registers the spool's instruments on meter (the connector
// framework's provider: connector.Metrics.Provider().Meter(...)).
func NewMetrics(meter metric.Meter, connectorName string) (*Metrics, error) {
	m := &Metrics{conn: attribute.String("connector", connectorName)}
	var err error
	if m.files, err = meter.Int64Gauge(metricFiles,
		metric.WithDescription("Files in the record spool, by directory (pending, rejected)")); err != nil {
		return nil, err
	}
	if m.bytes, err = meter.Int64Gauge(metricBytes,
		metric.WithDescription("Bytes in the record spool, both directories (bounded by max_bytes)")); err != nil {
		return nil, err
	}
	if m.oldest, err = meter.Float64Gauge(metricOldest,
		metric.WithDescription("Age of the oldest pending spool file in seconds (0 when empty)")); err != nil {
		return nil, err
	}
	if m.dropped, err = meter.Int64Counter(metricDropped,
		metric.WithDescription("Spool files deleted unsent, by reason")); err != nil {
		return nil, err
	}
	if m.rejected, err = meter.Int64Counter(metricRejected,
		metric.WithDescription("Spool files moved to rejected/, by reason")); err != nil {
		return nil, err
	}
	if m.answers, err = meter.Int64Counter(metricAnswers,
		metric.WithDescription("Journal answers to the spool, by code, reason and action (code 0: transport)")); err != nil {
		return nil, err
	}
	if m.writeErrors, err = meter.Int64Counter(metricWriteErrors,
		metric.WithDescription("Snapshots that could not be written to the spool")); err != nil {
		return nil, err
	}
	if m.cut, err = meter.Int64Counter(metricCut,
		metric.WithDescription("Record text values cut to 64 KiB")); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Metrics) setGauges(pending, rejected int, bytes int64, oldestSeconds float64) {
	if m == nil {
		return
	}
	ctx := context.Background()
	m.files.Record(ctx, int64(pending), metric.WithAttributes(m.conn, attribute.String("dir", "pending")))
	m.files.Record(ctx, int64(rejected), metric.WithAttributes(m.conn, attribute.String("dir", "rejected")))
	m.bytes.Record(ctx, bytes, metric.WithAttributes(m.conn))
	m.oldest.Record(ctx, oldestSeconds, metric.WithAttributes(m.conn))
}

func (m *Metrics) droppedN(reason string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.dropped.Add(context.Background(), int64(n), metric.WithAttributes(m.conn, attribute.String("reason", reason)))
}

// rejectedOne counts a file moved to rejected/. reason is a ReasonLabel or
// RejectUnreadable.
func (m *Metrics) rejectedOne(reason string) {
	if m == nil {
		return
	}
	m.rejected.Add(context.Background(), 1, metric.WithAttributes(m.conn, attribute.String("reason", reason)))
}

func (m *Metrics) answer(code int, reason string, act Action) {
	if m == nil {
		return
	}
	m.answers.Add(context.Background(), 1, metric.WithAttributes(m.conn,
		attribute.String("code", codeLabel(code)), attribute.String("reason", ReasonLabel(reason)),
		attribute.String("action", act.String())))
}

func (m *Metrics) writeError() {
	if m == nil {
		return
	}
	m.writeErrors.Add(context.Background(), 1, metric.WithAttributes(m.conn))
}

func (m *Metrics) cutOne() {
	if m == nil {
		return
	}
	m.cut.Add(context.Background(), 1, metric.WithAttributes(m.conn))
}

// codeLabel bounds an HTTP status for a label: 0 (transport) or 100-599.
func codeLabel(code int) string {
	if code < 100 || code > 599 {
		return "0"
	}
	return strconv.Itoa(code)
}
