package connector

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPClient_DefaultUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewHTTPClient(HTTPClientOptions{})
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if gotUA != DefaultUserAgent {
		t.Errorf("expected User-Agent %q, got %q", DefaultUserAgent, gotUA)
	}
}

func TestHTTPClient_OverridesExistingUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewHTTPClient(HTTPClientOptions{})
	req, err := http.NewRequest("GET", srv.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error creating request: %v", err)
	}
	req.Header.Set("User-Agent", "SneakyBot/1.0")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if gotUA != DefaultUserAgent {
		t.Errorf("expected User-Agent %q, got %q", DefaultUserAgent, gotUA)
	}
}

func TestHTTPClient_CustomUserAgent(t *testing.T) {
	custom := "MyCustomBot/1.0 (+https://example.com)"
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewHTTPClient(HTTPClientOptions{UserAgent: custom})
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if gotUA != custom {
		t.Errorf("expected User-Agent %q, got %q", custom, gotUA)
	}
}

func TestHTTPClient_DefaultTimeout(t *testing.T) {
	client := NewHTTPClient(HTTPClientOptions{})
	if client.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", client.Timeout)
	}
}

func TestHTTPClient_CustomTimeout(t *testing.T) {
	client := NewHTTPClient(HTTPClientOptions{Timeout: 10 * time.Second})
	if client.Timeout != 10*time.Second {
		t.Errorf("expected timeout 10s, got %v", client.Timeout)
	}
}
