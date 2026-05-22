// Tests for the tus.io HTTP handler (spec 13 §4) -- C2a coverage:
// OPTIONS + POST + pre-flight idempotency.
//
// Coverage matrix:
//
//   TestOPTIONS_ReturnsCapabilities          -- 200 + four advertised headers
//   TestPOST_NoTusResumable_412              -- missing Tus-Resumable rejected
//   TestPOST_NoAuth_500_WiringBug            -- ctx without delivered_by -> 500
//   TestPOST_MissingUploadLength_400         -- header absent -> 400
//   TestPOST_NonNumericUploadLength_400      -- header garbage -> 400
//   TestPOST_UploadLengthExceedsMax_413      -- spec §3.2 Tus-Max-Size
//   TestPOST_MissingUploadMetadata_400       -- header absent -> 400
//   TestPOST_ReservedMetadataKey_DeliveredBy_400
//   TestPOST_ReservedMetadataKey_DeliveredAt_400
//   TestPOST_SizeBytesMismatch_400
//   TestPOST_UnknownMediaType_400
//   TestPOST_OversizedMetadataHeader_431     -- header > 4 KiB
//   TestPOST_HappyPath_201                   -- full success surface
//   TestPOST_Idempotency_303OnMatch          -- spec §4.3 fast-path
//   TestPOST_Idempotency_409OnSHADifference  -- spec §4.3 conflict, opaque body
//   TestPOST_Idempotency_409OnSourceIDDifference -- spec §4.3 cross-source
//   TestPOST_QuotaHardCap_503                -- spec §5.4 503 + Retry-After 600
//   TestPOST_PerSourceConcurrentCap_429      -- spec §5.4 429 + Retry-After 60
//   TestPOST_GlobalConcurrentCap_429         -- spec §5.4 429 + Retry-After 60

package archives

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/ingest"
)

// ---------------------------------------------------------------------
// Fakes and helpers (local to handler_test.go per the plan -- not added
// to helpers_test.go, which is B2's domain).
// ---------------------------------------------------------------------

// fakeQuota lets tests force the QuotaProvider.ShouldBlock return
// without needing the real C3 measurer.
type fakeQuota struct{ block bool }

func (f *fakeQuota) ShouldBlock() bool { return f.block }

// newTestHandler constructs a Handler wired with a t.TempDir() staging
// root, the given Store + QuotaProvider, and real Telemetry. Returns
// the handler plus the staging root so tests can pre-create staged
// archives for idempotency cases.
func newTestHandler(t *testing.T, store *Store, quota QuotaProvider, tusMax int64) (*Handler, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir .tmp-archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}
	tel, err := NewTelemetry("test", "test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	h := NewHandler(store, quota, tel, HandlerConfig{
		StagingRoot: root,
		TusMaxSize:  tusMax,
		TmpExpiry:   72 * time.Hour,
	})
	return h, root
}

// defaultStore returns a Store with the spec §5.4 default caps so most
// tests don't have to spell them out.
func defaultStore() *Store {
	return NewStore(StoreConfig{
		PerSourceMaxConcurrent: 4,
		GlobalMaxConcurrent:    32,
	})
}

// encMeta encodes a tus.io Upload-Metadata header from a key/value map.
// Iteration order is deterministic-ish per Go map semantics; tests rely
// only on the presence of keys, not their order.
func encMeta(pairs map[string]string) string {
	var parts []string
	for k, v := range pairs {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

// validMetaPairs returns a fully-valid Upload-Metadata pair map keyed
// for an archive/mbox raw delivery whose declared size matches contentLen.
func validMetaPairs(archiveID string, contentLen int64) map[string]string {
	return map[string]string{
		"archive_id":            archiveID,
		"archive_filename":      "test.mbox",
		"subtree_relative_path": ".",
		"media_type":            "archive/mbox",
		"matcher_id":            "test/matcher",
		"provider":              "test",
		"sha256":                "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"size_bytes":            strconv.FormatInt(contentLen, 10),
	}
}

// authedPostRequest builds a POST /v1/archives request whose context
// carries delivered_by=sourceID, with the supplied headers applied.
func authedPostRequest(t *testing.T, sourceID string, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, archivesBasePath, nil)
	if sourceID != "" {
		req = req.WithContext(ingest.WithDeliveredBy(req.Context(), sourceID))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// stageExistingArchive writes a fake metadata.json at archives/<id>/
// so the pre-flight idempotency check finds something. Returns the
// receipt that was written, for the caller to assert against.
func stageExistingArchive(t *testing.T, root, archiveID, deliveredBy, sha256Hex string) FinalizeReceipt {
	t.Helper()
	dir := filepath.Join(root, "archives", archiveID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}
	r := FinalizeReceipt{
		ArchiveID:      archiveID,
		ReceivedAt:     time.Now().UTC(),
		DeliveredBy:    deliveredBy,
		MediaType:      "archive/mbox",
		SizeBytes:      1024,
		SHA256:         sha256Hex,
		SHA256Verified: true,
		StagedPath:     filepath.Join("archives", archiveID),
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	metaPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	return r
}

// decodeErrorBody reads the JSON error body and returns the "error"
// code field. Empty string on parse failure.
func decodeErrorBody(t *testing.T, body io.Reader) string {
	t.Helper()
	var obj map[string]any
	if err := json.NewDecoder(body).Decode(&obj); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	s, _ := obj["error"].(string)
	return s
}

// ---------------------------------------------------------------------
// OPTIONS
// ---------------------------------------------------------------------

func TestOPTIONS_ReturnsCapabilities(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 32212254720)
	req := httptest.NewRequest(http.MethodOptions, archivesBasePath, nil)
	w := httptest.NewRecorder()
	h.options(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Tus-Version"); got != "1.0.0" {
		t.Errorf("Tus-Version = %q, want 1.0.0", got)
	}
	if got := w.Header().Get("Tus-Max-Size"); got != "32212254720" {
		t.Errorf("Tus-Max-Size = %q, want 32212254720", got)
	}
	if got := w.Header().Get("Tus-Extension"); got != "creation,termination,checksum" {
		t.Errorf("Tus-Extension = %q, want creation,termination,checksum", got)
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Errorf("Tus-Resumable = %q, want 1.0.0", got)
	}
}

// ---------------------------------------------------------------------
// POST — header preconditions
// ---------------------------------------------------------------------

func TestPOST_NoTusResumable_412(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	req := authedPostRequest(t, "recognizer", map[string]string{
		// Intentionally NO Tus-Resumable header.
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", w.Code)
	}
	if got := w.Header().Get("Tus-Version"); got != "1.0.0" {
		t.Errorf("Tus-Version echo = %q, want 1.0.0", got)
	}
}

func TestPOST_NoAuth_500_WiringBug(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	// Build a request WITHOUT WithDeliveredBy in the context.
	req := httptest.NewRequest(http.MethodPost, archivesBasePath, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "1024")
	req.Header.Set("Upload-Metadata", encMeta(validMetaPairs("a1", 1024)))

	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestPOST_MissingUploadLength_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable": "1.0.0",
		// Upload-Length intentionally absent.
		"Upload-Metadata": encMeta(validMetaPairs("a1", 1024)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPOST_NonNumericUploadLength_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "not-a-number",
		"Upload-Metadata": encMeta(validMetaPairs("a1", 1024)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPOST_UploadLengthExceedsMax_413(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1024)
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "2048",
		"Upload-Metadata": encMeta(validMetaPairs("a1", 2048)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

func TestPOST_MissingUploadMetadata_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Length": "1024",
		// Upload-Metadata intentionally absent.
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------
// POST — metadata validation
// ---------------------------------------------------------------------

func TestPOST_ReservedMetadataKey_DeliveredBy_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	pairs := validMetaPairs("a1", 1024)
	pairs["delivered_by"] = "attacker"
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	code := decodeErrorBody(t, w.Body)
	if code == "" || strings.Contains(code, "delivered_by") {
		t.Errorf("body code = %q; expected opaque code without echoing field name", code)
	}
}

func TestPOST_ReservedMetadataKey_DeliveredAt_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	pairs := validMetaPairs("a1", 1024)
	pairs["delivered_at"] = "2026-01-01T00:00:00Z"
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPOST_SizeBytesMismatch_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	pairs := validMetaPairs("a1", 1024)
	pairs["size_bytes"] = "9999" // != Upload-Length=1024
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPOST_UnknownMediaType_400(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	pairs := validMetaPairs("a1", 1024)
	pairs["media_type"] = "archive/unknown"
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPOST_OversizedMetadataHeader_431(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	// Build a base64-encoded value large enough that the assembled
	// Upload-Metadata header exceeds 4 KiB.
	big := strings.Repeat("A", 4096)
	pairs := validMetaPairs("a1", 1024)
	pairs["matcher_id"] = big
	header := encMeta(pairs)
	if len(header) <= uploadMetadataMaxBytes {
		t.Fatalf("test fixture too small: header len %d <= cap %d", len(header), uploadMetadataMaxBytes)
	}
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": header,
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", w.Code)
	}
}

// ---------------------------------------------------------------------
// POST — happy path
// ---------------------------------------------------------------------

func TestPOST_HappyPath_201(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(validMetaPairs("a1-test", 1024)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", w.Code, w.Body.String())
	}

	// Location: /v1/archives/<32-hex>
	loc := w.Header().Get("Location")
	locRe := regexp.MustCompile(`^/v1/archives/[0-9a-f]{32}$`)
	if !locRe.MatchString(loc) {
		t.Errorf("Location = %q does not match %v", loc, locRe)
	}

	// Tus-Resumable echo
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Errorf("Tus-Resumable = %q, want 1.0.0", got)
	}

	// Tus-Expires header parses as RFC1123 and is ~72h in the future.
	expires := w.Header().Get("Tus-Expires")
	if expires == "" {
		t.Fatal("Tus-Expires header missing")
	}
	tExpires, err := http.ParseTime(expires)
	if err != nil {
		t.Fatalf("Tus-Expires unparseable: %v", err)
	}
	want := time.Now().UTC().Add(72 * time.Hour)
	delta := tExpires.Sub(want)
	if delta < -time.Minute || delta > time.Minute {
		t.Errorf("Tus-Expires = %v, want ~ %v (delta %v)", tExpires, want, delta)
	}

	// Tmp file exists on disk under the upload-id.
	uploadID := strings.TrimPrefix(loc, "/v1/archives/")
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	if fi, err := os.Stat(tmpPath); err != nil {
		t.Errorf("expected tmp file at %s: %v", tmpPath, err)
	} else if fi.Size() != 0 {
		t.Errorf("tmp file size = %d, want 0 (PATCH appends later)", fi.Size())
	}

	// Store has the upload state.
	st, err := store.Get(uploadID, "recognizer")
	if err != nil {
		t.Errorf("store.Get(%q, recognizer) failed: %v", uploadID, err)
	}
	if st != nil && st.UploadLength != 1024 {
		t.Errorf("UploadLength = %d, want 1024", st.UploadLength)
	}
}

// ---------------------------------------------------------------------
// POST — idempotency (spec §4.3)
// ---------------------------------------------------------------------

func TestPOST_Idempotency_303OnMatch(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Pre-create a matching staged archive.
	archiveID := "matched-archive"
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_ = stageExistingArchive(t, root, archiveID, "recognizer", sha)

	pairs := validMetaPairs(archiveID, 1024)
	pairs["sha256"] = sha
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body = %s", w.Code, w.Body.String())
	}
	wantLoc := archivesBasePath + "/" + archiveID
	if got := w.Header().Get("Location"); got != wantLoc {
		t.Errorf("Location = %q, want %q", got, wantLoc)
	}
	// Body should be empty per the spec §4.3 303 contract.
	if w.Body.Len() != 0 {
		t.Errorf("body len = %d, want 0", w.Body.Len())
	}
	// NO upload state should have been created.
	if len(store.uploads) != 0 {
		t.Errorf("store has %d uploads after 303; want 0", len(store.uploads))
	}
}

func TestPOST_Idempotency_409OnSHADifference(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	archiveID := "conflict-archive-sha"
	existingSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	_ = stageExistingArchive(t, root, archiveID, "recognizer", existingSHA)

	// POST with the same archive_id but a DIFFERENT sha256.
	pairs := validMetaPairs(archiveID, 1024)
	pairs["sha256"] = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "archive_id_conflict") {
		t.Errorf("body = %q, want to contain archive_id_conflict", body)
	}
	// CRITICAL: body MUST NOT echo the existing sha256.
	if strings.Contains(body, existingSHA) {
		t.Errorf("body leaks existing sha256: %s", body)
	}
}

func TestPOST_Idempotency_409OnSourceIDDifference(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	archiveID := "conflict-archive-source"
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// Existing archive owned by "alice".
	_ = stageExistingArchive(t, root, archiveID, "alice", sha)

	// POST from "recognizer" with the SAME sha256.
	pairs := validMetaPairs(archiveID, 1024)
	pairs["sha256"] = sha
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// CRITICAL: body MUST NOT echo the existing source-id.
	if strings.Contains(body, "alice") {
		t.Errorf("body leaks existing source_id: %s", body)
	}
}

// ---------------------------------------------------------------------
// POST — quota and concurrency caps
// ---------------------------------------------------------------------

func TestPOST_QuotaHardCap_503(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{block: true}, 1<<30)
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(validMetaPairs("a1", 1024)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After = %q, want 600", got)
	}
	if code := decodeErrorBody(t, w.Body); code != "storage_hard_cap" {
		t.Errorf("body code = %q, want storage_hard_cap", code)
	}
}

func TestPOST_PerSourceConcurrentCap_429(t *testing.T) {
	// Store with per-source cap of 1.
	store := NewStore(StoreConfig{
		PerSourceMaxConcurrent: 1,
		GlobalMaxConcurrent:    32,
	})
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Pre-allocate one upload for "recognizer" so the next POST trips
	// the per-source cap.
	if _, err := store.Create("recognizer", &Metadata{ArchiveID: "x"}, 1024); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(validMetaPairs("a1", 1024)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if code := decodeErrorBody(t, w.Body); code != "concurrent_uploads_per_source" {
		t.Errorf("body code = %q, want concurrent_uploads_per_source", code)
	}
}

func TestPOST_GlobalConcurrentCap_429(t *testing.T) {
	// Store with global cap of 1.
	store := NewStore(StoreConfig{
		PerSourceMaxConcurrent: 32,
		GlobalMaxConcurrent:    1,
	})
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Pre-allocate one upload (from a different source-id so per-source
	// isn't the limiting factor) so the next POST trips the global cap.
	if _, err := store.Create("alice", &Metadata{ArchiveID: "x"}, 1024); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "1024",
		"Upload-Metadata": encMeta(validMetaPairs("a1", 1024)),
	})
	w := httptest.NewRecorder()
	h.create(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if code := decodeErrorBody(t, w.Body); code != "concurrent_uploads_global" {
		t.Errorf("body code = %q, want concurrent_uploads_global", code)
	}
}

// ---------------------------------------------------------------------
// C2b helpers
// ---------------------------------------------------------------------

// patchURL builds the canonical PATCH/HEAD/DELETE URL for an upload-id.
func patchURL(uploadID string) string {
	return archivesBasePath + "/" + uploadID
}

// createUpload POSTs an upload through the handler and returns the
// upload-id along with the sha256 (hex) the metadata claims. The caller
// supplies the body bytes the upload will eventually contain; this
// helper hashes them so the claimed sha256 in the metadata header
// matches what PATCH will actually deliver. The upload-id is extracted
// from the Location header.
func createUpload(t *testing.T, h *Handler, sourceID, archiveID string, body []byte) (uploadID, claimedSHA string) {
	t.Helper()
	sum := sha256.Sum256(body)
	claimedSHA = hex.EncodeToString(sum[:])
	pairs := validMetaPairs(archiveID, int64(len(body)))
	pairs["sha256"] = claimedSHA
	req := authedPostRequest(t, sourceID, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   strconv.FormatInt(int64(len(body)), 10),
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("createUpload: status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	uploadID = strings.TrimPrefix(loc, archivesBasePath+"/")
	if uploadID == "" {
		t.Fatalf("createUpload: empty upload-id from Location %q", loc)
	}
	return uploadID, claimedSHA
}

// newPatchRequest constructs an authenticated PATCH request for the
// given upload-id with the given offset and body. Sets the canonical
// Content-Type and Tus-Resumable headers unless overridden by the
// caller (which uses an explicit override via NewRequest + Header.Set).
func newPatchRequest(t *testing.T, sourceID, uploadID string, offset int64, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, patchURL(uploadID), body)
	req = req.WithContext(ingest.WithDeliveredBy(req.Context(), sourceID))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", patchContentType)
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	return req
}

// newHeadRequest constructs an authenticated HEAD request for the given
// upload-id.
func newHeadRequest(t *testing.T, sourceID, uploadID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodHead, patchURL(uploadID), nil)
	req = req.WithContext(ingest.WithDeliveredBy(req.Context(), sourceID))
	req.Header.Set("Tus-Resumable", "1.0.0")
	return req
}

// newDeleteRequest constructs an authenticated DELETE request for the
// given upload-id.
func newDeleteRequest(t *testing.T, sourceID, uploadID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, patchURL(uploadID), nil)
	req = req.WithContext(ingest.WithDeliveredBy(req.Context(), sourceID))
	req.Header.Set("Tus-Resumable", "1.0.0")
	return req
}

// blockingReader is an io.Reader that yields its bytes only after
// release is closed. Used by TestPATCH_Concurrent_409UploadBusy to
// keep one PATCH goroutine deterministically mid-copy while a second
// PATCH races in and trips the per-upload TryLock.
type blockingReader struct {
	data    []byte
	release <-chan struct{}
	started chan struct{} // closed on the first Read so the test knows we're inside io.Copy
	once    sync.Once
}

// Read implements io.Reader. The first call signals "started" and
// blocks until release is closed; subsequent calls drain the buffer
// to io.EOF.
func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// slowReader feeds bytes from data but sleeps `delay` before each Read,
// so a PATCH idle-timeout that is shorter than `delay` will fire before
// io.Copy completes. Used by TestPATCH_IdleTimeout_408.
type slowReader struct {
	data  []byte
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil
}

// ---------------------------------------------------------------------
// HEAD
// ---------------------------------------------------------------------

func TestHEAD_UnknownUploadID_404(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	req := newHeadRequest(t, "recognizer", "00000000000000000000000000000000")
	w := httptest.NewRecorder()
	h.head(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "upload_not_found" {
		t.Errorf("body code = %q, want upload_not_found", code)
	}
}

func TestHEAD_WrongSourceID_404(t *testing.T) {
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Seed an upload owned by "alice".
	body := []byte("hello-world")
	uploadID, _ := createUpload(t, h, "alice", "wrong-source-archive", body)

	// HEAD as "recognizer" -- must look identical to the unknown-id case.
	req := newHeadRequest(t, "recognizer", uploadID)
	w := httptest.NewRecorder()
	h.head(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "upload_not_found" {
		t.Errorf("body code = %q, want upload_not_found (no leak)", code)
	}
	// No headers should reveal upload state.
	if got := w.Header().Get("Upload-Offset"); got != "" {
		t.Errorf("Upload-Offset header leaked: %q", got)
	}
	if got := w.Header().Get("Upload-Length"); got != "" {
		t.Errorf("Upload-Length header leaked: %q", got)
	}
}

func TestHEAD_HappyPath_200WithHeaders(t *testing.T) {
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	body := []byte("hello-world-some-bytes")
	uploadID, _ := createUpload(t, h, "recognizer", "head-happy", body)

	req := newHeadRequest(t, "recognizer", uploadID)
	w := httptest.NewRecorder()
	h.head(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Upload-Offset"); got != "0" {
		t.Errorf("Upload-Offset = %q, want 0 (fresh upload)", got)
	}
	wantLen := strconv.Itoa(len(body))
	if got := w.Header().Get("Upload-Length"); got != wantLen {
		t.Errorf("Upload-Length = %q, want %q", got, wantLen)
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Errorf("Tus-Resumable = %q, want 1.0.0", got)
	}
	if exp := w.Header().Get("Tus-Expires"); exp == "" {
		t.Errorf("Tus-Expires header missing")
	} else if _, err := http.ParseTime(exp); err != nil {
		t.Errorf("Tus-Expires unparseable: %v", err)
	}
}

// ---------------------------------------------------------------------
// PATCH
// ---------------------------------------------------------------------

func TestPATCH_WrongContentType_415(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	body := []byte("payload")
	uploadID, _ := createUpload(t, h, "recognizer", "patch-wrong-ct", body)

	req := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream") // wrong
	w := httptest.NewRecorder()
	h.patch(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "wrong_content_type" {
		t.Errorf("body code = %q, want wrong_content_type", code)
	}
}

func TestPATCH_OffsetMismatch_409(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	body := []byte("payload-bytes")
	uploadID, _ := createUpload(t, h, "recognizer", "patch-offset-mismatch", body)

	// Server expects offset=0 on a fresh upload; submit offset=5.
	req := newPatchRequest(t, "recognizer", uploadID, 5, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.patch(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	// Spec §4.4: the 409 body is {"error":"offset_mismatch","expected":N}.
	// The `expected` echo is the ONE allowed exception to the opaque-body
	// rule. Assert both fields are present and the expected value is 0.
	var obj map[string]any
	if err := json.NewDecoder(w.Body).Decode(&obj); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if code, _ := obj["error"].(string); code != "offset_mismatch" {
		t.Errorf("error = %q, want offset_mismatch", code)
	}
	// JSON numbers decode to float64.
	exp, ok := obj["expected"].(float64)
	if !ok {
		t.Fatalf("expected field missing or wrong type: %T %v", obj["expected"], obj["expected"])
	}
	if int64(exp) != 0 {
		t.Errorf("expected = %v, want 0 (server's current offset on fresh upload)", exp)
	}
}

func TestPATCH_WrongSourceID_404(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	body := []byte("payload")
	uploadID, _ := createUpload(t, h, "alice", "patch-wrong-source", body)

	// PATCH as a different source-id; must 404 with the same code as the
	// unknown-id case (no leak).
	req := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.patch(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "upload_not_found" {
		t.Errorf("body code = %q, want upload_not_found (no leak)", code)
	}
}

func TestPATCH_Concurrent_409UploadBusy(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)

	// Use a payload large enough that the first PATCH spans the body
	// completes. The first goroutine's blockingReader holds the mutex
	// until we release it; the second PATCH must observe the busy state
	// and 409 immediately (TryLock).
	body := []byte("hello-concurrent-patch")
	uploadID, _ := createUpload(t, h, "recognizer", "patch-busy", body)

	release := make(chan struct{})
	started := make(chan struct{})
	br := &blockingReader{
		data:    body,
		release: release,
		started: started,
	}

	// Goroutine A: PATCH with a body that blocks until release.
	reqA := newPatchRequest(t, "recognizer", uploadID, 0, br)
	wA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		h.patch(wA, reqA)
		close(doneA)
	}()

	// Wait until A's io.Copy has invoked the first Read so we know A
	// is holding the upload mutex.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		<-doneA
		t.Fatal("goroutine A never reached its first body Read")
	}

	// Goroutine B: a second PATCH against the SAME upload-id while A
	// holds the mutex. TryLock must fail -> 409 upload_busy.
	reqB := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(body))
	wB := httptest.NewRecorder()
	h.patch(wB, reqB)

	if wB.Code != http.StatusConflict {
		close(release)
		<-doneA
		t.Fatalf("B status = %d, want 409", wB.Code)
	}
	if code := decodeErrorBody(t, wB.Body); code != "upload_busy" {
		close(release)
		<-doneA
		t.Errorf("B body code = %q, want upload_busy", code)
	}

	// Now let A finish so the test exits cleanly. A's outcome doesn't
	// affect the assertion; we only require that exactly one of A/B saw
	// upload_busy (B).
	close(release)
	<-doneA
}

func TestPATCH_HappyPath_AppendBytes(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Two-part upload so the first PATCH is a partial (offset advances
	// but offset < UploadLength); finalize is exercised separately.
	body := []byte("the-quick-brown-fox-jumps-over-the-lazy-dog")
	uploadID, _ := createUpload(t, h, "recognizer", "patch-happy", body)

	// PATCH a prefix only.
	prefix := body[:10]
	req := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(prefix))
	w := httptest.NewRecorder()
	h.patch(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", w.Code, w.Body.String())
	}
	wantOffset := strconv.Itoa(len(prefix))
	if got := w.Header().Get("Upload-Offset"); got != wantOffset {
		t.Errorf("Upload-Offset = %q, want %q", got, wantOffset)
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Errorf("Tus-Resumable = %q, want 1.0.0", got)
	}

	// Verify the tmp file actually contains the bytes that were appended.
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read tmp file: %v", err)
	}
	if !bytes.Equal(got, prefix) {
		t.Errorf("tmp file contents = %q, want %q", got, prefix)
	}

	// Store state should reflect the new offset and the upload should
	// still be in-flight (not removed).
	st, gerr := store.Get(uploadID, "recognizer")
	if gerr != nil {
		t.Fatalf("store.Get after partial PATCH: %v", gerr)
	}
	if st.Offset != int64(len(prefix)) {
		t.Errorf("st.Offset = %d, want %d", st.Offset, len(prefix))
	}
}

func TestPATCH_CompletesUpload_TriggersFinalize(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Single-shot PATCH covering the whole upload length so finalize runs.
	body := []byte("complete-upload-body-bytes-for-finalize")
	uploadID, claimedSHA := createUpload(t, h, "recognizer", "finalize-happy", body)

	// Resolve the archive_id the staged metadata used so we can verify
	// the post-rename location.
	st, gerr := store.Get(uploadID, "recognizer")
	if gerr != nil {
		t.Fatalf("store.Get: %v", gerr)
	}
	archiveID := st.ArchiveID

	req := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.patch(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", w.Code, w.Body.String())
	}
	wantOffset := strconv.Itoa(len(body))
	if got := w.Header().Get("Upload-Offset"); got != wantOffset {
		t.Errorf("Upload-Offset = %q, want %q", got, wantOffset)
	}

	// Store should have dropped the upload state after a successful finalize.
	if _, err := store.Get(uploadID, "recognizer"); err == nil {
		t.Errorf("expected store.Get to fail after finalize; state still present")
	}

	// archives/<archive_id>/metadata.json should exist with the claimed sha.
	metaPath := filepath.Join(root, "archives", archiveID, "metadata.json")
	data, rerr := os.ReadFile(metaPath)
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}
	var receipt FinalizeReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatalf("unmarshal receipt: %v", err)
	}
	if receipt.SHA256 != claimedSHA {
		t.Errorf("receipt.SHA256 = %q, want %q", receipt.SHA256, claimedSHA)
	}
	if !receipt.SHA256Verified {
		t.Errorf("receipt.SHA256Verified = false, want true")
	}

	// The tmp file should be gone (renamed into raw/).
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("tmp file still present after finalize: %v", err)
	}
}

func TestPATCH_FinalizeSHA256Mismatch_400(t *testing.T) {
	// Defense against a recurring class of bug: the spec §4.6 contract is
	// that a sha256 mismatch at finalize returns 400 with
	// {"error":"sha256_mismatch"} (opaque -- no echo of claimed/computed).
	// We engineer the mismatch by writing the upload state with a CLAIMED
	// sha256 that does not match the body bytes we will PATCH.
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	body := []byte("payload-that-will-mismatch")
	// Claim a sha256 that's definitely NOT the body's actual hash.
	wrongSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	pairs := validMetaPairs("sha-mismatch", int64(len(body)))
	pairs["sha256"] = wrongSHA
	req := authedPostRequest(t, "recognizer", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   strconv.Itoa(len(body)),
		"Upload-Metadata": encMeta(pairs),
	})
	w := httptest.NewRecorder()
	h.create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
	uploadID := strings.TrimPrefix(w.Header().Get("Location"), archivesBasePath+"/")

	// PATCH the full body. Server will reach finalize -> sha256 compare
	// fails -> 400 + sha256_mismatch.
	pReq := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(body))
	pW := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PATCH panicked on sha256 mismatch path: %v", r)
		}
	}()
	h.patch(pW, pReq)

	if pW.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on sha256 mismatch (got body %q)", pW.Code, pW.Body.String())
	}
	code := decodeErrorBody(t, pW.Body)
	if code != "sha256_mismatch" {
		t.Errorf("body code = %q, want sha256_mismatch", code)
	}
	// Body MUST NOT echo the claimed or computed sha256 (opacity rule).
	bodyStr := pW.Body.String()
	if strings.Contains(bodyStr, wrongSHA) {
		t.Errorf("body leaks claimed sha256: %s", bodyStr)
	}
}

func TestPATCH_FinalizeRenameCollision_409(t *testing.T) {
	// Spec §4.3 / §4.6: if archives/<archive_id>/ already exists at
	// finalize time, the rename fails (Finalize returns ErrArchiveExists)
	// and the handler MUST respond with 409 + archive_id_conflict.
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	body := []byte("collision-body")
	archiveID := "finalize-collision"
	uploadID, _ := createUpload(t, h, "recognizer", archiveID, body)

	// Pre-create archives/<archive_id>/ with a sentinel file so the final
	// rename collides. The handler's idempotency check is at POST time,
	// not at PATCH; creating the directory after createUpload bypasses
	// the POST-time pre-flight and forces the collision into the finalize
	// rename step.
	collideDir := filepath.Join(root, "archives", archiveID)
	if err := os.MkdirAll(collideDir, 0o700); err != nil {
		t.Fatalf("mkdir collision dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(collideDir, "sentinel"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	req := newPatchRequest(t, "recognizer", uploadID, 0, bytes.NewReader(body))
	w := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PATCH panicked on rename-collision path: %v", r)
		}
	}()
	h.patch(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 on rename collision (got body %q)", w.Code, w.Body.String())
	}
	if code := decodeErrorBody(t, w.Body); code != "archive_id_conflict" {
		t.Errorf("body code = %q, want archive_id_conflict", code)
	}
}

func TestPATCH_IdleTimeout_408(t *testing.T) {
	// Construct a handler with a SHORT PATCH idle timeout. The slowReader
	// sleeps longer than the timeout before delivering its first byte,
	// which forces the patchBodyReader's ctx.Done branch to fire.
	store := defaultStore()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir .tmp-archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}
	tel, err := NewTelemetry("test", "test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	h := NewHandler(store, &fakeQuota{}, tel, HandlerConfig{
		StagingRoot:      root,
		TusMaxSize:       1 << 30,
		TmpExpiry:        72 * time.Hour,
		PatchIdleTimeout: 100 * time.Millisecond,
	})

	body := []byte("slow-payload-that-will-time-out")
	uploadID, _ := createUpload(t, h, "recognizer", "patch-idle", body)

	// 500ms sleep before the first Read; timeout is 100ms.
	slow := &slowReader{data: body, delay: 500 * time.Millisecond}
	req := newPatchRequest(t, "recognizer", uploadID, 0, slow)
	w := httptest.NewRecorder()
	h.patch(w, req)

	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "patch_idle_timeout" {
		t.Errorf("body code = %q, want patch_idle_timeout", code)
	}
}

// ---------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------

func TestDELETE_HappyPath_204(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	body := []byte("delete-body")
	uploadID, _ := createUpload(t, h, "recognizer", "delete-happy", body)

	// Confirm the tmp file exists before DELETE.
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("pre-DELETE: tmp file missing: %v", err)
	}

	req := newDeleteRequest(t, "recognizer", uploadID)
	w := httptest.NewRecorder()
	h.delete(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Errorf("Tus-Resumable = %q, want 1.0.0", got)
	}

	// Tmp file must be gone.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("tmp file still present after DELETE: %v", err)
	}
	// Store must have dropped the upload.
	if _, err := store.Get(uploadID, "recognizer"); err == nil {
		t.Errorf("expected store.Get to fail after DELETE; state still present")
	}
}

func TestDELETE_WrongSourceID_404(t *testing.T) {
	store := defaultStore()
	h, root := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	body := []byte("delete-wrong-source")
	uploadID, _ := createUpload(t, h, "alice", "delete-wrong-source", body)

	// DELETE as a different source-id; same 404 as unknown-id (no leak).
	req := newDeleteRequest(t, "recognizer", uploadID)
	w := httptest.NewRecorder()
	h.delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "upload_not_found" {
		t.Errorf("body code = %q, want upload_not_found (no leak)", code)
	}

	// Tmp file must NOT have been removed (the wrong-source caller has
	// no business cleaning up alice's upload).
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("tmp file removed by wrong-source DELETE: %v", err)
	}
	// alice's upload state should still be present.
	if _, err := store.Get(uploadID, "alice"); err != nil {
		t.Errorf("alice's upload state was dropped by wrong-source DELETE: %v", err)
	}
}

// ---------------------------------------------------------------------
// GET — finalize receipt fetch (spec §4.1 / §4.8)
// ---------------------------------------------------------------------

// newGetRequest constructs an authenticated GET request for the given
// archive-id. The Tus-Resumable header is intentionally NOT set: GET
// is a normal HTTP receipt fetch per spec §4.1's method table.
func newGetRequest(t *testing.T, sourceID, archiveID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, archivesBasePath+"/"+archiveID, nil)
	if sourceID != "" {
		req = req.WithContext(ingest.WithDeliveredBy(req.Context(), sourceID))
	}
	return req
}

func TestGET_FinalizedArchive_200(t *testing.T) {
	h, root := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)

	archiveID := "abcdef12-takeout-001"
	staged := stageExistingArchive(t, root, archiveID, "recognizer",
		"deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef")

	req := newGetRequest(t, "recognizer", archiveID)
	w := httptest.NewRecorder()
	h.get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var got FinalizeReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode receipt: %v; body = %s", err, w.Body.String())
	}
	if got.ArchiveID != staged.ArchiveID {
		t.Errorf("ArchiveID = %q, want %q", got.ArchiveID, staged.ArchiveID)
	}
	if got.DeliveredBy != staged.DeliveredBy {
		t.Errorf("DeliveredBy = %q, want %q", got.DeliveredBy, staged.DeliveredBy)
	}
	if got.SHA256 != staged.SHA256 {
		t.Errorf("SHA256 = %q, want %q", got.SHA256, staged.SHA256)
	}
	if got.MediaType != staged.MediaType {
		t.Errorf("MediaType = %q, want %q", got.MediaType, staged.MediaType)
	}
	if got.SizeBytes != staged.SizeBytes {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, staged.SizeBytes)
	}
	if got.StagedPath != staged.StagedPath {
		t.Errorf("StagedPath = %q, want %q", got.StagedPath, staged.StagedPath)
	}
	if !got.SHA256Verified {
		t.Errorf("SHA256Verified = false, want true")
	}
}

func TestGET_WrongSourceID_404(t *testing.T) {
	h, root := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)

	archiveID := "alice-archive-001"
	stageExistingArchive(t, root, archiveID, "alice",
		"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")

	// GET as a different source-id; same 404 + opaque body as not-found.
	req := newGetRequest(t, "recognizer", archiveID)
	w := httptest.NewRecorder()
	h.get(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "archive_not_found" {
		t.Errorf("body code = %q, want archive_not_found (no leak)", code)
	}
}

func TestGET_NonExistent_404(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)

	req := newGetRequest(t, "recognizer", "does-not-exist")
	w := httptest.NewRecorder()
	h.get(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "archive_not_found" {
		t.Errorf("body code = %q, want archive_not_found", code)
	}
}

func TestGET_MalformedArchiveID_404(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)

	// extractUploadID only takes the FIRST path segment, so a path like
	// /v1/archives/foo/bar yields "foo". Any subpath that survives
	// extractUploadID but fails archiveIDRe must produce 404+opaque.
	// httptest.NewRequest insists on a valid HTTP target, so we construct
	// the request via a parsed URL that bypasses URL-syntax validation
	// for the path bytes we want to exercise.
	cases := []struct {
		name      string
		archiveID string
	}{
		// "" (empty) -- extractUploadID returns "" for /v1/archives/ ;
		// regex {1,128} rejects empty.
		{"empty", ""},
		// Plus sign -- not in the character class [a-zA-Z0-9._-].
		{"plus", "has+plus"},
		// Colon -- not in the character class.
		{"colon", "has:colon"},
		// Tilde -- not in the character class.
		{"tilde", "has~tilde"},
		// Over 128 chars (all chars individually valid).
		{"too_long", strings.Repeat("a", 129)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build a request whose r.URL.Path is exactly what we want.
			// We can't use httptest.NewRequest because it parses the target
			// as an HTTP request line and rejects characters that aren't
			// valid in a request URI (spaces, control chars). Instead we
			// build the Request struct directly with a constructed URL.
			req := httptest.NewRequest(http.MethodGet, archivesBasePath+"/placeholder", nil)
			req.URL.Path = archivesBasePath + "/" + tc.archiveID
			req = req.WithContext(ingest.WithDeliveredBy(req.Context(), "recognizer"))

			w := httptest.NewRecorder()
			h.get(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
			}
			if code := decodeErrorBody(t, w.Body); code != "archive_not_found" {
				t.Errorf("body code = %q, want archive_not_found (no shape leak)", code)
			}
		})
	}
}

func TestGET_NotYetFinalized_404(t *testing.T) {
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)

	// Spin up an in-flight upload via POST; PATCH never completes, so
	// archives/<archive_id>/ is never populated. The archive_id is the
	// CLIENT-supplied one, not the upload-id; GET on it must 404.
	clientArchiveID := "inflight-001"
	_, _ = createUpload(t, h, "recognizer", clientArchiveID, []byte("not-finalized"))

	req := newGetRequest(t, "recognizer", clientArchiveID)
	w := httptest.NewRecorder()
	h.get(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrorBody(t, w.Body); code != "archive_not_found" {
		t.Errorf("body code = %q, want archive_not_found", code)
	}
}

func TestGET_NoAuth_500_WiringBug(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)

	// Build a request WITHOUT WithDeliveredBy in the context.
	req := httptest.NewRequest(http.MethodGet, archivesBasePath+"/any-id", nil)
	w := httptest.NewRecorder()
	h.get(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if code := decodeErrorBody(t, w.Body); code != "internal_wiring" {
		t.Errorf("body code = %q, want internal_wiring", code)
	}
}

// ---------------------------------------------------------------------
// Mount — routing + middleware wiring
// ---------------------------------------------------------------------

// withAuthCtxMiddleware is a tiny test middleware that injects a
// delivered_by value into the request context before delegating to the
// next handler. Used by routing tests to satisfy requireDeliveredBy.
func withAuthCtxMiddleware(sourceID string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(ingest.WithDeliveredBy(r.Context(), sourceID))
			next.ServeHTTP(w, r)
		})
	}
}

func TestMount_RoutesOptionsToOptions(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 32212254720)
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodOptions, archivesBasePath, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Tus-Version"); got != "1.0.0" {
		t.Errorf("Tus-Version = %q, want 1.0.0", got)
	}
	if got := w.Header().Get("Tus-Extension"); got != "creation,termination,checksum" {
		t.Errorf("Tus-Extension = %q, want extensions list", got)
	}
}

func TestMount_RoutesPostToCreate(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux, withAuthCtxMiddleware("recognizer"))

	body := []byte("mount-post-body")
	sum := sha256.Sum256(body)
	pairs := validMetaPairs("mount-post-001", int64(len(body)))
	pairs["sha256"] = hex.EncodeToString(sum[:])

	req := httptest.NewRequest(http.MethodPost, archivesBasePath, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.FormatInt(int64(len(body)), 10))
	req.Header.Set("Upload-Metadata", encMeta(pairs))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, archivesBasePath+"/") {
		t.Errorf("Location = %q, want prefix %q", loc, archivesBasePath+"/")
	}
}

func TestMount_RoutesHEADToHead(t *testing.T) {
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux, withAuthCtxMiddleware("recognizer"))

	body := []byte("mount-head")
	uploadID, _ := createUpload(t, h, "recognizer", "mount-head-001", body)

	req := httptest.NewRequest(http.MethodHead, archivesBasePath+"/"+uploadID, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Upload-Offset"); got != "0" {
		t.Errorf("Upload-Offset = %q, want 0", got)
	}
	if got := w.Header().Get("Upload-Length"); got != strconv.FormatInt(int64(len(body)), 10) {
		t.Errorf("Upload-Length = %q, want %d", got, len(body))
	}
}

func TestMount_RoutesPATCHToPatch(t *testing.T) {
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux, withAuthCtxMiddleware("recognizer"))

	// First create the upload (also via the mux to prove POST works).
	body := []byte("mount-patch-body")
	uploadID, _ := createUpload(t, h, "recognizer", "mount-patch-001", body)

	req := httptest.NewRequest(http.MethodPatch, archivesBasePath+"/"+uploadID, bytes.NewReader(body))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", patchContentType)
	req.Header.Set("Upload-Offset", "0")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", w.Code, w.Body.String())
	}
}

func TestMount_RoutesDELETEToDelete(t *testing.T) {
	store := defaultStore()
	h, _ := newTestHandler(t, store, &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux, withAuthCtxMiddleware("recognizer"))

	body := []byte("mount-delete")
	uploadID, _ := createUpload(t, h, "recognizer", "mount-delete-001", body)

	req := httptest.NewRequest(http.MethodDelete, archivesBasePath+"/"+uploadID, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", w.Code, w.Body.String())
	}
}

func TestMount_RoutesGETToGet(t *testing.T) {
	h, root := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux, withAuthCtxMiddleware("recognizer"))

	archiveID := "mount-get-001"
	stageExistingArchive(t, root, archiveID, "recognizer",
		"feedfacefeedface0123456789abcdef0123456789abcdef0123456789abcdef")

	req := httptest.NewRequest(http.MethodGet, archivesBasePath+"/"+archiveID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestMount_MethodNotAllowed_BasePath(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodPut, archivesBasePath, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	allow := w.Header().Get("Allow")
	if !strings.Contains(allow, "OPTIONS") || !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q, want OPTIONS and POST listed", allow)
	}
}

func TestMount_MethodNotAllowed_PerIDPath(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 1<<30)
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodPut, archivesBasePath+"/some-id", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	allow := w.Header().Get("Allow")
	for _, want := range []string{"HEAD", "PATCH", "DELETE", "GET"} {
		if !strings.Contains(allow, want) {
			t.Errorf("Allow = %q, want %s listed", allow, want)
		}
	}
}

// recordingMiddleware appends its tag to a shared slice each time it
// runs. Used to assert middleware-chain order in TestMount_MiddlewareThreadsInOrder.
func recordingMiddleware(tag string, order *[]string, mu *sync.Mutex) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*order = append(*order, tag)
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
}

func TestMount_MiddlewareThreadsInOrder(t *testing.T) {
	h, _ := newTestHandler(t, defaultStore(), &fakeQuota{}, 32212254720)
	mux := http.NewServeMux()

	var mu sync.Mutex
	var order []string
	outer := recordingMiddleware("outer", &order, &mu)
	inner := recordingMiddleware("inner", &order, &mu)

	// Pass [outer, inner] -> expect outer runs first, then inner, then
	// the handler. The Mount comment specifies outermost-to-innermost
	// in the argument order.
	h.Mount(mux, outer, inner)

	req := httptest.NewRequest(http.MethodOptions, archivesBasePath, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	want := []string{"outer", "inner"}
	if len(order) != len(want) {
		t.Fatalf("middleware invocation count = %d (%v), want %d (%v)", len(order), order, len(want), want)
	}
	for i, tag := range want {
		if order[i] != tag {
			t.Errorf("order[%d] = %q, want %q (full = %v)", i, order[i], tag, order)
		}
	}
}
