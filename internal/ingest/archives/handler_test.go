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
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

