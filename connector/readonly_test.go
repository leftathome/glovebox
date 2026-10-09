package connector

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadOnlyTransport(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewReadOnlyHTTPClient("test", 5*time.Second)
	if err != nil {
		t.Fatalf("NewReadOnlyHTTPClient: %v", err)
	}
	if client.Jar == nil {
		t.Fatal("client has no cookie jar; session cookies would be dropped")
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(method, srv.URL+"/ok", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", method, err)
		}
		resp.Body.Close()
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("read requests reaching server = %d, want 2", got)
	}

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodConnect, http.MethodTrace, "PROPFIND",
	} {
		req, _ := http.NewRequest(method, srv.URL+"/write?token=secret", strings.NewReader("x"))
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: request was allowed", method)
		}
		if !errors.Is(err, ErrWriteBlocked) {
			t.Fatalf("%s: error = %v, want ErrWriteBlocked", method, err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("a blocked request reached the server: hits = %d, want 2", got)
	}
}

func TestReadOnlyTransportBlocksRedirectedWrite(t *testing.T) {
	// A 307 preserves the method; the guard must hold on the hop too.
	var writes atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
	}))
	defer target.Close()
	rt := &ReadOnlyTransport{}
	req, _ := http.NewRequest(http.MethodPost, target.URL, nil)
	req.URL.RawQuery = "token=secret"
	_, err := rt.RoundTrip(req)
	if !errors.Is(err, ErrWriteBlocked) {
		t.Fatalf("error = %v, want ErrWriteBlocked", err)
	}
	// The guard's own error carries host + path only, never the query.
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks query string: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("write reached the server")
	}
}
