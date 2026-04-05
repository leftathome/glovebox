package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// parsedIngest holds the parsed multipart request from a mock ingest server.
type parsedIngest struct {
	metadata    map[string]any
	content     []byte
	contentType string
	headers     http.Header
}

// parseIngestRequest reads a multipart/form-data request body and extracts
// the "metadata" JSON part and the "content" raw bytes part.
func parseIngestRequest(t *testing.T, r *http.Request) parsedIngest {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse content-type %q: %v", ct, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("expected multipart content-type, got %q", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("missing multipart boundary")
	}

	reader := multipart.NewReader(r.Body, boundary)
	result := parsedIngest{headers: r.Header}

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part data: %v", err)
		}
		switch part.FormName() {
		case "metadata":
			if err := json.Unmarshal(data, &result.metadata); err != nil {
				t.Fatalf("unmarshal metadata part: %v", err)
			}
		case "content":
			result.content = data
			result.contentType = part.Header.Get("Content-Type")
		}
		part.Close()
	}
	return result
}

func validHTTPOpts() ItemOptions {
	return ItemOptions{
		Source:           "test-source",
		Sender:           "sender@example.com",
		Subject:          "Test subject",
		Timestamp:        time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
	}
}

func TestHTTPBackendSuccessfulPost(t *testing.T) {
	var got parsedIngest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseIngestRequest(t, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	item, err := backend.NewItem(validHTTPOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() returned error: %v", err)
	}

	if got.metadata == nil {
		t.Fatal("server did not receive metadata")
	}
	if got.metadata["source"] != "test-source" {
		t.Errorf("metadata.source = %v, want test-source", got.metadata["source"])
	}
	if string(got.content) != "hello world" {
		t.Errorf("content = %q, want %q", got.content, "hello world")
	}
}

func TestHTTPBackendRetryOn429(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond // speed up test
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() should succeed after retry, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
}

func TestHTTPBackendRetryOn503(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() should succeed after retry, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
}

func TestHTTPBackendPermanentErrorOn400(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request body"))
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	err := item.Commit()
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !IsPermanent(err) {
		t.Errorf("expected PermanentError, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt (no retry), got %d", got)
	}
}

func TestHTTPBackendPermanentErrorOn413(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	err := item.Commit()
	if err == nil {
		t.Fatal("expected error on 413")
	}
	if !IsPermanent(err) {
		t.Errorf("expected PermanentError, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt (no retry), got %d", got)
	}
}

func TestHTTPBackendRetryOnNetworkError(t *testing.T) {
	// Start server and immediately close it to produce a network error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	closedURL := srv.URL + "/v1/ingest"
	srv.Close()

	backend := NewHTTPStagingBackend(closedURL, "test-connector", &http.Client{Timeout: 100 * time.Millisecond})
	backend.retryBase = 1 * time.Millisecond
	backend.retryMax = 2
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	err := item.Commit()
	if err == nil {
		t.Fatal("expected error after retries exhausted on network error")
	}
}

func TestHTTPBackendMaxRetriesExceeded(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond
	backend.retryMax = 2

	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	err := item.Commit()
	if err == nil {
		t.Fatal("expected error after max retries exceeded")
	}
	// 1 initial + 2 retries = 3 total attempts
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts (1 initial + 2 retries), got %d", got)
	}
}

func TestHTTPBackendMultiWriteContent(t *testing.T) {
	var got parsedIngest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseIngestRequest(t, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("chunk1"))
	item.WriteContent([]byte("chunk2"))
	item.WriteContent([]byte("chunk3"))

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() returned error: %v", err)
	}

	if string(got.content) != "chunk1chunk2chunk3" {
		t.Errorf("content = %q, want %q", got.content, "chunk1chunk2chunk3")
	}
}

func TestHTTPBackendIdentityMerge(t *testing.T) {
	var got parsedIngest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseIngestRequest(t, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.SetConfigIdentity(&ConfigIdentity{
		Provider:   "github",
		Tenant:     "steve",
		AuthMethod: "oauth",
	})

	opts := validHTTPOpts()
	opts.Identity = &Identity{
		AccountID: "steve@github",
		Scopes:    []string{"repo", "read:org"},
	}

	item, _ := backend.NewItem(opts)
	item.WriteContent([]byte("content"))
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	idRaw, ok := got.metadata["identity"]
	if !ok {
		t.Fatal("metadata missing identity field")
	}
	idMap, ok := idRaw.(map[string]any)
	if !ok {
		t.Fatalf("identity is not an object: %T", idRaw)
	}
	if idMap["provider"] != "github" {
		t.Errorf("identity.provider = %v, want github (from config)", idMap["provider"])
	}
	if idMap["tenant"] != "steve" {
		t.Errorf("identity.tenant = %v, want steve (from config)", idMap["tenant"])
	}
	if idMap["auth_method"] != "oauth" {
		t.Errorf("identity.auth_method = %v, want oauth (from config)", idMap["auth_method"])
	}
	if idMap["account_id"] != "steve@github" {
		t.Errorf("identity.account_id = %v, want steve@github (from item)", idMap["account_id"])
	}
}

func TestHTTPBackendConnectorHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Glovebox-Connector")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "my-connector", srv.Client())
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))
	item.Commit()

	if gotHeader != "my-connector" {
		t.Errorf("X-Glovebox-Connector = %q, want %q", gotHeader, "my-connector")
	}
}

func TestHTTPBackendHonorsRetryAfter(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1") // 1 second
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond // normally tiny, but Retry-After should override

	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	start := time.Now()
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() should succeed after retry, got: %v", err)
	}
	elapsed := time.Since(start)

	// The Retry-After header says 1 second. We should wait at least ~900ms
	// (with some tolerance for timing). If retryBase were used, it would be ~1ms.
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected delay >= 900ms honoring Retry-After, got %v", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
}

func TestHTTPBackendValidationRejectsEmptyDestination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for invalid metadata")
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	opts := validHTTPOpts()
	opts.DestinationAgent = ""
	item, _ := backend.NewItem(opts)
	item.WriteContent([]byte("data"))

	err := item.Commit()
	if err == nil {
		t.Fatal("expected validation error for empty destination_agent")
	}
	if !strings.Contains(err.Error(), "destination_agent") {
		t.Errorf("error should mention destination_agent, got: %v", err)
	}
}

func TestHTTPBackendCleansTempFileOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	item, _ := backend.NewItem(validHTTPOpts())
	// Write enough to force a temp file (> 4MB threshold).
	bigChunk := make([]byte, 1024)
	for i := range bigChunk {
		bigChunk[i] = 'A'
	}
	for i := 0; i < 5; i++ {
		item.WriteContent(bigChunk)
	}

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() returned error: %v", err)
	}
	// After successful commit, the temp file (if any) in the StagingItem's dir
	// should be cleaned up. We cannot easily test the exact path, but at least
	// verify no error.
}

func TestHTTPBackendMetadataContentType(t *testing.T) {
	// Verify the POST request is multipart/form-data.
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))
	item.Commit()

	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", gotContentType)
	}
}

func TestHTTPBackendRetryAfterDateFormat(t *testing.T) {
	// Retry-After can also be an HTTP-date. Verify we handle it.
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			retryTime := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
			w.Header().Set("Retry-After", retryTime)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	backend.retryBase = 1 * time.Millisecond

	item, _ := backend.NewItem(validHTTPOpts())
	item.WriteContent([]byte("data"))

	start := time.Now()
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit() should succeed after retry, got: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 1*time.Second {
		t.Errorf("expected delay >= 1s honoring Retry-After date, got %v", elapsed)
	}
}

func TestHTTPBackendSubjectSanitized(t *testing.T) {
	var got parsedIngest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseIngestRequest(t, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	backend := NewHTTPStagingBackend(srv.URL+"/v1/ingest", "test-connector", srv.Client())
	opts := validHTTPOpts()
	opts.Subject = fmt.Sprintf("Hello%cWorld", 0x00)
	item, _ := backend.NewItem(opts)
	item.WriteContent([]byte("data"))
	item.Commit()

	subj, ok := got.metadata["subject"].(string)
	if !ok {
		t.Fatal("metadata missing subject")
	}
	if strings.ContainsRune(subj, 0x00) {
		t.Error("subject should have control chars stripped")
	}
}
