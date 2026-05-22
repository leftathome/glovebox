// Package archives -- tus.io HTTP handler (spec 13 §4).
//
// This file owns the HTTP surface that speaks the tus.io v1.0.0 resumable
// upload protocol over /v1/archives*. It is implemented across three
// sub-tasks per the plan:
//
//   - C2a (this commit): OPTIONS + POST + pre-flight idempotency.
//     Establishes Handler / HandlerConfig / NewHandler, the
//     Tus-Resumable version-check helper, and a Mount placeholder.
//   - C2b: HEAD + PATCH + DELETE (per-upload-id mutex, rolling sha256,
//     finalize trigger, idle timeout).
//   - C2c: GET receipt + Mount wiring with the auth middleware from
//     Wave A.
//
// Design notes:
//
//   - The handler is constructed via NewHandler and is dependency-injected
//     with a *Store (B2), a *Telemetry (B4), a QuotaProvider (filled in
//     by C3), and a HandlerConfig. No global state lives in this package.
//   - Every error response body is a SINGLE-FIELD JSON object
//     `{"error":"<code>"}` per spec §4.3 / §5.4. We do not echo the
//     conflicting sha256 or source-id on 409, the failed metadata key on
//     400, or any other detail beyond a closed-set error code. This is a
//     deliberate security discipline (spec §4.3 acknowledged-leak
//     discussion) and the tests assert it.
//   - The pre-flight idempotency check (spec §4.3) is scoped by the
//     authenticated source-id, NOT by archive_id alone. A cross-source
//     archive_id collision returns 409, not 303, so one source-id can
//     never probe what another source-id has uploaded.
//   - Telemetry calls happen ONLY on success paths in this commit;
//     C2b/c add the failure-side RecordUploadFailed calls.
package archives

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/leftathome/glovebox/internal/ingest"
)

// tusVersion is the single tus.io protocol version this server speaks.
// Spec 13 §4.1 fixes this at "1.0.0"; any client sending a different
// Tus-Resumable value receives 412 Precondition Failed.
const tusVersion = "1.0.0"

// tusExtensions is the advertised Tus-Extension header value. Spec 13
// §4.1 lists creation, termination, and checksum.
const tusExtensions = "creation,termination,checksum"

// defaultTmpExpiry is the §5.5 cleanup horizon for in-flight tmp files.
// HandlerConfig.TmpExpiry overrides; if unset, NewHandler picks this
// default so callers don't have to know the spec constant.
const defaultTmpExpiry = 72 * time.Hour

// archivesBasePath is the public URL prefix for archive uploads.
// Centralized so the Location-header formatting is consistent across
// 201, 303, and (future) HEAD/PATCH/DELETE/GET responses.
const archivesBasePath = "/v1/archives"

// HandlerConfig holds the runtime knobs for the archive handler.
// Constructed once at wire-up time and passed by value to NewHandler;
// the Handler retains a copy.
type HandlerConfig struct {
	// StagingRoot is the absolute path under which archives/ and
	// .tmp-archives/ live (spec 13 §5.1).
	StagingRoot string

	// TusMaxSize is the protocol-level per-upload cap advertised on
	// OPTIONS and enforced on POST. Spec 13 §3.2 / §5.4 default is
	// 30 GiB; the Helm chart's ingest.archives.maxUploadSize overrides
	// at production. Zero means "no advertised cap" but POST still
	// enforces against the field, so leaving zero in production
	// effectively rejects every POST -- the wire-up code MUST set this.
	TusMaxSize int64

	// TmpExpiry controls the Tus-Expires header on POST/HEAD responses.
	// Plan default 72h; NewHandler fills in defaultTmpExpiry when zero.
	TmpExpiry time.Duration
}

// QuotaProvider is the seam C3 fills in with the real storage measurer
// (spec 13 §5.4 global hard cap). C2a only needs ShouldBlock to decide
// whether to 503 on POST; the production implementation reads
// archives/ + .tmp-archives/ totals from a background scanner.
//
// Defined as an interface so tests can pass a fakeQuota{block: ...} and
// the handler stays decoupled from the C3 measurement loop.
type QuotaProvider interface {
	// ShouldBlock returns true when new POST /v1/archives requests
	// should be rejected with 503 because the global hard cap is in
	// effect (spec §5.4: archives/ + .tmp-archives/ combined > 95% of
	// PVC). The lift threshold (85%) is the provider's concern.
	ShouldBlock() bool
}

// Handler implements the tus.io v1.0.0 protocol for /v1/archives*.
// It is constructed by NewHandler with all dependencies wired; the
// Mount method (added in C2c) attaches it to an http.ServeMux.
type Handler struct {
	store *Store
	quota QuotaProvider
	tel   *Telemetry
	cfg   HandlerConfig
}

// NewHandler constructs the handler. tel may be nil (every Telemetry
// helper is nil-safe). quota may NOT be nil; passing nil here means the
// 503 hard-cap branch will panic on POST, which is a wiring bug worth
// surfacing loudly -- production wires the C3 measurer in.
func NewHandler(store *Store, quota QuotaProvider, tel *Telemetry, cfg HandlerConfig) *Handler {
	if cfg.TmpExpiry == 0 {
		cfg.TmpExpiry = defaultTmpExpiry
	}
	return &Handler{store: store, quota: quota, tel: tel, cfg: cfg}
}

// Mount is a placeholder for C2c. It will wire OPTIONS/POST/HEAD/PATCH/
// DELETE/GET handlers onto mux and wrap each route with the supplied
// middleware (auth from Wave A). For C2a it is intentionally a no-op so
// the package compiles and the handler can be exercised directly via
// httptest in tests; production wire-up waits for C2c.
//
//nolint:unused // intentional placeholder for the C2c sub-task; the
// signature is fixed by the plan §Task C2c so future work touches only
// the body.
func (h *Handler) Mount(mux *http.ServeMux) {
	// C2c fills in the routing table. See plan §Task C2c for the
	// route list and the middleware threading pattern.
	_ = mux
}

// checkTusResumable validates the Tus-Resumable header on every method
// except OPTIONS (tus.io exempts OPTIONS per the spec). On mismatch the
// helper writes 412 Precondition Failed with the canonical Tus-Version
// echo header and returns false; callers MUST stop processing on a
// false return.
func (h *Handler) checkTusResumable(w http.ResponseWriter, r *http.Request) bool {
	if v := r.Header.Get("Tus-Resumable"); v != tusVersion {
		w.Header().Set("Tus-Version", tusVersion)
		w.WriteHeader(http.StatusPreconditionFailed)
		return false
	}
	return true
}

// options implements OPTIONS /v1/archives -- capability discovery per
// spec §4.1. Returns 200 with Tus-Version, Tus-Max-Size, Tus-Extension,
// and Tus-Resumable. OPTIONS is exempt from the Tus-Resumable header
// check (tus.io spec).
func (h *Handler) options(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Version", tusVersion)
	w.Header().Set("Tus-Max-Size", strconv.FormatInt(h.cfg.TusMaxSize, 10))
	w.Header().Set("Tus-Extension", tusExtensions)
	w.Header().Set("Tus-Resumable", tusVersion)
	w.WriteHeader(http.StatusOK)
	_ = r
}

// create implements POST /v1/archives -- upload initiation per spec
// §4.1, §4.3 (idempotency), §4.5 (media allow-list), §5.4 (caps), and
// §3.2 (Tus-Max-Size). The check order is load-bearing; see the inline
// comments at each step.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	// Step 1: Tus-Resumable. Non-OPTIONS verbs require the header.
	if !h.checkTusResumable(w, r) {
		return
	}

	// Step 2: Upload-Length. Missing/non-numeric/negative -> 400.
	uploadLengthHeader := r.Header.Get("Upload-Length")
	if uploadLengthHeader == "" {
		writeErrorJSON(w, http.StatusBadRequest, "upload_length_missing")
		return
	}
	uploadLength, err := strconv.ParseInt(uploadLengthHeader, 10, 64)
	if err != nil || uploadLength < 0 {
		writeErrorJSON(w, http.StatusBadRequest, "upload_length_invalid")
		return
	}

	// Step 3: Tus-Max-Size enforcement (spec §3.2). The protocol-level
	// cap fires BEFORE any metadata parsing so a client with an absurd
	// Upload-Length learns of the failure quickly.
	if h.cfg.TusMaxSize > 0 && uploadLength > h.cfg.TusMaxSize {
		writeErrorJSON(w, http.StatusRequestEntityTooLarge, "upload_length_exceeds_max")
		return
	}

	// Step 4: Upload-Metadata header presence. The header itself is
	// required even before we parse it; absence is a 400 with a distinct
	// code so log dashboards can tell "no header" apart from "header
	// invalid".
	metadataHeader := r.Header.Get("Upload-Metadata")
	if metadataHeader == "" {
		writeErrorJSON(w, http.StatusBadRequest, "upload_metadata_missing")
		return
	}

	// Step 5: delivered_by from request context. The auth middleware
	// (Wave A) is responsible for setting this; reaching the handler
	// without it means the route was mounted without auth, which is a
	// production wiring bug worth surfacing as 500. In production the
	// auth middleware would have already returned 401 to an
	// unauthenticated caller before the request reached here.
	sourceID, ok := ingest.DeliveredBy(r.Context())
	if !ok {
		slog.Error("archive POST reached handler without delivered_by in context; auth middleware not mounted",
			"path", r.URL.Path)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_wiring")
		return
	}

	// Step 6: parse + validate metadata. ParseUploadMetadata returns a
	// fully-validated *Metadata or one of the sentinel errors defined in
	// metadata.go. We translate each sentinel to a single opaque code;
	// the response body never echoes which field failed (security
	// discipline per spec §4.3 / §4.5).
	meta, perr := ParseUploadMetadata(metadataHeader, uploadLength)
	if perr != nil {
		status, code := mapMetadataError(perr)
		writeErrorJSON(w, status, code)
		return
	}

	// Step 7: pre-flight idempotency (spec §4.3). Scoped by source-id:
	// the lookup is (source_id, archive_id) -> staged metadata.json.
	idemStatus, idemBody := h.preflightIdempotency(sourceID, meta)
	switch idemStatus {
	case http.StatusSeeOther:
		// Same source-id + matching sha256 -> 303 to the existing
		// finalized archive. No upload state created. Body empty.
		w.Header().Set("Location", archivesBasePath+"/"+meta.ArchiveID)
		w.Header().Set("Tus-Resumable", tusVersion)
		w.WriteHeader(http.StatusSeeOther)
		return
	case http.StatusConflict:
		// Different source-id OR different sha256. Body is opaque:
		// {"error":"archive_id_conflict"} -- no echo of the existing
		// sha256 or source-id.
		writeErrorJSON(w, http.StatusConflict, "archive_id_conflict")
		return
	case 0:
		// No collision; proceed to upload-state allocation.
	default:
		// Internal error from the idempotency lookup (filesystem read
		// failure on metadata.json). Body has already been built.
		writeErrorJSON(w, idemStatus, idemBody)
		return
	}

	// Step 8: quota hard cap (spec §5.4). 503 with Retry-After: 600. The
	// quota provider's Lift threshold (85%) is its own concern; we just
	// ask "should we block?". A nil provider here would panic; production
	// wires the C3 measurer in.
	if h.quota != nil && h.quota.ShouldBlock() {
		w.Header().Set("Retry-After", "600")
		writeErrorJSON(w, http.StatusServiceUnavailable, "storage_hard_cap")
		return
	}

	// Step 9: allocate upload state via Store.Create. The store checks
	// global cap first, then per-source cap, then generates the upload-id.
	st, cerr := h.store.Create(sourceID, meta, uploadLength)
	if cerr != nil {
		switch {
		case errors.Is(cerr, ErrConcurrencyGlobal):
			h.tel.RecordConcurrentRejected(r.Context(), sourceID, "global")
			w.Header().Set("Retry-After", "60")
			writeErrorJSON(w, http.StatusTooManyRequests, "concurrent_uploads_global")
		case errors.Is(cerr, ErrConcurrencyPerSource):
			h.tel.RecordConcurrentRejected(r.Context(), sourceID, "per_source")
			w.Header().Set("Retry-After", "60")
			writeErrorJSON(w, http.StatusTooManyRequests, "concurrent_uploads_per_source")
		default:
			// Likely a crypto/rand failure or an upload-id collision;
			// both are internal-class events.
			slog.Error("archive POST: store.Create failed",
				"source_id", sourceID,
				"archive_id", meta.ArchiveID,
				"err", cerr)
			writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		}
		return
	}

	// Step 10: create the tmp file. PATCH (in C2b) reopens it for
	// append; we just stake the upload-id's claim on the path so
	// concurrent CREATEs for the same id (which shouldn't happen given
	// 128-bit randomness, but the O_EXCL flag is belt-and-suspenders)
	// fail loudly.
	tmpDir := filepath.Join(h.cfg.StagingRoot, ".tmp-archives")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		h.store.Remove(st.ID)
		slog.Error("archive POST: mkdir .tmp-archives failed",
			"source_id", sourceID,
			"archive_id", meta.ArchiveID,
			"err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}
	tmpPath := filepath.Join(tmpDir, st.ID)
	f, ferr := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if ferr != nil {
		h.store.Remove(st.ID)
		slog.Error("archive POST: create tmp file failed",
			"source_id", sourceID,
			"archive_id", meta.ArchiveID,
			"upload_id", st.ID,
			"err", ferr)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}
	if cerr := f.Close(); cerr != nil {
		// Closing the just-opened empty file failed; tear down state +
		// surface 500. The PATCH handler in C2b reopens this file in
		// append mode, so we don't need to keep it open here.
		_ = os.Remove(tmpPath)
		h.store.Remove(st.ID)
		slog.Error("archive POST: close tmp file failed",
			"source_id", sourceID,
			"upload_id", st.ID,
			"err", cerr)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}

	// Step 11: telemetry. RecordUploadCreated + RecordUploadInFlight(+1).
	// Both are nil-safe so tests that don't wire telemetry pass a nil
	// Telemetry. Failure paths use RecordUploadFailed; C2b/c exercise
	// that path more.
	ctx := r.Context()
	h.tel.RecordUploadCreated(ctx, sourceID, meta.MediaType)
	h.tel.RecordUploadInFlight(ctx, sourceID, +1)

	// Step 12: log the spec §7.3 "upload created" event.
	slog.Info(EventUploadCreated,
		"source_id", sourceID,
		"archive_id", meta.ArchiveID,
		"media_type", meta.MediaType,
		"declared_size_bytes", uploadLength,
		"declared_sha256", meta.SHA256)

	// Step 13: 201 + Location + Tus-Resumable + Tus-Expires. The
	// Tus-Expires header carries the cleanup-eligibility deadline per
	// spec §5.5 so clients know when to resume by.
	expires := time.Now().UTC().Add(h.cfg.TmpExpiry).Format(http.TimeFormat)
	w.Header().Set("Location", archivesBasePath+"/"+st.ID)
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Tus-Expires", expires)
	w.WriteHeader(http.StatusCreated)
}

// preflightIdempotency implements the three-branch lookup of spec §4.3:
//
//	(absent)                                         -> proceed (status 0)
//	(present, source matches, sha256 matches)        -> 303 See Other
//	(present, source different OR sha256 different)  -> 409 Conflict
//
// Returns the HTTP status to use (0 means "no collision, keep going")
// and an error-code string for the 409 body. The 303 / 409 bodies are
// not constructed here; the caller assembles them so it can set the
// Location header in the 303 case.
//
// A read error on a present metadata.json is treated as 500 with code
// "internal_idempotency". We deliberately do NOT fall through to
// "proceed" on a read failure: if archives/<id>/metadata.json exists
// but is unreadable, allowing the upload would risk overwriting a
// finalized archive on the next finalize-rename collision check, which
// fails with ErrArchiveExists. 500 here lets the operator investigate.
func (h *Handler) preflightIdempotency(sourceID string, meta *Metadata) (int, string) {
	metaPath := filepath.Join(h.cfg.StagingRoot, "archives", meta.ArchiveID, "metadata.json")
	fi, err := os.Stat(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "" // proceed
		}
		slog.Error("archive POST: stat existing metadata.json failed",
			"archive_id", meta.ArchiveID,
			"err", err)
		return http.StatusInternalServerError, "internal_idempotency"
	}
	if fi.IsDir() {
		// metadata.json is a directory? Filesystem corruption; treat
		// as internal error rather than guessing whether to proceed.
		slog.Error("archive POST: metadata.json is a directory",
			"archive_id", meta.ArchiveID)
		return http.StatusInternalServerError, "internal_idempotency"
	}

	data, err := os.ReadFile(metaPath)
	if err != nil {
		slog.Error("archive POST: read existing metadata.json failed",
			"archive_id", meta.ArchiveID,
			"err", err)
		return http.StatusInternalServerError, "internal_idempotency"
	}
	var existing FinalizeReceipt
	if err := json.Unmarshal(data, &existing); err != nil {
		slog.Error("archive POST: unmarshal existing metadata.json failed",
			"archive_id", meta.ArchiveID,
			"err", err)
		return http.StatusInternalServerError, "internal_idempotency"
	}

	// Same source-id AND matching sha256 -> 303 fast-path. Anything
	// else -> 409 conflict. We do NOT echo the existing sha256 or
	// source-id; the response body is the opaque code only (spec §4.3).
	if existing.DeliveredBy == sourceID && existing.SHA256 == meta.SHA256 {
		return http.StatusSeeOther, ""
	}
	return http.StatusConflict, "archive_id_conflict"
}

// mapMetadataError maps a sentinel from ParseUploadMetadata to an HTTP
// status + opaque error code. Per spec §4.2, ErrMetadataTooLong is the
// 431 case (header overflow); all others are 400 with a single
// "metadata_invalid" code so the body never reveals which field failed.
func mapMetadataError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrMetadataTooLong):
		return http.StatusRequestHeaderFieldsTooLarge, "metadata_too_long"
	case errors.Is(err, ErrMetadataMissing),
		errors.Is(err, ErrMetadataInvalid),
		errors.Is(err, ErrMetadataReservedKey),
		errors.Is(err, ErrMetadataUnknownMediaType),
		errors.Is(err, ErrMetadataSizeMismatch):
		return http.StatusBadRequest, "metadata_invalid"
	default:
		return http.StatusBadRequest, "metadata_invalid"
	}
}

// writeErrorJSON writes a single-field JSON error body. The body shape
// is always {"error":"<code>"} -- NO details, hints, fields, or echoes
// of any caller-supplied value. Spec §4.3 / §4.5 require this opacity.
//
// The function tolerates a (rare) Encode failure by falling back to a
// hand-built body so the response is never empty on the wire.
func writeErrorJSON(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": code}); err != nil {
		// json.NewEncoder on a ResponseWriter does not normally fail;
		// the fallback exists so a body is always present on the wire.
		_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
	}
}

