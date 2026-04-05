package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/config"
)

// validMetadataJSON returns a well-formed metadata JSON string that will
// pass staging.Validate with the given allowlist agent name.
func validMetadataJSON(agent string) string {
	return fmt.Sprintf(`{
		"source": "test-connector",
		"sender": "unit-test",
		"subject": "test item",
		"timestamp": "2026-01-15T10:30:00Z",
		"destination_agent": %q,
		"content_type": "text/plain"
	}`, agent)
}

// buildMultipart creates a valid multipart body with metadata and content parts.
func buildMultipart(metadataJSON string, content []byte) (*bytes.Buffer, string) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// metadata part
	metaPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"; filename="metadata.json"`},
		"Content-Type":        {"application/json"},
	})
	metaPart.Write([]byte(metadataJSON))

	// content part
	contentPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="content"; filename="content.raw"`},
		"Content-Type":        {"application/octet-stream"},
	})
	contentPart.Write(content)

	writer.Close()
	return body, writer.FormDataContentType()
}

func newTestHandler(t *testing.T, cfg config.IngestConfig, allowlist []string) (*Handler, string) {
	t.Helper()
	stagingDir := t.TempDir()
	h := NewHandler(stagingDir, cfg, allowlist)
	h.SetReady()
	return h, stagingDir
}

func defaultIngestConfig() config.IngestConfig {
	return config.IngestConfig{
		Enabled:               true,
		Port:                  9091,
		MaxBodyBytes:          1 << 20, // 1 MiB
		MaxMetadataBytes:      1 << 16, // 64 KiB
		BackpressureThreshold: 100,
		RequestTimeoutSeconds: 60,
	}
}

func TestValidPostReturns202(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("hello world"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "accepted" {
		t.Errorf("expected status=accepted, got %q", result["status"])
	}
	if result["item_id"] == "" {
		t.Error("expected non-empty item_id")
	}

	// Verify item exists in staging directory
	itemDir := filepath.Join(stagingDir, result["item_id"])
	if _, err := os.Stat(filepath.Join(itemDir, "metadata.json")); err != nil {
		t.Errorf("metadata.json not found in staging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(itemDir, "content.raw")); err != nil {
		t.Errorf("content.raw not found in staging: %v", err)
	}

	// Verify content matches
	data, _ := os.ReadFile(filepath.Join(itemDir, "content.raw"))
	if string(data) != "hello world" {
		t.Errorf("content mismatch: got %q", string(data))
	}
}

func TestMissingMetadataPart(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Build multipart with only content, no metadata
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	contentPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="content"; filename="content.raw"`},
		"Content-Type":        {"application/octet-stream"},
	})
	contentPart.Write([]byte("hello"))
	writer.Close()

	resp, err := http.Post(ts.URL+"/v1/ingest", writer.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMissingContentPart(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Build multipart with only metadata, no content
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	metaPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"; filename="metadata.json"`},
		"Content-Type":        {"application/json"},
	})
	metaPart.Write([]byte(validMetadataJSON("home-agent")))
	writer.Close()

	resp, err := http.Post(ts.URL+"/v1/ingest", writer.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDuplicateParts(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Build multipart with two metadata parts
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for i := 0; i < 2; i++ {
		metaPart, _ := writer.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="metadata"; filename="metadata.json"`},
			"Content-Type":        {"application/json"},
		})
		metaPart.Write([]byte(validMetadataJSON("home-agent")))
	}
	contentPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="content"; filename="content.raw"`},
		"Content-Type":        {"application/octet-stream"},
	})
	contentPart.Write([]byte("data"))
	writer.Close()

	resp, err := http.Post(ts.URL+"/v1/ingest", writer.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestInvalidMetadataJSON(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart("not valid json{{{", []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMetadataValidationFailure(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Missing required fields (empty JSON object)
	body, contentType := buildMultipart(`{}`, []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestOversizedMetadata(t *testing.T) {
	cfg := defaultIngestConfig()
	cfg.MaxMetadataBytes = 64 // very small limit
	h, _ := newTestHandler(t, cfg, []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
}

func TestOversizedBody(t *testing.T) {
	cfg := defaultIngestConfig()
	cfg.MaxBodyBytes = 256 // very small limit
	h, _ := newTestHandler(t, cfg, []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	bigContent := bytes.Repeat([]byte("x"), 1024)
	body, contentType := buildMultipart(validMetadataJSON("home-agent"), bigContent)
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	// Accept either 413 or 400 -- the body limit reader may cause a parse error
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 413 or 400, got %d", resp.StatusCode)
	}
}

func TestBackpressure429(t *testing.T) {
	cfg := defaultIngestConfig()
	cfg.BackpressureThreshold = 1
	h, stagingDir := newTestHandler(t, cfg, []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Pre-create 2 items in staging to exceed threshold
	for i := 0; i < 2; i++ {
		dir := filepath.Join(stagingDir, fmt.Sprintf("item-%d", i))
		os.MkdirAll(dir, 0755)
	}
	if err := h.InitQueueDepth(); err != nil {
		t.Fatalf("InitQueueDepth: %v", err)
	}

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 429, got %d: %s", resp.StatusCode, string(b))
	}

	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("expected Retry-After: 5, got %q", ra)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "backpressure" {
		t.Errorf("expected status=backpressure, got %v", result["status"])
	}
}

func TestNotReady503(t *testing.T) {
	stagingDir := t.TempDir()
	h := NewHandler(stagingDir, defaultIngestConfig(), []string{"home-agent"})
	// Do NOT call SetReady

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "unavailable" {
		t.Errorf("expected status=unavailable, got %q", result["status"])
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ingest")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestAtomicWrite(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("atomic test data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}

	// No partial items should be in .ingest-tmp
	tmpDir := filepath.Join(stagingDir, ".ingest-tmp")
	entries, err := os.ReadDir(tmpDir)
	if err == nil {
		for _, e := range entries {
			t.Errorf("found orphan in .ingest-tmp: %s", e.Name())
		}
	}

	// The final item should be directly in staging (not in .ingest-tmp)
	stagingEntries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range stagingEntries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			found = true
			// Verify both files exist
			itemDir := filepath.Join(stagingDir, e.Name())
			if _, err := os.Stat(filepath.Join(itemDir, "metadata.json")); err != nil {
				t.Errorf("metadata.json missing from final item: %v", err)
			}
			if _, err := os.Stat(filepath.Join(itemDir, "content.raw")); err != nil {
				t.Errorf("content.raw missing from final item: %v", err)
			}
		}
	}
	if !found {
		t.Error("no item directory found in staging")
	}
}

func TestOrphanCleanup(t *testing.T) {
	stagingDir := t.TempDir()

	// Create orphan .ingest-tmp directory with leftover data
	orphanDir := filepath.Join(stagingDir, ".ingest-tmp", "orphan-item")
	os.MkdirAll(orphanDir, 0755)
	os.WriteFile(filepath.Join(orphanDir, "content.raw"), []byte("stale"), 0644)

	h := NewHandler(stagingDir, defaultIngestConfig(), []string{"home-agent"})
	if err := h.InitQueueDepth(); err != nil {
		t.Fatalf("InitQueueDepth: %v", err)
	}

	// .ingest-tmp should be removed
	if _, err := os.Stat(filepath.Join(stagingDir, ".ingest-tmp")); !os.IsNotExist(err) {
		t.Error("expected .ingest-tmp to be removed after InitQueueDepth")
	}
}

func TestQueueDepthInitialization(t *testing.T) {
	stagingDir := t.TempDir()

	// Pre-create 3 item directories
	for i := 0; i < 3; i++ {
		os.MkdirAll(filepath.Join(stagingDir, fmt.Sprintf("item-%d", i)), 0755)
	}

	// Also create a hidden dir that should not count
	os.MkdirAll(filepath.Join(stagingDir, ".hidden"), 0755)

	// And a regular file that should not count
	os.WriteFile(filepath.Join(stagingDir, "somefile.txt"), []byte("nope"), 0644)

	h := NewHandler(stagingDir, defaultIngestConfig(), []string{"home-agent"})
	if err := h.InitQueueDepth(); err != nil {
		t.Fatalf("InitQueueDepth: %v", err)
	}

	depth := h.queueDepth.Load()
	if depth != 3 {
		t.Errorf("expected queueDepth=3, got %d", depth)
	}
}

func TestWrongMetadataContentType(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Build multipart with wrong content-type on metadata part
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	metaPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"; filename="metadata.json"`},
		"Content-Type":        {"text/plain"},
	})
	metaPart.Write([]byte(validMetadataJSON("home-agent")))
	contentPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="content"; filename="content.raw"`},
		"Content-Type":        {"application/octet-stream"},
	})
	contentPart.Write([]byte("data"))
	writer.Close()

	resp, err := http.Post(ts.URL+"/v1/ingest", writer.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUnexpectedPart(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	metaPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"; filename="metadata.json"`},
		"Content-Type":        {"application/json"},
	})
	metaPart.Write([]byte(validMetadataJSON("home-agent")))
	contentPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="content"; filename="content.raw"`},
		"Content-Type":        {"application/octet-stream"},
	})
	contentPart.Write([]byte("data"))
	// Extra unexpected part
	extraPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="extra"; filename="extra.dat"`},
		"Content-Type":        {"application/octet-stream"},
	})
	extraPart.Write([]byte("surprise"))
	writer.Close()

	resp, err := http.Post(ts.URL+"/v1/ingest", writer.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestQueueDepthIncrements(t *testing.T) {
	h, _ := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	before := h.queueDepth.Load()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	after := h.queueDepth.Load()
	if after != before+1 {
		t.Errorf("expected queueDepth to increment from %d to %d, got %d", before, before+1, after)
	}
}

// Ensure the ingest-tmp dir is created for writes and cleaned after success.
func TestIngestTmpCreatedAndClean(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(ts.URL+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	// .ingest-tmp should exist (as a directory) but be empty of item dirs
	tmpDir := filepath.Join(stagingDir, ".ingest-tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("unexpected error reading .ingest-tmp: %v", err)
	}
	for _, e := range entries {
		t.Errorf("orphan in .ingest-tmp after successful ingest: %s", e.Name())
	}
}

// Ensure the server starts and responds (smoke test for StartServer).
func TestStartServer(t *testing.T) {
	stagingDir := t.TempDir()
	h := NewHandler(stagingDir, defaultIngestConfig(), []string{"home-agent"})
	h.SetReady()

	srv := StartServer(h, 0, 5*time.Second)
	defer srv.Close()

	// Port 0 won't actually work for http.Server.ListenAndServe, so just
	// verify the server was created with the right handler and timeouts.
	if srv.ReadTimeout != 5*time.Second {
		t.Errorf("expected ReadTimeout 5s, got %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 5*time.Second {
		t.Errorf("expected WriteTimeout 5s, got %v", srv.WriteTimeout)
	}
}
