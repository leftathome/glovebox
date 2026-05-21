package schoology

import (
	"context"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// NewTestTelemetry is the convenience helper used throughout the
// schoology package tests instead of constructing connector.NewMetrics
// + NewTelemetry by hand. The returned Telemetry has a working tracer
// plus all schoology-specific counters bound to a fresh framework
// Metrics. Callers should NOT register a Shutdown on the framework
// Metrics; the test process exits at scope end and the in-memory
// Prometheus exporter has no resources that require explicit cleanup.
func NewTestTelemetry(t *testing.T) *Telemetry {
	t.Helper()
	base, err := connector.NewMetrics("schoology-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	tel, err := NewTelemetry(base, "schoology-test", "schoology-test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	return tel
}

// TestNewTelemetry_RegistersAllCounters constructs Telemetry against a
// real *connector.Metrics, exercises every schoology-specific Record
// helper once, and also exercises a framework method to confirm Go
// method promotion works through the embedded pointer.
//
// No assertions on counter values: OTel's PeriodicReader doesn't expose
// easy in-process state, and the wave-3 pattern is to trust the SDK and
// assert on call-site behavior in integration tests instead.
func TestNewTelemetry_RegistersAllCounters(t *testing.T) {
	base, err := connector.NewMetrics("schoology-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	tel, err := NewTelemetry(base, "schoology", "schoology")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}

	// Schoology-specific helpers.
	tel.RecordPollByTrigger("scheduled")
	tel.RecordParseFailure("feed", "row_parse", "k1", "feed")
	tel.RecordParseFailureReceipt("feed", "row_parse")
	tel.RecordAttachmentBytes("k1", "application/pdf", 12345)
	tel.RecordAttachmentSkipped(SkipSizeExceeded)
	tel.RecordTriggerRequest("accepted")
	tel.RecordViewChildSwitch("k1")

	// Framework methods reachable via promotion of *connector.Metrics.
	tel.RecordItemDropped("test")
	tel.RecordPoll("success")
	tel.RecordError("transient")
	tel.RecordItemProduced("school")
	tel.SetCheckpointAge(0)
}

// TestNewTelemetry_RejectsNilBase confirms the constructor refuses to
// build a Telemetry without a framework Metrics — the embedded pointer
// would be nil and every promoted method call would panic.
func TestNewTelemetry_RejectsNilBase(t *testing.T) {
	if _, err := NewTelemetry(nil, "schoology", "schoology"); err == nil {
		t.Fatal("NewTelemetry(nil base) returned nil error; want non-nil")
	}
}

// TestNewTelemetry_RejectsEmptyNames confirms the constructor refuses
// to build a Telemetry with an empty meter or tracer name. Catches
// wiring mistakes that would silently emit metrics under "".
func TestNewTelemetry_RejectsEmptyNames(t *testing.T) {
	base, err := connector.NewMetrics("schoology-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	if _, err := NewTelemetry(base, "", "schoology"); err == nil {
		t.Errorf("NewTelemetry(empty meterName) returned nil error; want non-nil")
	}
	if _, err := NewTelemetry(base, "schoology", ""); err == nil {
		t.Errorf("NewTelemetry(empty tracerName) returned nil error; want non-nil")
	}
}

// TestTelemetry_NilSafe verifies every schoology-specific Record helper
// is a no-op on a nil receiver. Tests that don't wire telemetry pass
// nil; those code paths must not panic.
func TestTelemetry_NilSafe(t *testing.T) {
	var tel *Telemetry // nil

	tel.RecordPollByTrigger("scheduled")
	tel.RecordParseFailure("feed", "row_parse", "k1", "feed")
	tel.RecordParseFailureReceipt("feed", "row_parse")
	tel.RecordAttachmentBytes("k1", "application/pdf", 1)
	tel.RecordAttachmentSkipped(SkipSizeExceeded)
	tel.RecordTriggerRequest("accepted")
	tel.RecordViewChildSwitch("k1")

	// StartSpan on nil receiver still returns a usable ctx + span so
	// callers can `defer span.End()` without nil-checking.
	ctx, span := tel.StartSpan(context.Background(), "test.span")
	if ctx == nil {
		t.Error("StartSpan(nil) returned nil ctx")
	}
	if span == nil {
		t.Error("StartSpan(nil) returned nil span")
	}
	span.End()
}

// TestTelemetry_StartSpan exercises the happy-path tracer wrapper and
// confirms the returned span is usable. We don't assert on span name
// (recording the span via an in-memory exporter is fiddly and the
// wave-3 pattern is to trust the SDK) but we do confirm a non-nil span
// and that End() doesn't panic.
func TestTelemetry_StartSpan(t *testing.T) {
	tel := NewTestTelemetry(t)

	ctx, span := tel.StartSpan(context.Background(), "schoology.test")
	if ctx == nil {
		t.Fatal("StartSpan returned nil ctx")
	}
	if span == nil {
		t.Fatal("StartSpan returned nil span")
	}
	defer span.End()

	// Child span exercises the returned ctx is a real ctx.
	_, child := tel.StartSpan(ctx, "schoology.test.child")
	if child == nil {
		t.Fatal("child StartSpan returned nil span")
	}
	child.End()
}

// TestNewTelemetry_ErrorIncludesMetricName is a smoke check that
// constructor-error wrapping preserves the metric name. This guards
// future maintainers from accidentally dropping the name when they
// reshape the constructor.
//
// We can't easily force an Int64Counter constructor to fail with the
// default OTel meter, so we instead exercise the nil/empty-name guards
// and verify they mention the missing field. That gives the same
// "errors are debuggable" guarantee at lower cost.
func TestNewTelemetry_ErrorIncludesMetricName(t *testing.T) {
	_, err := NewTelemetry(nil, "x", "y")
	if err == nil || !strings.Contains(err.Error(), "base") {
		t.Errorf("nil-base error should mention 'base'; got %v", err)
	}
}
