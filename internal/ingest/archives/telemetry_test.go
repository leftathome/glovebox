package archives

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNewTelemetry_RegistersAllInstruments constructs Telemetry against
// the global OTel meter, exercises every Record/Set helper once, and
// confirms no panic. No assertions on metric values: OTel's
// PeriodicReader doesn't expose easy in-process state, and the
// established pattern in this repo (see connectors/schoology/telemetry_test.go)
// is to trust the SDK and assert on call-site behavior in integration
// tests instead.
func TestNewTelemetry_RegistersAllInstruments(t *testing.T) {
	tel, err := NewTelemetry("glovebox-test", "glovebox-test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	ctx := context.Background()

	// Spec 13 §7.1 archive metrics.
	tel.RecordUploadCreated(ctx, "recognizer", "archive/mbox")
	tel.RecordUploadCompleted(ctx, "recognizer", "archive/mbox", time.Second)
	tel.RecordUploadFailed(ctx, "recognizer", "archive/mbox", "failed_sha256")
	tel.RecordUploadBytes(ctx, "recognizer", "archive/mbox", 4096)
	tel.RecordUploadInFlight(ctx, "recognizer", 1)
	tel.RecordUploadInFlight(ctx, "recognizer", -1)
	tel.RecordPatchChunkBytes(ctx, 65536)
	tel.RecordExtractedEntries(ctx, "archive/google-takeout-subtree", 42)
	tel.RecordExtractedBytes(ctx, "archive/google-takeout-subtree", 1<<20)
	tel.SetStorageBytes(ctx, 1<<30)
	tel.SetStoragePct(ctx, 0.42)
	tel.SetStorageSourceBytes(ctx, "recognizer", 1<<20)
	tel.RecordConcurrentRejected(ctx, "recognizer", "per_source")
	tel.RecordConcurrentRejected(ctx, "recognizer", "global")
	tel.RecordPatchIdleTimeout(ctx, "recognizer")
	tel.RecordTarSafetyReject(ctx, "recognizer", string(TarRejectNameTraversal))

	// Spec 10 §6.4 / spec 13 §7.1 ingest auth metrics.
	tel.RecordAuth(ctx, "/v1/archives", "accepted")
	tel.RecordAuth(ctx, "/v1/archives", "rejected")
	tel.RecordAuth(ctx, "/v1/archives", "rate_limited")
	tel.RecordAuthRejectedByIP(ctx, "10.0.0.0/8")
	tel.RecordTokenLoadError(ctx, "recognizer")

	// Tracer surface.
	spanCtx, span := tel.StartSpan(ctx, "glovebox.archive.test")
	if spanCtx == nil {
		t.Error("StartSpan returned nil ctx")
	}
	if span == nil {
		t.Error("StartSpan returned nil span")
	}
	span.End()
}

// TestNewTelemetry_RejectsEmptyNames confirms the constructor refuses
// to build a Telemetry with an empty meter or tracer name. Catches
// wiring mistakes that would silently emit metrics under "".
func TestNewTelemetry_RejectsEmptyNames(t *testing.T) {
	if _, err := NewTelemetry("", "glovebox-test"); err == nil {
		t.Error("NewTelemetry(empty meterName) returned nil error; want non-nil")
	} else if !strings.Contains(err.Error(), "meterName") {
		t.Errorf("empty-meter error should mention 'meterName'; got %v", err)
	}
	if _, err := NewTelemetry("glovebox-test", ""); err == nil {
		t.Error("NewTelemetry(empty tracerName) returned nil error; want non-nil")
	} else if !strings.Contains(err.Error(), "tracerName") {
		t.Errorf("empty-tracer error should mention 'tracerName'; got %v", err)
	}
}

// TestTelemetry_NilSafe verifies every Record/Set helper is a no-op on
// a nil receiver. Tests and the C2 nil-passing variants pass nil; those
// code paths must not panic.
func TestTelemetry_NilSafe(t *testing.T) {
	var tel *Telemetry // nil
	ctx := context.Background()

	tel.RecordUploadCreated(ctx, "x", "archive/mbox")
	tel.RecordUploadCompleted(ctx, "x", "archive/mbox", time.Second)
	tel.RecordUploadFailed(ctx, "x", "archive/mbox", "failed_sha256")
	tel.RecordUploadBytes(ctx, "x", "archive/mbox", 1)
	tel.RecordUploadInFlight(ctx, "x", 1)
	tel.RecordPatchChunkBytes(ctx, 1)
	tel.RecordExtractedEntries(ctx, "archive/google-takeout-subtree", 1)
	tel.RecordExtractedBytes(ctx, "archive/google-takeout-subtree", 1)
	tel.SetStorageBytes(ctx, 1)
	tel.SetStoragePct(ctx, 0.1)
	tel.SetStorageSourceBytes(ctx, "x", 1)
	tel.RecordConcurrentRejected(ctx, "x", "per_source")
	tel.RecordPatchIdleTimeout(ctx, "x")
	tel.RecordTarSafetyReject(ctx, "x", string(TarRejectNameTraversal))
	tel.RecordAuth(ctx, "/v1/archives", "rejected")
	tel.RecordAuthRejectedByIP(ctx, "10.0.0.0/8")
	tel.RecordTokenLoadError(ctx, "x")
}

// TestTelemetry_StartSpanOnNilUsesGlobalTracer verifies that StartSpan
// on a nil receiver still returns a usable ctx + span via the OTel
// global tracer (fallback name "glovebox.archive"), so callers can keep
// `defer span.End()` unconditional without nil-checking.
func TestTelemetry_StartSpanOnNilUsesGlobalTracer(t *testing.T) {
	var tel *Telemetry // nil

	ctx, span := tel.StartSpan(context.Background(), "test.span")
	if ctx == nil {
		t.Fatal("StartSpan(nil receiver) returned nil ctx")
	}
	if span == nil {
		t.Fatal("StartSpan(nil receiver) returned nil span")
	}
	// span.End() must not panic; defer would also work but we want the
	// panic (if any) to be attributed to this test, not deferred cleanup.
	span.End()
}

// TestTelemetry_StartSpanChild exercises the happy-path tracer wrapper
// and confirms the returned ctx carries the parent span so a child
// StartSpan call can attach to it.
func TestTelemetry_StartSpanChild(t *testing.T) {
	tel, err := NewTelemetry("glovebox-test", "glovebox-test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	ctx, parent := tel.StartSpan(context.Background(), "glovebox.archive.upload")
	if parent == nil {
		t.Fatal("parent StartSpan returned nil span")
	}
	defer parent.End()

	_, child := tel.StartSpan(ctx, "glovebox.archive.create")
	if child == nil {
		t.Fatal("child StartSpan returned nil span")
	}
	child.End()
}
