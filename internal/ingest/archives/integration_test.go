// End-to-end integration tests for the archive delivery endpoint
// (spec 13 Wave C Task C4).
//
// Unlike handler_test.go (which drives create/head/patch/delete/get
// directly via httptest.NewRecorder) and listener_test.go (which
// drives the C3 wire-up), these tests exercise the full stack as a
// black box:
//
//   - A real auth.TokenStore loaded via a fake VaultClient.
//   - A real auth.RateLimiter, auth.ProxyResolver, archives.Telemetry,
//     archives.Store.
//   - A real archives.Handler mounted on an http.ServeMux through the
//     real auth.Middleware.
//   - An httptest.NewServer to expose all of the above over a live
//     TCP socket.
//   - A hand-rolled tus.io v1.0.0 client (POST + chunked PATCH + HEAD)
//     speaking the real wire protocol; no third-party tus client.
//
// The scenarios cover the §Task C4 plan list:
//
//	1. Happy path -- 50 MB synthetic mbox upload, sha256 verified,
//	   staged under archives/<id>/raw/.
//	2. Happy path -- 5-file tarball uploaded; archives/<id>/tree/
//	   contains all files at mode 0600.
//	3. Corrupted sha256 -- claimed-vs-computed mismatch at finalize;
//	   400 + sha256_mismatch; archives/<id>/ does NOT exist; tmp dir
//	   is cleaned.
//	4. Tar-safety violation -- a ".." path entry rejected at finalize;
//	   400 + tar_unsafe_entry; archives/<id>/ absent; tmp cleaned.
//	5. Resume after partial PATCH -- 30 MB first, HEAD confirms
//	   offset, second PATCH delivers the rest, finalize succeeds.
//	6. Idempotent retry -- second POST with same archive_id + sha256
//	   from the same source-id returns 303 with no new state.
//	7. Auth -- POST without Authorization returns 401, no state
//	   allocated.
//	8. Auth -- POST with an unknown-but-shaped bearer token returns
//	   401 with the SAME body as the missing-token case (no leak).
//
// The 50 MB-per-test size is a deliberate trade-off: large enough to
// exercise multi-chunk PATCH behavior, the rolling sha256 across
// chunks, and the io.LimitReader path; small enough that `go test`
// completes in seconds on a developer laptop. The 12 GiB end-to-end
// smoke test lives in Wave D2 as a bash script driving the built
// container image.

package archives

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/ingest/auth"
)

// ---------------------------------------------------------------------
// Test scaffolding.
// ---------------------------------------------------------------------

// integTestSourceID is the source-id every happy-path scenario uses.
// Fixed string so log/metric labels are predictable in failure
// inspection. Matches auth's sourceIDRe (lowercase, starts with letter).
const integTestSourceID = "recognizer"

// integTestTokenHex is a deterministic 64-char hex token loaded into
// the TokenStore by the test scaffolding. Tests construct the
// Authorization header as `Bearer <integTestTokenHex>`.
const integTestTokenHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// integFakeVault is an in-memory auth.VaultClient used by the
// integration tests. Mirrors fakeListenerVault in listener_test.go but
// scoped to this file so the integration tests can evolve without
// touching the C3 listener fixtures.
type integFakeVault struct {
	tokens map[string]string // sourceID -> hex token
}

// Compile-time check: integFakeVault implements auth.VaultClient.
var _ auth.VaultClient = (*integFakeVault)(nil)

// ListPath returns the configured source-ids verbatim.
func (f *integFakeVault) ListPath(_ context.Context, _, _ string) ([]string, error) {
	out := make([]string, 0, len(f.tokens))
	for k := range f.tokens {
		out = append(out, k)
	}
	return out, nil
}

// ReadKV extracts the source-id from the trailing path segment and
// returns its token (or an error if unknown). Matches the
// helpers_test.go pattern.
func (f *integFakeVault) ReadKV(_ context.Context, _, path string) (map[string]any, error) {
	parts := strings.Split(path, "/")
	sid := parts[len(parts)-1]
	if tok, ok := f.tokens[sid]; ok {
		return map[string]any{"token": tok}, nil
	}
	return nil, fmt.Errorf("integFakeVault: no data for %q", sid)
}

// integTestServer holds the per-test fixture. The httptest.Server is
// already started by setupIntegrationServer; tests use srv.URL for the
// base. The staging root is exposed so tests can assert on-disk state.
type integTestServer struct {
	srv         *httptest.Server
	stagingRoot string
	sourceID    string
	tokenHex    string
}

// setupIntegrationServer wires the full archive stack onto an
// httptest.Server. Returns the running server + fixture metadata; the
// server is auto-Closed via t.Cleanup. The TokenStore is preloaded
// with one source-id (integTestSourceID) via the fake VaultClient.
//
// The ProxyResolver is constructed with no trusted CIDRs so
// r.RemoteAddr (127.0.0.1 from httptest) is used verbatim for
// rate-limit bucketing and X-Forwarded-For is ignored. That matches
// the production "trust nothing by default" stance.
func setupIntegrationServer(t *testing.T) *integTestServer {
	t.Helper()

	stagingRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stagingRoot, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir .tmp-archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stagingRoot, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}

	// TokenStore loaded with one source-id.
	fv := &integFakeVault{tokens: map[string]string{integTestSourceID: integTestTokenHex}}
	store := auth.NewTokenStore()
	if err := store.Reload(context.Background(), auth.ReloadConfig{
		Source: auth.NewVaultTokenSource(fv, "secret", "glovebox/ingest-tokens"),
	}); err != nil {
		t.Fatalf("token store reload: %v", err)
	}

	// RateLimiter with generous limits so tests don't trip rate caps.
	rl := auth.NewRateLimiter(auth.RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  1000,
		GlobalMaxRejected: 10000,
	})
	pr := &auth.ProxyResolver{} // no trusted CIDRs

	tel, err := NewTelemetry("integration", "integration")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}

	uploadStore := NewStore(StoreConfig{
		PerSourceMaxConcurrent: 4,
		GlobalMaxConcurrent:    32,
	})

	// Quota provider explicitly nil -- these tests never exercise the
	// hard-cap path; the handler's nil-guard skips ShouldBlock().
	handler := NewHandler(uploadStore, nil, tel, HandlerConfig{
		StagingRoot:      stagingRoot,
		TusMaxSize:       100 * 1024 * 1024, // 100 MB cap; 50 MB tests fit
		TmpExpiry:        72 * time.Hour,
		PatchIdleTimeout: 5 * time.Minute,
	})

	mux := http.NewServeMux()
	mw := auth.Middleware(store, rl, pr, tel)
	handler.Mount(mux, Middleware(mw))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &integTestServer{
		srv:         srv,
		stagingRoot: stagingRoot,
		sourceID:    integTestSourceID,
		tokenHex:    integTestTokenHex,
	}
}

// ---------------------------------------------------------------------
// tus.io client helpers (hand-rolled; no external dependency).
// ---------------------------------------------------------------------

// buildUploadMetadataHeader encodes a key->value map into the tus.io
// Upload-Metadata header format: `key1 base64(value1),key2 base64(value2)`
// per spec 13 §4.2 + the tus.io v1.0.0 core spec. Values are encoded
// with standard base64 (NOT URL-safe), matching ParseUploadMetadata's
// decoder.
func buildUploadMetadataHeader(meta map[string]string) string {
	parts := make([]string, 0, len(meta))
	for k, v := range meta {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

// tusUploadResult captures the upcoming-final-PATCH outcome so tests
// can assert on the finalize step's response. Status is the HTTP
// status of the LAST PATCH (which is the finalize-triggering one for a
// completed upload); ErrorCode is the parsed "error" field on a 4xx
// JSON body, empty otherwise; UploadID is the server-assigned id from
// the POST 201 Location header.
type tusUploadResult struct {
	UploadID    string
	FinalStatus int
	FinalBody   []byte
	ErrorCode   string
}

// doTusPOST sends POST /v1/archives with the supplied metadata +
// Upload-Length. Returns the upload-id parsed from the Location
// header. Asserts a 201 response; failures land in t.Fatalf with the
// observed status + body so debugging is straightforward.
//
// If wantStatus != 201, the helper asserts that exact status and
// returns the empty upload-id (caller is checking an error path).
func doTusPOST(t *testing.T, ts *integTestServer, meta map[string]string, uploadLength int64, wantStatus int) (string, *http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.srv.URL+archivesBasePath, nil)
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", strconv.FormatInt(uploadLength, 10))
	req.Header.Set("Upload-Metadata", buildUploadMetadataHeader(meta))
	req.Header.Set("Authorization", "Bearer "+ts.tokenHex)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != wantStatus {
		t.Fatalf("POST status = %d, want %d; body=%q", resp.StatusCode, wantStatus, body)
	}

	if wantStatus != http.StatusCreated {
		return "", resp, body
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("POST 201 missing Location header")
	}
	uploadID := strings.TrimPrefix(loc, archivesBasePath+"/")
	if uploadID == loc || uploadID == "" {
		t.Fatalf("POST 201 Location header malformed: %q", loc)
	}
	return uploadID, resp, body
}

// doTusPATCH sends a PATCH /v1/archives/<upload-id> with the supplied
// body and offset. Returns the (status, response-headers, body) tuple
// so the caller can assert on whichever fields the scenario cares
// about. Sets the canonical tus.io headers automatically.
func doTusPATCH(t *testing.T, ts *integTestServer, uploadID string, offset int64, chunk []byte) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch,
		ts.srv.URL+archivesBasePath+"/"+uploadID, bytes.NewReader(chunk))
	if err != nil {
		t.Fatalf("build PATCH: %v", err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	req.Header.Set("Content-Type", patchContentType)
	req.Header.Set("Content-Length", strconv.Itoa(len(chunk)))
	req.Header.Set("Authorization", "Bearer "+ts.tokenHex)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Clone(), body
}

// doTusHEAD sends HEAD /v1/archives/<upload-id> and returns the
// parsed Upload-Offset, the status, and the response headers. A
// non-200 status returns offset = -1; the caller asserts on status.
func doTusHEAD(t *testing.T, ts *integTestServer, uploadID string) (int, http.Header, int64) {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead,
		ts.srv.URL+archivesBasePath+"/"+uploadID, nil)
	if err != nil {
		t.Fatalf("build HEAD: %v", err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Authorization", "Bearer "+ts.tokenHex)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, resp.Header.Clone(), -1
	}
	offStr := resp.Header.Get("Upload-Offset")
	off, perr := strconv.ParseInt(offStr, 10, 64)
	if perr != nil {
		t.Fatalf("HEAD Upload-Offset %q: %v", offStr, perr)
	}
	return resp.StatusCode, resp.Header.Clone(), off
}

// uploadInChunks PATCHes body in chunks of chunkSize, asserting each
// intermediate PATCH returns 204 with the expected Upload-Offset, and
// returns the result of the final PATCH (which triggers finalize).
//
// The function deliberately uses a hand-rolled loop rather than a
// streaming PATCH so the test exercises the multi-chunk path (the
// rolling sha256, the per-chunk offset accounting) that a single big
// PATCH would skip.
func uploadInChunks(t *testing.T, ts *integTestServer, uploadID string, body []byte, chunkSize int) tusUploadResult {
	t.Helper()
	total := int64(len(body))
	var offset int64
	res := tusUploadResult{UploadID: uploadID}

	for offset < total {
		end := offset + int64(chunkSize)
		if end > total {
			end = total
		}
		chunk := body[offset:end]
		status, hdr, respBody := doTusPATCH(t, ts, uploadID, offset, chunk)

		// The final chunk's PATCH is the finalize-triggering one; we
		// record its status/body and return WITHOUT asserting that it
		// is 204 — caller may be testing a finalize-time failure
		// (sha256 mismatch, tar-safety violation) where the status is
		// 400. For intermediate chunks we assert 204 hard.
		isFinalChunk := end == total
		if !isFinalChunk {
			if status != http.StatusNoContent {
				t.Fatalf("intermediate PATCH at offset %d: status = %d, body=%q",
					offset, status, respBody)
			}
			newOff, _ := strconv.ParseInt(hdr.Get("Upload-Offset"), 10, 64)
			if newOff != end {
				t.Fatalf("intermediate PATCH Upload-Offset = %d, want %d", newOff, end)
			}
		} else {
			res.FinalStatus = status
			res.FinalBody = respBody
			if status >= 400 {
				// Parse {"error":"<code>"} for failure-path tests.
				var obj map[string]any
				if jerr := json.Unmarshal(respBody, &obj); jerr == nil {
					if code, ok := obj["error"].(string); ok {
						res.ErrorCode = code
					}
				}
			}
		}
		offset = end
	}
	return res
}

// ---------------------------------------------------------------------
// Tarball builder for the tarball-shape integration scenarios.
// ---------------------------------------------------------------------

// buildIntegrationTar produces an uncompressed tar archive with the
// supplied entries. Each entry is a regular file at mode 0600. The
// builder is local to the integration tests so we don't depend on
// untar_test.go's `entry` type and its richer field set.
func buildIntegrationTar(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     int64(len(content)),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %q: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar Write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

// buildTraversalTarball produces a malicious 2-entry tarball where one
// entry uses a ".." path component to attempt directory traversal.
// The tar-safety check (spec §4.7 step 4) rejects this at finalize.
func buildTraversalTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := []struct {
		name string
		body []byte
	}{
		{name: "ok.txt", body: []byte("ok")},
		{name: "../escape.txt", body: []byte("evil")},
	}
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     int64(len(e.body)),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %q: %v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("tar Write %q: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

// makeSyntheticMbox produces a deterministic but high-entropy synthetic
// mbox-shaped payload of exactly size bytes. We don't need real mbox
// parsing — finalize verifies sha256 + size only — but we DO want
// high-entropy bytes so the file isn't trivially compressible by any
// transport in the middle. crypto/rand is overkill for tests but
// guarantees the produced sha256 is sensitive to size changes.
func makeSyntheticMbox(t *testing.T, size int) ([]byte, string) {
	t.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])
}

// rawMboxMeta returns a valid Upload-Metadata map for a raw mbox
// delivery. Tests override the sha256 field to exercise corruption
// scenarios.
func rawMboxMeta(archiveID, filename, sha256Hex string, sizeBytes int64) map[string]string {
	return map[string]string{
		"archive_id":            archiveID,
		"archive_filename":      filename,
		"subtree_relative_path": ".",
		"media_type":            "archive/mbox",
		"matcher_id":            "test/matcher",
		"provider":              "test",
		"sha256":                sha256Hex,
		"size_bytes":            strconv.FormatInt(sizeBytes, 10),
	}
}

// tarballMeta returns a valid Upload-Metadata map for a tarball
// delivery. The archive_filename is recorded but the staged path uses
// `tree/` (not `raw/<filename>`).
func tarballMeta(archiveID, filename, sha256Hex string, sizeBytes int64) map[string]string {
	return map[string]string{
		"archive_id":            archiveID,
		"archive_filename":      filename,
		"subtree_relative_path": "Mail",
		"media_type":            "archive/google-takeout-subtree",
		"matcher_id":            "google-takeout/mail",
		"provider":              "google",
		"sha256":                sha256Hex,
		"size_bytes":            strconv.FormatInt(sizeBytes, 10),
	}
}

// ---------------------------------------------------------------------
// Scenario 1: TestIntegration_HappyPath_RawMbox
// ---------------------------------------------------------------------

func TestIntegration_HappyPath_RawMbox(t *testing.T) {
	ts := setupIntegrationServer(t)

	const sizeMB = 50
	body, sha256Hex := makeSyntheticMbox(t, sizeMB*1024*1024)
	archiveID := "test-happy-raw-001"

	uploadID, _, _ := doTusPOST(t, ts,
		rawMboxMeta(archiveID, "happy.mbox", sha256Hex, int64(len(body))),
		int64(len(body)), http.StatusCreated)

	res := uploadInChunks(t, ts, uploadID, body, 8*1024*1024)
	if res.FinalStatus != http.StatusNoContent {
		t.Fatalf("final PATCH status = %d, want 204; body=%q",
			res.FinalStatus, res.FinalBody)
	}

	// Staged file exists with correct content + mode.
	rawPath := filepath.Join(ts.stagingRoot, "archives", archiveID, "raw", "happy.mbox")
	got, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read staged raw: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("staged content mismatch: len(got)=%d len(want)=%d", len(got), len(body))
	}
	stagedSum := sha256.Sum256(got)
	if hex.EncodeToString(stagedSum[:]) != sha256Hex {
		t.Errorf("staged sha256 mismatch")
	}

	fi, err := os.Stat(rawPath)
	if err != nil {
		t.Fatalf("stat staged: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("staged raw mode = %o, want 0600", fi.Mode().Perm())
	}

	// metadata.json carries the §4.8 fields including the Identity block.
	metaBytes, rerr := os.ReadFile(filepath.Join(ts.stagingRoot, "archives", archiveID, "metadata.json"))
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}
	var receipt FinalizeReceipt
	if jerr := json.Unmarshal(metaBytes, &receipt); jerr != nil {
		t.Fatalf("unmarshal metadata.json: %v", jerr)
	}
	if receipt.ArchiveID != archiveID {
		t.Errorf("metadata.ArchiveID = %q, want %q", receipt.ArchiveID, archiveID)
	}
	if receipt.DeliveredBy != ts.sourceID {
		t.Errorf("metadata.DeliveredBy = %q, want %q", receipt.DeliveredBy, ts.sourceID)
	}
	if receipt.MediaType != "archive/mbox" {
		t.Errorf("metadata.MediaType = %q, want archive/mbox", receipt.MediaType)
	}
	if receipt.SizeBytes != int64(len(body)) {
		t.Errorf("metadata.SizeBytes = %d, want %d", receipt.SizeBytes, len(body))
	}
	if receipt.SHA256 != sha256Hex {
		t.Errorf("metadata.SHA256 = %q, want %q", receipt.SHA256, sha256Hex)
	}
	if !receipt.SHA256Verified {
		t.Error("metadata.SHA256Verified = false, want true")
	}
	if receipt.RawFilename != "happy.mbox" {
		t.Errorf("metadata.RawFilename = %q, want happy.mbox", receipt.RawFilename)
	}
	if receipt.EntriesExtracted != 0 {
		t.Errorf("metadata.EntriesExtracted = %d, want 0 for raw shape", receipt.EntriesExtracted)
	}
	if receipt.Identity == nil {
		t.Fatal("metadata.Identity is nil; want populated block")
	}
	if receipt.Identity.Provider != "ingest" {
		t.Errorf("Identity.Provider = %q, want ingest", receipt.Identity.Provider)
	}
	if receipt.Identity.AuthMethod != "bearer_token" {
		t.Errorf("Identity.AuthMethod = %q, want bearer_token", receipt.Identity.AuthMethod)
	}
	if receipt.Identity.AccountID != ts.sourceID {
		t.Errorf("Identity.AccountID = %q, want %q", receipt.Identity.AccountID, ts.sourceID)
	}

	// Tmp file is gone after a successful finalize.
	tmpPath := filepath.Join(ts.stagingRoot, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file %q lingered after finalize (err=%v)", tmpPath, err)
	}
}

// ---------------------------------------------------------------------
// Scenario 2: TestIntegration_HappyPath_Tarball
// ---------------------------------------------------------------------

func TestIntegration_HappyPath_Tarball(t *testing.T) {
	ts := setupIntegrationServer(t)

	entries := map[string][]byte{
		"a.txt":      []byte("hello a"),
		"b.txt":      []byte("hello b"),
		"sub/c.txt":  []byte("hello c"),
		"sub/d.txt":  []byte("hello d"),
		"deep/e.txt": []byte("hello e"),
	}
	body := buildIntegrationTar(t, entries)
	sum := sha256.Sum256(body)
	sha256Hex := hex.EncodeToString(sum[:])
	archiveID := "test-happy-tar-001"

	uploadID, _, _ := doTusPOST(t, ts,
		tarballMeta(archiveID, "takeout-mail.tar", sha256Hex, int64(len(body))),
		int64(len(body)), http.StatusCreated)

	// Chunk smaller than the tarball so we still cross the multi-PATCH
	// path for tar bodies. 4 KiB is well under any plausible single
	// entry size in this fixture.
	res := uploadInChunks(t, ts, uploadID, body, 4*1024)
	if res.FinalStatus != http.StatusNoContent {
		t.Fatalf("final PATCH status = %d, want 204; body=%q",
			res.FinalStatus, res.FinalBody)
	}

	for name, want := range entries {
		stagedPath := filepath.Join(ts.stagingRoot, "archives", archiveID, "tree", name)
		got, err := os.ReadFile(stagedPath)
		if err != nil {
			t.Errorf("read entry %q: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("entry %q content mismatch: got %q want %q", name, got, want)
		}
		fi, ferr := os.Stat(stagedPath)
		if ferr != nil {
			t.Errorf("stat entry %q: %v", name, ferr)
			continue
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("entry %q mode = %o, want 0600", name, fi.Mode().Perm())
		}
	}

	metaBytes, rerr := os.ReadFile(filepath.Join(ts.stagingRoot, "archives", archiveID, "metadata.json"))
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}
	var receipt FinalizeReceipt
	if jerr := json.Unmarshal(metaBytes, &receipt); jerr != nil {
		t.Fatalf("unmarshal metadata.json: %v", jerr)
	}
	if receipt.EntriesExtracted != len(entries) {
		t.Errorf("metadata.EntriesExtracted = %d, want %d",
			receipt.EntriesExtracted, len(entries))
	}
	if receipt.RawFilename != "" {
		t.Errorf("metadata.RawFilename = %q, want empty for tar shape", receipt.RawFilename)
	}
}

// ---------------------------------------------------------------------
// Scenario 3: TestIntegration_CorruptedSHA256_RejectedAtFinalize
// ---------------------------------------------------------------------

func TestIntegration_CorruptedSHA256_RejectedAtFinalize(t *testing.T) {
	ts := setupIntegrationServer(t)

	const sizeMB = 4
	body, _ := makeSyntheticMbox(t, sizeMB*1024*1024)

	// Claim a deliberately wrong (but well-formed) sha256. The bytes
	// the server receives hash to the REAL value; the metadata says
	// otherwise; finalize must catch the mismatch.
	wrongSum := sha256.Sum256([]byte("definitely not the real content"))
	wrongHex := hex.EncodeToString(wrongSum[:])
	archiveID := "test-corrupted-sha-001"

	uploadID, _, _ := doTusPOST(t, ts,
		rawMboxMeta(archiveID, "corrupt.mbox", wrongHex, int64(len(body))),
		int64(len(body)), http.StatusCreated)

	res := uploadInChunks(t, ts, uploadID, body, 1024*1024)
	if res.FinalStatus != http.StatusBadRequest {
		t.Fatalf("final PATCH status = %d, want 400; body=%q",
			res.FinalStatus, res.FinalBody)
	}
	if res.ErrorCode != "sha256_mismatch" {
		t.Errorf("error code = %q, want sha256_mismatch", res.ErrorCode)
	}

	// archives/<id>/ must NOT exist.
	archDir := filepath.Join(ts.stagingRoot, "archives", archiveID)
	if _, err := os.Stat(archDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archives/%s/ exists after sha mismatch (err=%v)", archiveID, err)
	}

	// .tmp-archives/<upload-id> and .finalize/ are cleaned.
	tmpPath := filepath.Join(ts.stagingRoot, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file %q lingered after sha mismatch (err=%v)", tmpPath, err)
	}
	finalizePath := filepath.Join(ts.stagingRoot, ".tmp-archives", uploadID+".finalize")
	if _, err := os.Stat(finalizePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finalize dir %q lingered after sha mismatch (err=%v)", finalizePath, err)
	}
}

// ---------------------------------------------------------------------
// Scenario 4: TestIntegration_TarSafetyViolation_RejectedAtFinalize
// ---------------------------------------------------------------------

func TestIntegration_TarSafetyViolation_RejectedAtFinalize(t *testing.T) {
	ts := setupIntegrationServer(t)

	body := buildTraversalTarball(t)
	sum := sha256.Sum256(body)
	sha256Hex := hex.EncodeToString(sum[:])
	archiveID := "test-tar-unsafe-001"

	uploadID, _, _ := doTusPOST(t, ts,
		tarballMeta(archiveID, "evil.tar", sha256Hex, int64(len(body))),
		int64(len(body)), http.StatusCreated)

	res := uploadInChunks(t, ts, uploadID, body, 4*1024)
	if res.FinalStatus != http.StatusBadRequest {
		t.Fatalf("final PATCH status = %d, want 400; body=%q",
			res.FinalStatus, res.FinalBody)
	}
	if res.ErrorCode != "tar_unsafe_entry" {
		t.Errorf("error code = %q, want tar_unsafe_entry", res.ErrorCode)
	}

	archDir := filepath.Join(ts.stagingRoot, "archives", archiveID)
	if _, err := os.Stat(archDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archives/%s/ exists after tar safety violation (err=%v)", archiveID, err)
	}

	tmpPath := filepath.Join(ts.stagingRoot, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file %q lingered after tar safety violation (err=%v)", tmpPath, err)
	}
	finalizePath := filepath.Join(ts.stagingRoot, ".tmp-archives", uploadID+".finalize")
	if _, err := os.Stat(finalizePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finalize dir %q lingered after tar safety violation (err=%v)",
			finalizePath, err)
	}
}

// ---------------------------------------------------------------------
// Scenario 5: TestIntegration_Resume_AfterPartialPATCH
// ---------------------------------------------------------------------

func TestIntegration_Resume_AfterPartialPATCH(t *testing.T) {
	ts := setupIntegrationServer(t)

	const sizeMB = 50
	body, sha256Hex := makeSyntheticMbox(t, sizeMB*1024*1024)
	archiveID := "test-resume-001"

	uploadID, _, _ := doTusPOST(t, ts,
		rawMboxMeta(archiveID, "resume.mbox", sha256Hex, int64(len(body))),
		int64(len(body)), http.StatusCreated)

	// First PATCH: 30 MB.
	firstCut := 30 * 1024 * 1024
	status, hdr, respBody := doTusPATCH(t, ts, uploadID, 0, body[:firstCut])
	if status != http.StatusNoContent {
		t.Fatalf("first PATCH status = %d, want 204; body=%q", status, respBody)
	}
	gotOff, _ := strconv.ParseInt(hdr.Get("Upload-Offset"), 10, 64)
	if gotOff != int64(firstCut) {
		t.Fatalf("first PATCH Upload-Offset = %d, want %d", gotOff, firstCut)
	}

	// HEAD: verify resume info available.
	headStatus, headHdr, headOff := doTusHEAD(t, ts, uploadID)
	if headStatus != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", headStatus)
	}
	if headOff != int64(firstCut) {
		t.Errorf("HEAD Upload-Offset = %d, want %d", headOff, firstCut)
	}
	if headHdr.Get("Upload-Length") != strconv.Itoa(len(body)) {
		t.Errorf("HEAD Upload-Length = %q, want %d",
			headHdr.Get("Upload-Length"), len(body))
	}
	if headHdr.Get("Tus-Resumable") != tusVersion {
		t.Errorf("HEAD Tus-Resumable = %q, want %s",
			headHdr.Get("Tus-Resumable"), tusVersion)
	}

	// Second PATCH: the remaining 20 MB. This is the finalize-triggering
	// chunk; we expect 204 with Upload-Offset == len(body).
	status2, hdr2, respBody2 := doTusPATCH(t, ts, uploadID, int64(firstCut), body[firstCut:])
	if status2 != http.StatusNoContent {
		t.Fatalf("resume PATCH status = %d, want 204; body=%q", status2, respBody2)
	}
	gotOff2, _ := strconv.ParseInt(hdr2.Get("Upload-Offset"), 10, 64)
	if gotOff2 != int64(len(body)) {
		t.Errorf("resume PATCH Upload-Offset = %d, want %d", gotOff2, len(body))
	}

	// Verify the archive landed.
	rawPath := filepath.Join(ts.stagingRoot, "archives", archiveID, "raw", "resume.mbox")
	got, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read staged after resume: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("staged content mismatch after resume: len(got)=%d len(want)=%d",
			len(got), len(body))
	}
}

// ---------------------------------------------------------------------
// Scenario 6: TestIntegration_Idempotent_Retry_303
// ---------------------------------------------------------------------

func TestIntegration_Idempotent_Retry_303(t *testing.T) {
	ts := setupIntegrationServer(t)

	const sizeMB = 2
	body, sha256Hex := makeSyntheticMbox(t, sizeMB*1024*1024)
	archiveID := "test-idempotent-001"

	// First POST: full upload through finalize.
	uploadID, _, _ := doTusPOST(t, ts,
		rawMboxMeta(archiveID, "idem.mbox", sha256Hex, int64(len(body))),
		int64(len(body)), http.StatusCreated)
	res := uploadInChunks(t, ts, uploadID, body, 512*1024)
	if res.FinalStatus != http.StatusNoContent {
		t.Fatalf("first POST final PATCH status = %d, want 204", res.FinalStatus)
	}

	// Second POST with the SAME archive_id + sha256 from the same
	// source-id. Disable redirect following on the http client so we
	// can observe the 303 directly.
	noRedirect := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.srv.URL+archivesBasePath, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", strconv.Itoa(len(body)))
	req.Header.Set("Upload-Metadata", buildUploadMetadataHeader(
		rawMboxMeta(archiveID, "idem.mbox", sha256Hex, int64(len(body)))))
	req.Header.Set("Authorization", "Bearer "+ts.tokenHex)

	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second POST status = %d, want 303", resp.StatusCode)
	}
	wantLoc := archivesBasePath + "/" + archiveID
	if got := resp.Header.Get("Location"); got != wantLoc {
		t.Errorf("Location header = %q, want %q", got, wantLoc)
	}
}

// ---------------------------------------------------------------------
// Scenario 7: TestIntegration_Auth_401_OnMissingToken
// ---------------------------------------------------------------------

func TestIntegration_Auth_401_OnMissingToken(t *testing.T) {
	ts := setupIntegrationServer(t)

	req, err := http.NewRequest(http.MethodPost, ts.srv.URL+archivesBasePath, nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", "1024")
	req.Header.Set("Upload-Metadata", buildUploadMetadataHeader(
		rawMboxMeta("ignored", "x.mbox",
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1024)))
	// Deliberately NO Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", resp.StatusCode, body)
	}
	// Body should be opaque (the auth middleware writes "unauthorized"
	// via http.Error, which yields a trailing newline -- we tolerate that
	// rather than asserting an exact byte sequence).
	if got := strings.TrimSpace(string(body)); got != "unauthorized" {
		t.Errorf("body = %q, want %q", got, "unauthorized")
	}
}

// ---------------------------------------------------------------------
// Scenario 8: TestIntegration_Auth_401_OnUnknownToken
// ---------------------------------------------------------------------

func TestIntegration_Auth_401_OnUnknownToken(t *testing.T) {
	ts := setupIntegrationServer(t)

	// A well-shaped 64-hex token that's NOT in the TokenStore. Spec
	// §5.1 mandates the SAME body shape as the missing-token case so a
	// probing caller can't distinguish "I didn't send a token" from "I
	// sent a wrong-but-shaped token".
	unknownHex := "ff" + strings.Repeat("00", 31)

	req, err := http.NewRequest(http.MethodPost, ts.srv.URL+archivesBasePath, nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", "1024")
	req.Header.Set("Upload-Metadata", buildUploadMetadataHeader(
		rawMboxMeta("ignored", "x.mbox",
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1024)))
	req.Header.Set("Authorization", "Bearer "+unknownHex)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", resp.StatusCode, body)
	}
	if got := strings.TrimSpace(string(body)); got != "unauthorized" {
		t.Errorf("body = %q, want %q (must match missing-token body for no-leak)", got, "unauthorized")
	}
}

// ---------------------------------------------------------------------
// Scenario 9: 401 bodies are byte-equal across missing-vs-unknown
// (spec 10 §5.1 / §5.2 no-leak invariant). The two scenario tests above
// each assert against the literal "unauthorized" individually, but
// would silently pass if one returned "unauthorized " with trailing
// whitespace (TrimSpace masks that). This test compares both bodies
// byte-for-byte to lock the no-leak contract.
// ---------------------------------------------------------------------

func TestIntegration_Auth_401_BodiesByteEqual(t *testing.T) {
	ts := setupIntegrationServer(t)

	get401Body := func(authHeader string) (int, []byte) {
		req, err := http.NewRequest(http.MethodPost, ts.srv.URL+archivesBasePath, nil)
		if err != nil {
			t.Fatalf("build req: %v", err)
		}
		req.Header.Set("Tus-Resumable", tusVersion)
		req.Header.Set("Upload-Length", "1024")
		req.Header.Set("Upload-Metadata", buildUploadMetadataHeader(
			rawMboxMeta("ignored", "x.mbox",
				"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1024)))
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	statusMissing, bodyMissing := get401Body("") // no Authorization header
	statusUnknown, bodyUnknown := get401Body("Bearer " + "ff" + strings.Repeat("00", 31))

	if statusMissing != http.StatusUnauthorized || statusUnknown != http.StatusUnauthorized {
		t.Fatalf("expected both 401; got missing=%d unknown=%d", statusMissing, statusUnknown)
	}
	if !bytes.Equal(bodyMissing, bodyUnknown) {
		t.Errorf("401 bodies differ:\n  missing=%q\n  unknown=%q\nspec 10 §5.1 requires byte-equal bodies", bodyMissing, bodyUnknown)
	}
}
