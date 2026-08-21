package connector

import (
	"testing"
	"time"
)

// With none of the variables set the framework keeps the pre-existing
// plaintext client, so an un-migrated deployment is unaffected.
func TestNewIngestHTTPClient_PlainWhenUnconfigured(t *testing.T) {
	t.Setenv(EnvIngestCA, "")
	t.Setenv(EnvIngestClientCert, "")
	t.Setenv(EnvIngestClientKey, "")

	client, err := NewIngestHTTPClient(5 * time.Second)
	if err != nil {
		t.Fatalf("NewIngestHTTPClient: %v", err)
	}
	if client.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", client.Timeout)
	}
	if IngestTLSConfigured() {
		t.Error("IngestTLSConfigured() should be false with no variables set")
	}
}

// A half-configured client must be an error. Falling back to plaintext
// would silently undo the control the operator was turning on, and the
// connector would keep working -- which is how such a mistake survives.
func TestNewIngestHTTPClient_PartialConfigIsAnError(t *testing.T) {
	cases := []struct{ ca, cert, key string }{
		{"/ca.crt", "", ""},
		{"", "/tls.crt", ""},
		{"", "", "/tls.key"},
		{"/ca.crt", "/tls.crt", ""},
	}
	for _, tc := range cases {
		t.Setenv(EnvIngestCA, tc.ca)
		t.Setenv(EnvIngestClientCert, tc.cert)
		t.Setenv(EnvIngestClientKey, tc.key)

		if _, err := NewIngestHTTPClient(0); err == nil {
			t.Errorf("ca=%q cert=%q key=%q: expected an error for a partial mTLS configuration", tc.ca, tc.cert, tc.key)
		}
	}
}

func TestNewIngestHTTPClient_MissingFilesAreAnError(t *testing.T) {
	t.Setenv(EnvIngestCA, "/nonexistent/ca.crt")
	t.Setenv(EnvIngestClientCert, "/nonexistent/tls.crt")
	t.Setenv(EnvIngestClientKey, "/nonexistent/tls.key")

	if _, err := NewIngestHTTPClient(0); err == nil {
		t.Error("expected an error when the configured files do not exist")
	}
}
