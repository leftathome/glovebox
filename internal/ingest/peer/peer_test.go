package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func spiffeCert(t *testing.T, uri string) *x509.Certificate {
	t.Helper()
	cert := &x509.Certificate{}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatalf("parse %q: %v", uri, err)
		}
		cert.URIs = []*url.URL{u}
	}
	return cert
}

func TestFromCertificate(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		domain   string
		wantKind string
		wantName string
		wantErr  bool
	}{
		{
			name:     "connector identity",
			uri:      "spiffe://glovebox/connector/rss",
			domain:   "glovebox",
			wantKind: "connector",
			wantName: "rss",
		},
		{
			name:     "producer identity",
			uri:      "spiffe://glovebox/producer/recognizer",
			domain:   "glovebox",
			wantKind: "producer",
			wantName: "recognizer",
		},
		{
			name:    "no uri san",
			uri:     "",
			domain:  "glovebox",
			wantErr: true,
		},
		{
			// A certificate that chains to our CA but names a different
			// trust domain is not ours to honour.
			name:    "foreign trust domain",
			uri:     "spiffe://someone-else/connector/rss",
			domain:  "glovebox",
			wantErr: true,
		},
		{
			name:    "malformed path",
			uri:     "spiffe://glovebox/rss",
			domain:  "glovebox",
			wantErr: true,
		},
		{
			name:    "non-spiffe uri ignored",
			uri:     "https://example.com/connector/rss",
			domain:  "glovebox",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromCertificate(spiffeCert(t, tc.uri), tc.domain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("FromCertificate(%q) = %+v, want error", tc.uri, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromCertificate(%q): %v", tc.uri, err)
			}
			if got.Kind != tc.wantKind || got.Name != tc.wantName {
				t.Errorf("got kind=%q name=%q, want kind=%q name=%q", got.Kind, got.Name, tc.wantKind, tc.wantName)
			}
		})
	}
}

func TestFromCertificate_NilCert(t *testing.T) {
	if _, err := FromCertificate(nil, "glovebox"); err == nil {
		t.Error("nil certificate should not yield an identity")
	}
}

func TestContextRoundTrip(t *testing.T) {
	want := Peer{Kind: "connector", Name: "imap", URI: "spiffe://glovebox/connector/imap"}
	ctx := NewContext(context.Background(), want)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext returned no peer")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A bare context must not yield an identity: handlers rely on "no peer"
// meaning "transport unverified", never "trusted".
func TestFromContext_EmptyIsNotTrusted(t *testing.T) {
	if p, ok := FromContext(context.Background()); ok {
		t.Errorf("empty context yielded peer %+v", p)
	}
}

func TestMiddleware_RejectsRequestWithoutClientCert(t *testing.T) {
	var reached bool
	h := Middleware("glovebox")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	// No TLS state at all: this is what a plaintext request looks like.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ingest", nil))

	if reached {
		t.Error("handler ran for a request with no client certificate")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_RejectsUnrecognizedIdentity(t *testing.T) {
	var reached bool
	h := Middleware("glovebox")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{spiffeCert(t, "spiffe://other/connector/rss")},
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Error("handler ran for a certificate from a foreign trust domain")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestMiddleware_AttachesIdentity(t *testing.T) {
	var got Peer
	var ok bool
	h := Middleware("glovebox")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{spiffeCert(t, "spiffe://glovebox/connector/rss")},
	}
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("handler saw no peer identity")
	}
	if got.Name != "rss" || got.Kind != "connector" {
		t.Errorf("peer = %+v, want connector/rss", got)
	}
}
