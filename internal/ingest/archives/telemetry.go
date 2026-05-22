// Telemetry for the archive-delivery ingest endpoint (spec 13 §7).
//
// Spec 13 §7 defines three observability surfaces:
//
//  1. §7.1 metrics -- 15 OTel instruments under the
//     "glovebox_archive_*" and "glovebox_ingest_*" namespaces.
//  2. §7.2 traces -- a root span (`glovebox.archive.upload`) plus four
//     child spans (`create`, `patch`, `finalize`, `untar`). The tracer
//     is supplied by NewTelemetry; callers wire the spans.
//  3. §7.3 structured logs -- typed event-name constants and closed-set
//     reason enums so callers import the constant instead of passing
//     free-form strings that would break log-aggregator queries.
//
// This file mirrors the spec-12 schoology connector's Telemetry pattern
// (connectors/schoology/telemetry.go). Differences from that precedent:
//
//   - This package is internal (not a connector), so there is no
//     embedded *connector.Metrics; every metric here is owned by this
//     Telemetry.
//   - The instrument set is spec 13 §7.1 (archive delivery) rather than
//     spec 12 §13.1 (schoology).
//
// Structural choices preserved from the precedent:
//
//   - Every Record* / Set* helper is nil-safe (early-return on t == nil).
//     Callers that don't wire telemetry (most tests, the C2 nil-passing
//     variants) can pass nil without panicking.
//   - StartSpan on a nil receiver still returns a usable ctx + span via
//     the OTel global tracer so callers can keep `defer span.End()`
//     unconditional.
//   - NewTelemetry refuses empty meter or tracer names; silent emission
//     under an empty meter name is a wiring bug that would survive
//     review unnoticed.
//   - Each metric constructor error is wrapped with the metric name
//     ("construct <name>: <err>") so a registry conflict is debuggable.
//
// Label-value conventions worth flagging at this surface:
//
//   - glovebox_ingest_auth_rejected_total carries a `remote_ip_bucket`
//     label whose values are CIDR strings (e.g. "10.0.0.0/8") produced
//     by the BucketIP helper in internal/ingest/auth/proxy.go. This
//     package doesn't import that helper; callers do, and pass the
//     bucketed string into RecordAuthRejectedByIP.
package archives

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry is the archive-delivery observability handle. Every field
// below is constructed by NewTelemetry; no field is publicly settable.
type Telemetry struct {
	// Spec 13 §7.1 archive metrics.
	uploadsTotal             metric.Int64Counter       // status: created/completed/failed_*/terminated
	uploadBytesTotal         metric.Int64Counter       // sum of successfully received bytes
	uploadDuration           metric.Float64Histogram   // POST through finalize seconds
	uploadInFlight           metric.Int64UpDownCounter // current active uploads per source
	patchChunkBytes          metric.Int64Histogram     // per-PATCH body size
	extractedEntriesTotal    metric.Int64Counter       // tar entries written
	extractedBytesTotal      metric.Int64Counter       // tar bytes written
	storageBytes             metric.Int64Gauge         // bytes under archives/
	storagePct               metric.Float64Gauge       // storageBytes / PVC capacity
	storageSourceBytes       metric.Int64Gauge         // bytes attributable to one source
	concurrentRejectedTotal  metric.Int64Counter       // scope: per_source/global
	patchIdleTimeoutTotal    metric.Int64Counter       // §4.4 PATCH idle timeouts
	tarSafetyRejectionsTotal metric.Int64Counter       // reason: closed enum per §4.7
	// Spec 10 §6.4 / spec 13 §7.1 auth metrics.
	authTotal              metric.Int64Counter // status: accepted/rejected/rate_limited
	authRejectedByIPBucket metric.Int64Counter // remote_ip_bucket label
	tokenLoadErrorsTotal   metric.Int64Counter // per-source token load errors

	tracer trace.Tracer
}

// NewTelemetry constructs the archive-delivery telemetry handle.
//
// meterName scopes the 15 OTel instruments; tracerName scopes the
// tracer used by StartSpan. Both are required; an empty value is
// refused so wiring mistakes can't silently emit under "". "glovebox"
// is the conventional production value for both, but tests pass
// shorter strings.
//
// Every Record* / Set* method is nil-safe so callers that pass nil
// (mostly tests and the C2 nil-passing variants) don't panic.
func NewTelemetry(meterName, tracerName string) (*Telemetry, error) {
	if meterName == "" {
		return nil, fmt.Errorf("archives.NewTelemetry: meterName is required")
	}
	if tracerName == "" {
		return nil, fmt.Errorf("archives.NewTelemetry: tracerName is required")
	}

	meter := otel.Meter(meterName)
	t := &Telemetry{tracer: otel.Tracer(tracerName)}

	var err error

	t.uploadsTotal, err = meter.Int64Counter("glovebox_archive_uploads_total",
		metric.WithDescription("Archive uploads by source_id, media_type, status"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_uploads_total: %w", err)
	}
	t.uploadBytesTotal, err = meter.Int64Counter("glovebox_archive_upload_bytes_total",
		metric.WithDescription("Sum of successfully received archive bytes by source_id, media_type"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_upload_bytes_total: %w", err)
	}
	t.uploadDuration, err = meter.Float64Histogram("glovebox_archive_upload_duration_seconds",
		metric.WithDescription("Archive upload duration in seconds, POST through finalize"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_upload_duration_seconds: %w", err)
	}
	t.uploadInFlight, err = meter.Int64UpDownCounter("glovebox_archive_upload_in_flight",
		metric.WithDescription("Currently active archive uploads per source_id"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_upload_in_flight: %w", err)
	}
	t.patchChunkBytes, err = meter.Int64Histogram("glovebox_archive_patch_chunk_bytes",
		metric.WithDescription("Per-PATCH body size in bytes"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_patch_chunk_bytes: %w", err)
	}
	t.extractedEntriesTotal, err = meter.Int64Counter("glovebox_archive_extracted_entries_total",
		metric.WithDescription("Total entries extracted from tarballs by media_type"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_extracted_entries_total: %w", err)
	}
	t.extractedBytesTotal, err = meter.Int64Counter("glovebox_archive_extracted_bytes_total",
		metric.WithDescription("Total bytes written during untar by media_type"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_extracted_bytes_total: %w", err)
	}
	t.storageBytes, err = meter.Int64Gauge("glovebox_archive_storage_bytes",
		metric.WithDescription("Current bytes under archives/"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_storage_bytes: %w", err)
	}
	t.storagePct, err = meter.Float64Gauge("glovebox_archive_storage_pct",
		metric.WithDescription("archive_storage_bytes / PVC capacity, range 0.0..1.0"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_storage_pct: %w", err)
	}
	t.storageSourceBytes, err = meter.Int64Gauge("glovebox_archive_storage_source_bytes",
		metric.WithDescription("Bytes attributable to one source_id"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_storage_source_bytes: %w", err)
	}
	t.concurrentRejectedTotal, err = meter.Int64Counter("glovebox_archive_concurrent_uploads_rejected_total",
		metric.WithDescription("Uploads rejected by concurrent-upload cap; scope ∈ per_source/global"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_concurrent_uploads_rejected_total: %w", err)
	}
	t.patchIdleTimeoutTotal, err = meter.Int64Counter("glovebox_archive_patch_idle_timeout_total",
		metric.WithDescription("PATCH terminated by §4.4 idle timeout, by source_id"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_patch_idle_timeout_total: %w", err)
	}
	t.tarSafetyRejectionsTotal, err = meter.Int64Counter("glovebox_archive_tar_safety_rejections_total",
		metric.WithDescription("Tar-safety rejections by source_id and §4.7 reason enum"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_archive_tar_safety_rejections_total: %w", err)
	}
	t.authTotal, err = meter.Int64Counter("glovebox_ingest_auth_total",
		metric.WithDescription("Ingest auth outcomes by endpoint and status (accepted/rejected/rate_limited)"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_ingest_auth_total: %w", err)
	}
	t.authRejectedByIPBucket, err = meter.Int64Counter("glovebox_ingest_auth_rejected_total",
		metric.WithDescription("Rejected ingest auth attempts by low-cardinality remote_ip_bucket (CIDR string)"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_ingest_auth_rejected_total: %w", err)
	}
	t.tokenLoadErrorsTotal, err = meter.Int64Counter("glovebox_ingest_token_load_errors_total",
		metric.WithDescription("Spec 10 §4.1 per-source token-load errors (malformed Vault entry, etc.)"))
	if err != nil {
		return nil, fmt.Errorf("construct glovebox_ingest_token_load_errors_total: %w", err)
	}

	return t, nil
}

// RecordUploadCreated bumps glovebox_archive_uploads_total{status="created"}.
// Call once on a successful POST /v1/archives that allocated an upload id.
func (t *Telemetry) RecordUploadCreated(ctx context.Context, sourceID, mediaType string) {
	if t == nil {
		return
	}
	t.uploadsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("media_type", mediaType),
		attribute.String("status", "created"),
	))
}

// RecordUploadCompleted bumps glovebox_archive_uploads_total{status="completed"}
// AND observes glovebox_archive_upload_duration_seconds. Call once per
// upload that reaches finalize successfully. duration is the wall-clock
// time from POST to finalize completion.
func (t *Telemetry) RecordUploadCompleted(ctx context.Context, sourceID, mediaType string, duration time.Duration) {
	if t == nil {
		return
	}
	t.uploadsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("media_type", mediaType),
		attribute.String("status", "completed"),
	))
	t.uploadDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("media_type", mediaType),
	))
}

// RecordUploadFailed bumps glovebox_archive_uploads_total with the
// reason as the status label. reason is one of the spec §7.1 status
// values: failed_sha256, failed_size, failed_untar, failed_auth,
// failed_quota, terminated.
func (t *Telemetry) RecordUploadFailed(ctx context.Context, sourceID, mediaType, reason string) {
	if t == nil {
		return
	}
	t.uploadsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("media_type", mediaType),
		attribute.String("status", reason),
	))
}

// RecordUploadBytes adds n to glovebox_archive_upload_bytes_total.
// Called per successfully-received PATCH chunk (n is the chunk length).
func (t *Telemetry) RecordUploadBytes(ctx context.Context, sourceID, mediaType string, n int64) {
	if t == nil {
		return
	}
	t.uploadBytesTotal.Add(ctx, n, metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("media_type", mediaType),
	))
}

// RecordUploadInFlight adjusts glovebox_archive_upload_in_flight by
// delta. Callers pass +1 on POST success and -1 on finalize/abort/DELETE
// so the gauge tracks currently-active uploads per source.
func (t *Telemetry) RecordUploadInFlight(ctx context.Context, sourceID string, delta int64) {
	if t == nil {
		return
	}
	t.uploadInFlight.Add(ctx, delta, metric.WithAttributes(
		attribute.String("source_id", sourceID),
	))
}

// RecordPatchChunkBytes records the size of a single PATCH body into
// glovebox_archive_patch_chunk_bytes. No labels per §7.1.
func (t *Telemetry) RecordPatchChunkBytes(ctx context.Context, n int64) {
	if t == nil {
		return
	}
	t.patchChunkBytes.Record(ctx, n)
}

// RecordExtractedEntries adds n to glovebox_archive_extracted_entries_total.
// Call once with the final entry count at the end of an untar, not per
// entry, so the metric matches the per-upload "entries_extracted"
// receipt field.
func (t *Telemetry) RecordExtractedEntries(ctx context.Context, mediaType string, n int64) {
	if t == nil {
		return
	}
	t.extractedEntriesTotal.Add(ctx, n, metric.WithAttributes(
		attribute.String("media_type", mediaType),
	))
}

// RecordExtractedBytes adds n to glovebox_archive_extracted_bytes_total.
// Call once with the final byte count at the end of an untar.
func (t *Telemetry) RecordExtractedBytes(ctx context.Context, mediaType string, n int64) {
	if t == nil {
		return
	}
	t.extractedBytesTotal.Add(ctx, n, metric.WithAttributes(
		attribute.String("media_type", mediaType),
	))
}

// SetStorageBytes sets glovebox_archive_storage_bytes to n. Called
// periodically by the storage scanner, not per-upload.
func (t *Telemetry) SetStorageBytes(ctx context.Context, n int64) {
	if t == nil {
		return
	}
	t.storageBytes.Record(ctx, n)
}

// SetStoragePct sets glovebox_archive_storage_pct to pct. pct is in
// range 0.0..1.0; the storage scanner divides used / capacity.
func (t *Telemetry) SetStoragePct(ctx context.Context, pct float64) {
	if t == nil {
		return
	}
	t.storagePct.Record(ctx, pct)
}

// SetStorageSourceBytes sets glovebox_archive_storage_source_bytes{source_id}
// to n. Called periodically by the storage scanner, one observation per
// source.
func (t *Telemetry) SetStorageSourceBytes(ctx context.Context, sourceID string, n int64) {
	if t == nil {
		return
	}
	t.storageSourceBytes.Record(ctx, n, metric.WithAttributes(
		attribute.String("source_id", sourceID),
	))
}

// RecordConcurrentRejected bumps
// glovebox_archive_concurrent_uploads_rejected_total when a POST is
// rejected for tripping a concurrent-upload cap. scope is "per_source"
// or "global" per §5.4.
func (t *Telemetry) RecordConcurrentRejected(ctx context.Context, sourceID, scope string) {
	if t == nil {
		return
	}
	t.concurrentRejectedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("scope", scope),
	))
}

// RecordPatchIdleTimeout bumps glovebox_archive_patch_idle_timeout_total
// when a PATCH is terminated by the §4.4 idle timeout.
func (t *Telemetry) RecordPatchIdleTimeout(ctx context.Context, sourceID string) {
	if t == nil {
		return
	}
	t.patchIdleTimeoutTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
	))
}

// RecordTarSafetyReject bumps glovebox_archive_tar_safety_rejections_total
// with the source_id and a §4.7 reason. Callers SHOULD pass a TarRejectReason
// constant from this package; the parameter is string-typed so the
// metric helper can also accept raw strings from older call sites.
func (t *Telemetry) RecordTarSafetyReject(ctx context.Context, sourceID, reason string) {
	if t == nil {
		return
	}
	t.tarSafetyRejectionsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
		attribute.String("reason", reason),
	))
}

// RecordAuth bumps glovebox_ingest_auth_total{endpoint, status}.
// endpoint is "/v1/ingest" or "/v1/archives"; status is "accepted",
// "rejected", or "rate_limited".
func (t *Telemetry) RecordAuth(ctx context.Context, endpoint, status string) {
	if t == nil {
		return
	}
	t.authTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", endpoint),
		attribute.String("status", status),
	))
}

// RecordAuthRejectedByIP bumps glovebox_ingest_auth_rejected_total with
// a low-cardinality bucketed CIDR string (produced by the BucketIP
// helper in internal/ingest/auth/proxy.go).
func (t *Telemetry) RecordAuthRejectedByIP(ctx context.Context, ipBucket string) {
	if t == nil {
		return
	}
	t.authRejectedByIPBucket.Add(ctx, 1, metric.WithAttributes(
		attribute.String("remote_ip_bucket", ipBucket),
	))
}

// RecordTokenLoadError bumps glovebox_ingest_token_load_errors_total
// when a per-source Vault token entry is skipped at reload time
// (spec 10 §4.1). Fires per reload attempt that skips an entry.
func (t *Telemetry) RecordTokenLoadError(ctx context.Context, sourceID string) {
	if t == nil {
		return
	}
	t.tokenLoadErrorsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source_id", sourceID),
	))
}

// StartSpan is a thin wrapper over the package tracer's Start. Callers
// MUST defer span.End(). When the receiver is nil, StartSpan still
// returns a usable (no-op) ctx + span via the OTel global tracer so
// callers can keep their defer chain unconditional.
func (t *Telemetry) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil {
		return otel.Tracer("glovebox.archive").Start(ctx, name, trace.WithAttributes(attrs...))
	}
	return t.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// Spec 13 §7.3 event names. Constants prevent typos that would break
// log-aggregator queries downstream.
const (
	EventUploadCreated    = "glovebox archive upload created"
	EventUploadCompleted  = "glovebox archive upload completed"
	EventUploadAborted    = "glovebox archive upload aborted"
	EventTarSafetyReject  = "glovebox archive upload tar safety reject"
	EventStorageNearCap   = "glovebox archive storage near cap"
	EventStorageHardCap   = "glovebox archive storage hard cap"
	EventCleanup          = "glovebox archive cleanup"
	EventIngestAuthOK     = "glovebox ingest authenticated"
	EventIngestAuthReject = "glovebox ingest auth rejected"
	EventIngestAuthRL     = "glovebox ingest auth rate limited"
)

// AbortReason is the closed-set enum for the `reason` field of the
// EventUploadAborted log event. Callers import these constants rather
// than passing free-form strings so log-aggregator queries against
// `reason=...` remain stable across the codebase.
type AbortReason string

const (
	AbortSHA256Mismatch      AbortReason = "sha256_mismatch"
	AbortSizeMismatch        AbortReason = "size_mismatch"
	AbortTarUnsafeEntry      AbortReason = "tar_unsafe_entry"
	AbortQuotaExceeded       AbortReason = "quota_exceeded"
	AbortTerminatedByClient  AbortReason = "terminated_by_client"
	AbortProcessDied         AbortReason = "process_died"
	AbortIdempotencyConflict AbortReason = "idempotency_conflict"
)

// TarRejectReason is the closed-set enum for the `reason` field of the
// EventTarSafetyReject log event AND for the `reason` label on
// glovebox_archive_tar_safety_rejections_total. Values match spec §4.7
// verbatim; operators dashboard against this enum.
type TarRejectReason string

const (
	TarRejectPaxOverride     TarRejectReason = "pax_path_override"
	TarRejectTypeflag        TarRejectReason = "typeflag_disallowed"
	TarRejectNameInvalidUTF8 TarRejectReason = "name_invalid_utf8"
	TarRejectNameNUL         TarRejectReason = "name_contains_nul"
	TarRejectNameControl     TarRejectReason = "name_contains_control"
	TarRejectNameTooLong     TarRejectReason = "name_too_long"
	TarRejectNameTraversal   TarRejectReason = "name_traversal"
	TarRejectNameAbsolute    TarRejectReason = "name_absolute"
	TarRejectSizeTooLarge    TarRejectReason = "size_too_large"
	TarRejectCumulativeSize  TarRejectReason = "cumulative_size_too_large"
	TarRejectEntryCount      TarRejectReason = "entry_count_too_large"
	TarRejectSoftCap         TarRejectReason = "soft_cap_exceeded"
)
