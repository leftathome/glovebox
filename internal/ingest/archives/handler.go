// Package archives -- tus.io HTTP handler (spec 13 §4).
//
// This file owns the HTTP surface that speaks the tus.io v1.0.0 resumable
// upload protocol over /v1/archives*. It is implemented across three
// sub-tasks per the plan:
//
//   - C2a: OPTIONS + POST + pre-flight idempotency.
//     Establishes Handler / HandlerConfig / NewHandler, the
//     Tus-Resumable version-check helper, and a Mount placeholder.
//   - C2b: HEAD + PATCH + DELETE (per-upload-id mutex,
//     rolling sha256, finalize trigger, idle timeout).
//   - C2c (this commit): GET receipt + Mount wiring with the auth
//     middleware from Wave A.
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
	"context"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/source"
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

// defaultPatchIdleTimeout is the §4.4 PATCH idle-timeout default (5 min).
// HandlerConfig.PatchIdleTimeout overrides; NewHandler fills this in
// when zero so callers don't have to know the spec constant.
const defaultPatchIdleTimeout = 5 * time.Minute

// patchContentType is the tus.io required Content-Type for PATCH bodies
// per spec §4.4 / the tus.io core protocol. Any other value -> 415.
const patchContentType = "application/offset+octet-stream"

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

	// PatchIdleTimeout bounds the wall-clock duration of a single PATCH
	// body read (spec §4.4 idle-timeout). When the body stalls for this
	// duration the server terminates the request with 408. Default 5 min
	// (defaultPatchIdleTimeout); NewHandler fills in the default when
	// zero. Tests override with shorter values.
	PatchIdleTimeout time.Duration

	// Sources is the connector-lane registry (glovebox-9s60), threaded into
	// FinalizeConfig so a registered recognizer-scanner source gets
	// operator-lane treatment and the recognizer-scan media type is gated
	// (fail-closed) to it. Nil disables the lane (and rejects the scanner
	// media type). Loaded at boot from config.SourcesFile by the listener.
	Sources *source.Registry

	// ExtractScanner scans the scanner lane's extracted text before it is
	// published. Threaded into FinalizeConfig; see writeExtractedMarkdown.
	ExtractScanner ExtractScanner
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
// helper is nil-safe). quota may be nil — the POST handler skips the
// hard-cap check via an explicit nil guard at the call site. Production
// wires the C3 storage measurer in; tests that don't care about quota
// behavior pass nil to avoid building a fake QuotaProvider just to be
// asked ShouldBlock() once.
func NewHandler(store *Store, quota QuotaProvider, tel *Telemetry, cfg HandlerConfig) *Handler {
	if cfg.TmpExpiry == 0 {
		cfg.TmpExpiry = defaultTmpExpiry
	}
	if cfg.PatchIdleTimeout == 0 {
		cfg.PatchIdleTimeout = defaultPatchIdleTimeout
	}
	return &Handler{store: store, quota: quota, tel: tel, cfg: cfg}
}

// Middleware is the standard http.Handler-chaining shape used by Mount.
// Wave A's auth wiring (added in C3) produces a value of this type that
// combines the token store, rate limiter, and proxy resolver. Defining
// it here keeps the archives package decoupled from auth at the type
// level — Mount accepts a slice of middlewares and is agnostic to what
// any given one does, so tests can pass fakes that just record their
// call order.
type Middleware func(http.Handler) http.Handler

// Mount registers the handler's HTTP routes on mux with the supplied
// middleware. The middleware is applied OUTERMOST -> INNERMOST: caller
// passes [auth] and gets a route that does auth then dispatches to the
// archive handler; caller passes [auth, ratelimit] and gets
// auth(ratelimit(handler)) -- auth runs first, then ratelimit, then
// the archive method.
//
// Routes (spec §4.1):
//
//	OPTIONS /v1/archives                  -> h.options
//	POST    /v1/archives                  -> h.create
//	HEAD    /v1/archives/<upload-id>      -> h.head
//	PATCH   /v1/archives/<upload-id>      -> h.patch
//	DELETE  /v1/archives/<upload-id>      -> h.delete
//	GET     /v1/archives/<archive_id>     -> h.get
//
// Method-not-allowed responses (e.g., PUT, POST against the per-id
// path) get 405 + an Allow header listing the supported methods per
// RFC 7231.
//
// The trailing-slash pattern (/v1/archives/) is required by net/http's
// ServeMux to match all subpaths.
func (h *Handler) Mount(mux *http.ServeMux, middleware ...Middleware) {
	apply := func(handler http.Handler) http.Handler {
		for i := len(middleware) - 1; i >= 0; i-- {
			handler = middleware[i](handler)
		}
		return handler
	}

	basePath := archivesBasePath

	// /v1/archives -- OPTIONS + POST
	mux.Handle(basePath, apply(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			h.options(w, r)
		case http.MethodPost:
			h.create(w, r)
		default:
			w.Header().Set("Allow", "OPTIONS, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})))

	// /v1/archives/<id> -- HEAD + PATCH + DELETE + GET
	mux.Handle(basePath+"/", apply(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			h.head(w, r)
		case http.MethodPatch:
			h.patch(w, r)
		case http.MethodDelete:
			h.delete(w, r)
		case http.MethodGet:
			h.get(w, r)
		default:
			w.Header().Set("Allow", "HEAD, PATCH, DELETE, GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})))
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
// mapMetadataError translates a Metadata sentinel into its HTTP status +
// closed-enum error code. The codes are distinct per sentinel so
// operator dashboards can categorize 4xx volume by root cause.
//
// Opacity rule (spec §4.3 / §4.5): the code is a server-known label
// that NEVER echoes any caller-supplied value (the client can craft
// metadata to inject strings into the response otherwise). The response
// body remains the single-field {"error":"<code>"} shape; only the code
// string varies. This preserves the §4.3 idempotency-leak prevention
// (the 409 conflict still uses the opaque `archive_id_conflict`) while
// restoring per-error-class dashboarding signal flagged by the C2a
// spec reviewer (spec §4.5 line 206).
func mapMetadataError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrMetadataTooLong):
		return http.StatusRequestHeaderFieldsTooLarge, "metadata_too_long"
	case errors.Is(err, ErrMetadataMissing):
		return http.StatusBadRequest, "metadata_missing"
	case errors.Is(err, ErrMetadataReservedKey):
		return http.StatusBadRequest, "metadata_reserved_key"
	case errors.Is(err, ErrMetadataUnknownMediaType):
		return http.StatusBadRequest, "unknown_media_type"
	case errors.Is(err, ErrMetadataSizeMismatch):
		return http.StatusBadRequest, "size_mismatch"
	case errors.Is(err, ErrMetadataInvalid):
		return http.StatusBadRequest, "metadata_invalid"
	default:
		// Unknown sentinel — default to the umbrella code so a future
		// added sentinel doesn't silently leak the raw error.
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

// extractUploadID returns the upload-id segment of a /v1/archives/<id>
// path. Returns the empty string if the path doesn't have a non-empty
// trailing segment; the caller MUST treat that as "no upload-id" and
// 404. We deliberately do NOT validate the format (32 hex chars) here:
// a malformed id can't match anything in the Store, so Store.Get's
// 404 path is the canonical reject and avoids a parallel format check
// that could drift from the upload-id generator.
func extractUploadID(path string) string {
	// archivesBasePath is "/v1/archives"; subpath is everything after.
	rest := strings.TrimPrefix(path, archivesBasePath)
	rest = strings.TrimPrefix(rest, "/")
	// Take the first path segment only. The endpoint surface is flat
	// (no nested IDs), so any '/' in the remainder is an unexpected
	// subpath; we treat the leading segment as the id and let Store.Get
	// 404 on anything that doesn't match.
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// requireDeliveredBy pulls the source-id from the request context and
// writes a 500 wiring-bug response if missing. Returns (sourceID, ok);
// callers MUST stop processing on a false return. The 500 surface is
// the same as POST: reaching a HEAD/PATCH/DELETE handler without auth
// means the route was mounted without the middleware, which is a
// production wiring bug worth surfacing rather than silently 401'ing
// (the middleware would have 401'd ahead of us anyway).
func (h *Handler) requireDeliveredBy(w http.ResponseWriter, r *http.Request, method string) (string, bool) {
	sid, ok := ingest.DeliveredBy(r.Context())
	if !ok {
		slog.Error("archive "+method+" reached handler without delivered_by in context; auth middleware not mounted",
			"path", r.URL.Path)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_wiring")
		return "", false
	}
	return sid, true
}

// head implements HEAD /v1/archives/<upload-id> -- offset query per
// spec §4.1. Returns 200 + four headers (Upload-Offset, Upload-Length,
// Tus-Resumable, Tus-Expires). The body is empty per the HTTP spec
// for HEAD responses.
//
// Cross-source binding failures return 404 (same as missing upload-id)
// per spec §4.1: an unauthorized source-id cannot distinguish "doesn't
// exist" from "exists, owned by someone else".
func (h *Handler) head(w http.ResponseWriter, r *http.Request) {
	if !h.checkTusResumable(w, r) {
		return
	}
	sourceID, ok := h.requireDeliveredBy(w, r, "HEAD")
	if !ok {
		return
	}
	uploadID := extractUploadID(r.URL.Path)
	if uploadID == "" {
		writeErrorJSON(w, http.StatusNotFound, "upload_not_found")
		return
	}

	st, err := h.store.Get(uploadID, sourceID)
	if err != nil {
		// ErrUploadNotFound covers both "missing id" and "wrong source-id"
		// per the Store contract (spec §4.1 no-leak rule). Any other error
		// from Get is unexpected; surface as 500 only after logging.
		if errors.Is(err, ErrUploadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, "upload_not_found")
			return
		}
		slog.Error("archive HEAD: store.Get failed",
			"upload_id", uploadID,
			"source_id", sourceID,
			"err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}

	// Snapshot the offset / length / created-at under the upload's mutex
	// so a concurrent PATCH cannot interleave its write into our read.
	// We acquire (not TryLock) on HEAD: it is fine for a read-only HEAD
	// to wait briefly for an in-flight PATCH to release; the spec §4.4
	// "do NOT block waiting" rule applies only to PATCH-vs-PATCH.
	st.Mu.Lock()
	offset := st.Offset
	length := st.UploadLength
	createdAt := st.CreatedAt
	st.Mu.Unlock()

	expires := createdAt.Add(h.cfg.TmpExpiry).Format(http.TimeFormat)
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Tus-Expires", expires)
	// Cache-Control: tus.io recommends no-cache on HEAD so intermediaries
	// don't serve a stale offset to a resuming client; this is cheap
	// insurance even on a local cluster.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// patchCounter wraps the in-flight PATCH body write into a single
// io.Writer that fans the bytes out to the tmp file, the rolling
// sha256 hasher, AND a counter the handler reads after the copy
// completes (or fails). Centralizing the three writes keeps the
// "bytes-that-landed" accounting consistent across success and failure.
type patchCounter struct {
	n int64
}

// Write implements io.Writer. The counter never errs; the upstream
// MultiWriter handles per-sink errors.
func (c *patchCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// patchBodyReader wraps the request body with an idle-timeout context.
// On each Read it races the underlying read against ctx.Done; if the
// context fires first the read returns ctx.Err() so io.Copy unwinds
// without further blocking. This is the spec §4.4 idle-timeout teeth.
//
// Implementation note: net/http's Request.Body honors context cancellation
// in modern Go, so a simpler approach is to set a deadline on the
// underlying conn. We can't do that directly here (httptest doesn't
// expose deadlines on the recorder path), so we use this explicit
// race so the test surface is straightforward AND production behavior
// is identical regardless of transport.
type patchBodyReader struct {
	ctx context.Context
	src io.Reader
}

// Read returns ctx.Err() (typically context.DeadlineExceeded) as soon
// as ctx fires; otherwise it blocks on src.Read like a vanilla reader.
// We do NOT spin a goroutine to drain src in the background -- io.Copy
// is the only caller and on cancellation it propagates the error up;
// the caller then closes r.Body which terminates the in-flight Read.
func (p *patchBodyReader) Read(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := p.src.Read(b)
		ch <- result{n: n, err: err}
	}()
	select {
	case <-p.ctx.Done():
		return 0, p.ctx.Err()
	case r := <-ch:
		return r.n, r.err
	}
}

// patch implements PATCH /v1/archives/<upload-id> -- append chunk per
// spec §4.4. Validates headers + binding + non-blocking mutex + offset,
// streams body into the tmp file with idle-timeout enforcement, updates
// rolling sha256 + offset, and triggers Finalize when offset reaches
// UploadLength.
//
// Per spec §4.4, concurrent PATCHes against the same upload-id return
// 409 upload_busy IMMEDIATELY (TryLock, do NOT block). HEAD/DELETE are
// blocking on the same mutex; this asymmetry is intentional.
//
// PATCH validation ordering (§4.4 steps 1-8); collapsing further would
// obscure the spec mapping.
//
//nolint:gocyclo // The handler's branching is dictated by the spec's
func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	if !h.checkTusResumable(w, r) {
		return
	}
	sourceID, ok := h.requireDeliveredBy(w, r, "PATCH")
	if !ok {
		return
	}
	uploadID := extractUploadID(r.URL.Path)
	if uploadID == "" {
		writeErrorJSON(w, http.StatusNotFound, "upload_not_found")
		return
	}

	st, err := h.store.Get(uploadID, sourceID)
	if err != nil {
		if errors.Is(err, ErrUploadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, "upload_not_found")
			return
		}
		slog.Error("archive PATCH: store.Get failed",
			"upload_id", uploadID,
			"source_id", sourceID,
			"err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}

	// Step: Content-Type check (spec §4.4). 415 on mismatch. Done before
	// Upload-Offset parsing so a wrong-CT body is rejected without
	// touching the mutex.
	if ct := r.Header.Get("Content-Type"); ct != patchContentType {
		writeErrorJSON(w, http.StatusUnsupportedMediaType, "wrong_content_type")
		return
	}

	// Step: parse Upload-Offset.
	offsetHeader := r.Header.Get("Upload-Offset")
	if offsetHeader == "" {
		writeErrorJSON(w, http.StatusBadRequest, "upload_offset_missing")
		return
	}
	clientOffset, perr := strconv.ParseInt(offsetHeader, 10, 64)
	if perr != nil || clientOffset < 0 {
		writeErrorJSON(w, http.StatusBadRequest, "upload_offset_invalid")
		return
	}

	// Step: NON-BLOCKING per-upload mutex (spec §4.4 step 2). The
	// explicit TryLock + immediate 409 is the spec's "do NOT block
	// waiting" rule -- concurrent PATCHes from the same client (two TCP
	// sessions, a buggy retry loop) get the busy signal immediately
	// rather than serializing through the mutex.
	if !st.Mu.TryLock() {
		writeErrorJSON(w, http.StatusConflict, "upload_busy")
		return
	}
	defer st.Mu.Unlock()

	// Step: offset agreement. THIS IS THE ONE DELIBERATE EXCEPTION to
	// the opaque-body rule: the 409 body echoes the server's expected
	// offset because (a) the value is server-known and not derived from
	// caller input, and (b) the spec §4.4 step 3 explicitly authorizes
	// the echo so clients can self-correct without an extra HEAD.
	if clientOffset != st.Offset {
		writeOffsetMismatch(w, st.Offset)
		return
	}

	// Step: stream the body with the idle-timeout context. The wrapper
	// reader returns ctx.Err() when the timeout fires so io.Copy
	// terminates promptly. Bytes that landed before the timeout are
	// persisted (st.Offset advances by counter.n regardless of outcome).
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.PatchIdleTimeout)
	defer cancel()

	tmpPath := filepath.Join(h.cfg.StagingRoot, ".tmp-archives", uploadID)
	f, ferr := os.OpenFile(tmpPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if ferr != nil {
		slog.Error("archive PATCH: open tmp file failed",
			"upload_id", uploadID,
			"source_id", sourceID,
			"err", ferr)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}
	defer func() { _ = f.Close() }()

	// Cap writes at the remaining declared length so overshoot is tolerated
	// silently (spec §4.4 line 193: "the server discards the surplus, sets
	// the offset to Upload-Length, and proceeds to finalize"). The
	// LimitReader caps the actual bytes that land in f/Hasher/counter at
	// Upload-Length; we drain any surplus from r.Body so the client's send
	// completes cleanly. If the client sends fewer bytes than the cap,
	// io.Copy returns when r.Body EOFs first and we still finalize correctly.
	counter := &patchCounter{}
	body := &patchBodyReader{ctx: ctx, src: r.Body}
	mw := io.MultiWriter(f, st.Hasher, counter)
	remaining := st.UploadLength - st.Offset
	if remaining < 0 {
		remaining = 0
	}
	_, copyErr := io.Copy(mw, io.LimitReader(body, remaining))
	if copyErr == nil {
		// Drain any surplus the client sent; we don't write it to disk but
		// we DO need to consume it so the client's POST/PATCH completes
		// rather than seeing a broken-pipe error.
		_, _ = io.Copy(io.Discard, body)
	}

	// Whatever made it onto disk is persisted, regardless of copyErr.
	st.Offset += counter.n
	st.LastActivity = time.Now().UTC()

	if copyErr != nil {
		// Distinguish idle-timeout from a generic body-read error so we
		// can surface 408 (idle) vs 400 (truncated/dropped). The body
		// reader returns ctx.Err() on timeout; we inspect the wrapping
		// context to decide.
		if errors.Is(copyErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.tel.RecordPatchIdleTimeout(r.Context(), sourceID)
			slog.Warn(EventUploadAborted,
				"upload_id", uploadID,
				"source_id", sourceID,
				"archive_id", st.ArchiveID,
				"reason", "patch_idle_timeout",
				"bytes_persisted", counter.n)
			writeErrorJSON(w, http.StatusRequestTimeout, "patch_idle_timeout")
			return
		}
		// Client closed the connection or transport error. The upload-id
		// remains valid; the client may resume via HEAD + a new PATCH.
		// We log at WARN with the bytes that landed so operators can
		// see how far the client got.
		slog.Warn(EventUploadAborted,
			"upload_id", uploadID,
			"source_id", sourceID,
			"archive_id", st.ArchiveID,
			"reason", string(AbortTerminatedByClient),
			"bytes_persisted", counter.n,
			"err", copyErr)
		writeErrorJSON(w, http.StatusBadRequest, "patch_body_truncated")
		return
	}

	// Step: telemetry for the successful chunk.
	h.tel.RecordPatchChunkBytes(r.Context(), counter.n)
	h.tel.RecordUploadBytes(r.Context(), sourceID, st.Meta.MediaType, counter.n)

	// Step: finalize when offset reaches UploadLength (spec §4.6). The
	// finalize runs WHILE STILL HOLDING the upload mutex per §4.4 step
	// 7 so a concurrent DELETE cannot race the rename.
	if st.Offset >= st.UploadLength {
		// Close the tmp file BEFORE Finalize: on Linux, finalize re-opens
		// the path read-only for untar OR renames it for raw-file
		// delivery, and holding a write-mode fd at the same time risks
		// data loss on certain filesystems. The deferred Close above
		// would still run after Finalize but is then a no-op on a closed
		// fd.
		_ = f.Close()
		h.finalizeAndRespond(w, r, st, sourceID, uploadID)
		return
	}

	// Step: partial PATCH success -- return 204 + new Upload-Offset.
	w.Header().Set("Upload-Offset", strconv.FormatInt(st.Offset, 10))
	w.Header().Set("Tus-Resumable", tusVersion)
	w.WriteHeader(http.StatusNoContent)
}

// finalizeAndRespond runs Finalize for an upload whose offset just
// reached UploadLength and writes the appropriate HTTP response.
// Extracted from patch() so the spec §4.6 result mapping lives in one
// place and the cyclomatic complexity of patch() stays in check.
//
// On every terminal outcome (success OR failure) this method removes
// the in-memory upload state via Store.Remove and adjusts the
// in-flight gauge. Tmp + finalize dir cleanup is owned by Finalize
// itself on failure paths and by Finalize's rename on success.
func (h *Handler) finalizeAndRespond(w http.ResponseWriter, r *http.Request, st *UploadState, sourceID, uploadID string) {
	// Re-read r.Context() (the un-wrapped request context) intentionally:
	// PATCH wraps the body-read context with PatchIdleTimeout, but that
	// deadline is for THE PATCH BODY read, not for finalize. Using the
	// raw r.Context() means a fired PATCH-idle timer doesn't cancel
	// finalize's sha256 + untar + rename work.
	ctx := r.Context()
	mediaType := st.Meta.MediaType
	duration := time.Since(st.CreatedAt)

	_, err := Finalize(ctx, st, FinalizeConfig{
		StagingRoot:    h.cfg.StagingRoot,
		Sources:        h.cfg.Sources,
		ExtractScanner: h.cfg.ExtractScanner,
	})
	if err != nil {
		status, code := h.recordFinalizeFailure(ctx, st, sourceID, uploadID, mediaType, err)
		// Decrement in-flight gauge on every terminal failure path.
		h.store.Remove(uploadID)
		h.tel.RecordUploadInFlight(ctx, sourceID, -1)
		writeErrorJSON(w, status, code)
		return
	}

	// Success: drop in-memory state, record telemetry, log, respond.
	h.store.Remove(uploadID)
	h.tel.RecordUploadCompleted(ctx, sourceID, mediaType, duration)
	h.tel.RecordUploadInFlight(ctx, sourceID, -1)
	slog.Info(EventUploadCompleted,
		"upload_id", uploadID,
		"source_id", sourceID,
		"archive_id", st.ArchiveID,
		"media_type", mediaType,
		"size_bytes", st.UploadLength,
		"duration_ms", duration.Milliseconds())
	w.Header().Set("Upload-Offset", strconv.FormatInt(st.Offset, 10))
	w.Header().Set("Tus-Resumable", tusVersion)
	w.WriteHeader(http.StatusNoContent)
}

// recordFinalizeFailure maps a Finalize error to its telemetry counter
// increment + structured log entry, AND returns the HTTP (status, code)
// the caller should write. Separating "record" from "write" lets the
// caller (finalizeAndRespond) own the http.ResponseWriter — earlier
// drafts tried to thread the writer through this function via context
// and ended up with a dead httpResponseWriterFromCtx stub that panicked.
//
// The mapping table mirrors spec §4.6 / §4.3:
//
//	ErrSHAMismatch   -> 400 sha256_mismatch       status=failed_sha256
//	ErrSizeMismatch  -> 400 size_mismatch         status=failed_size
//	ErrArchiveExists -> 409 archive_id_conflict   status=failed_idempotency
//	tar sentinel     -> 400 tar_unsafe_entry      status=failed_untar
//	(other)          -> 500 internal_finalize     status=failed_internal
//
// The opaque body shape (single {"error":"<code>"} field) is preserved
// even though Finalize knows the claimed-vs-computed sha pair; spec
// §4.3 / §4.5 forbid echoing computed values to the client.
func (h *Handler) recordFinalizeFailure(ctx context.Context, st *UploadState, sourceID, uploadID, mediaType string, err error) (status int, code string) {
	logBase := []any{
		"upload_id", uploadID,
		"source_id", sourceID,
		"archive_id", st.ArchiveID,
		"media_type", mediaType,
	}

	switch {
	case errors.Is(err, ErrSHAMismatch):
		h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_sha256")
		slog.Warn(EventUploadAborted, append(logBase, "reason", string(AbortSHA256Mismatch))...)
		return http.StatusBadRequest, "sha256_mismatch"
	case errors.Is(err, ErrSizeMismatch):
		h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_size")
		slog.Warn(EventUploadAborted, append(logBase, "reason", string(AbortSizeMismatch))...)
		return http.StatusBadRequest, "size_mismatch"
	case errors.Is(err, ErrArchiveExists):
		h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_idempotency")
		slog.Warn(EventUploadAborted, append(logBase, "reason", string(AbortIdempotencyConflict))...)
		return http.StatusConflict, "archive_id_conflict"
	case errors.Is(err, ErrSourceNotAuthorized):
		// recognizer-scan lane gate (glovebox-9s60): the media type was used
		// by a source not registered as the recognizer-scanner kind.
		h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_authz")
		slog.Warn(EventUploadAborted, append(logBase, "reason", "source_not_authorized")...)
		return http.StatusForbidden, "source_not_authorized"
	default:
		// Tar-safety sentinels are %w-wrapped onto a top-level "untar:"
		// error by Finalize; ClassifyReason returns ok=true for any
		// recognized sentinel anywhere in the chain.
		if reason, classified := ClassifyReason(err); classified {
			h.tel.RecordTarSafetyReject(ctx, sourceID, reason)
			h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_untar")
			slog.Warn(EventTarSafetyReject, append(logBase, "reason", reason)...)
			return http.StatusBadRequest, "tar_unsafe_entry"
		}
		h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_internal")
		slog.Error("archive PATCH: finalize internal error", append(logBase, "err", err)...)
		return http.StatusInternalServerError, "internal_finalize"
	}
}

// writeOffsetMismatch writes the spec §4.4 409 with the {"error","expected"}
// body. This is the ONE exception to the opaque-body rule -- documented
// inline at the call site in patch().
func writeOffsetMismatch(w http.ResponseWriter, expected int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	body := map[string]any{
		"error":    "offset_mismatch",
		"expected": expected,
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Fallback hand-built body so the response is never empty.
		_, _ = w.Write([]byte(`{"error":"offset_mismatch","expected":` + strconv.FormatInt(expected, 10) + `}`))
	}
}

// _ guards the rolling-hash type from accidental nil checks in code
// completion; the field is required and non-nil after Store.Create.
var _ hash.Hash = (*nopHash)(nil)

// nopHash satisfies hash.Hash for the rare case where a test wires an
// UploadState directly without going through Store.Create. Real callers
// always have a sha256 hasher (Store.Create constructs sha256.New()),
// but the type fence here documents the contract.
type nopHash struct{}

func (nopHash) Write(p []byte) (int, error) { return len(p), nil }
func (nopHash) Sum(b []byte) []byte         { return b }
func (nopHash) Reset()                      {}
func (nopHash) Size() int                   { return 0 }
func (nopHash) BlockSize() int              { return 1 }

// delete implements DELETE /v1/archives/<upload-id> -- spec §4.1 abort
// + spec §6 termination extension. Returns 204 + removes the tmp file,
// the finalize dir (if any), and the in-memory state.
//
// Cross-source binding failures return 404 (NOT 403) per spec §4.1.
//
// DELETE acquires the upload mutex (blocking, not TryLock) so a
// DELETE arriving mid-PATCH waits for the PATCH to release. The spec
// doesn't specify the busy-vs-blocking behavior for DELETE; we choose
// blocking because the client's intent ("forget this upload") is
// unambiguous and a brief wait is harmless.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if !h.checkTusResumable(w, r) {
		return
	}
	sourceID, ok := h.requireDeliveredBy(w, r, "DELETE")
	if !ok {
		return
	}
	uploadID := extractUploadID(r.URL.Path)
	if uploadID == "" {
		writeErrorJSON(w, http.StatusNotFound, "upload_not_found")
		return
	}

	st, err := h.store.Get(uploadID, sourceID)
	if err != nil {
		if errors.Is(err, ErrUploadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, "upload_not_found")
			return
		}
		slog.Error("archive DELETE: store.Get failed",
			"upload_id", uploadID,
			"source_id", sourceID,
			"err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}

	// Acquire the upload mutex (blocking). A concurrent PATCH will
	// finish (or fail) before we proceed; the cleanup we do here is
	// independent of any bytes the PATCH may have just written.
	// store.Remove runs INSIDE the locked section so a concurrent HEAD
	// can't observe the store entry after the tmp file is gone (TOCTOU
	// closure flagged by C2b spec review).
	st.Mu.Lock()
	mediaType := st.Meta.MediaType
	archiveID := st.ArchiveID
	// Cleanup tmp file + finalize dir. Both are best-effort: a missing
	// path on either is not an error (idempotent cleanup semantics).
	cleanupTmp(h.cfg.StagingRoot, uploadID)
	h.store.Remove(uploadID)
	st.Mu.Unlock()

	h.tel.RecordUploadFailed(r.Context(), sourceID, mediaType, "terminated")
	h.tel.RecordUploadInFlight(r.Context(), sourceID, -1)
	slog.Info(EventUploadAborted,
		"upload_id", uploadID,
		"source_id", sourceID,
		"archive_id", archiveID,
		"media_type", mediaType,
		"reason", string(AbortTerminatedByClient))

	w.Header().Set("Tus-Resumable", tusVersion)
	w.WriteHeader(http.StatusNoContent)
}

// get implements GET /v1/archives/<archive_id> -- finalize-receipt
// fetch per spec §4.1 / §4.8. Unlike the other methods this is a
// normal HTTP receipt fetch, NOT a tus.io protocol verb, so there is
// no Tus-Resumable header check (spec §4.1's method table omits the
// requirement for GET).
//
// Behavior:
//
//   - No delivered_by in context -> 500 (wiring bug).
//   - Malformed archive_id (fails the §4.2 regex) -> 404 with the same
//     opaque body as "not found". Returning 400 would leak the shape
//     of valid archive_ids; we treat malformed identically to absent.
//   - archives/<id>/metadata.json missing -> 404 opaque.
//   - metadata.json read error -> 500.
//   - metadata.json parse error -> 500 (on-disk corruption is internal).
//   - DeliveredBy mismatch between the staged receipt and the request's
//     authenticated source-id -> 404 opaque (no cross-source existence
//     leak; same body as "not found").
//   - Happy path -> 200 + JSON receipt. We re-encode the FinalizeReceipt
//     rather than streaming the file bytes so the wire shape is governed
//     by the Go type, even if a future maintainer adds an on-disk-only
//     field. The Content-Type is application/json.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	sourceID, ok := h.requireDeliveredBy(w, r, "GET")
	if !ok {
		return
	}

	archiveID := extractUploadID(r.URL.Path)
	// Validate the archive_id matches the same regex used at POST. A
	// malformed id can't exist in archives/, so we'd 404 below anyway,
	// but doing the regex check up front lets us avoid touching the
	// filesystem with attacker-controlled path segments. The 404 (not
	// 400) is deliberate: it makes "malformed" and "missing" indistinguishable
	// on the wire, so a probing client learns nothing about valid-id shape.
	if !archiveIDRe.MatchString(archiveID) {
		writeErrorJSON(w, http.StatusNotFound, "archive_not_found")
		return
	}

	metaPath := filepath.Join(h.cfg.StagingRoot, "archives", archiveID, "metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Same opaque body as the source-mismatch case below.
			writeErrorJSON(w, http.StatusNotFound, "archive_not_found")
			return
		}
		slog.Error("archive GET: read metadata.json failed",
			"archive_id", archiveID,
			"source_id", sourceID,
			"err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}

	var receipt FinalizeReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		slog.Error("archive GET: unmarshal metadata.json failed",
			"archive_id", archiveID,
			"source_id", sourceID,
			"err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "internal_state")
		return
	}

	// Source-id binding: same opaque 404 as "not found" so a caller can't
	// distinguish "doesn't exist" from "exists, owned by another source".
	if receipt.DeliveredBy != sourceID {
		writeErrorJSON(w, http.StatusNotFound, "archive_not_found")
		return
	}

	// Re-encode the receipt rather than echoing data: we control the wire
	// shape from the Go type. Future on-disk-only fields don't accidentally
	// leak.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(receipt); err != nil {
		// At this point the status has been written; we can only log.
		slog.Error("archive GET: encode receipt response failed",
			"archive_id", archiveID,
			"source_id", sourceID,
			"err", err)
	}
}
