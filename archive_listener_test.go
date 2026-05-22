package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/config"
)

// fakeVault is a minimal in-memory auth.VaultClient for the
// integration tests. Mirrors integFakeVault in
// internal/ingest/archives/integration_test.go.
type fakeVault struct {
	tokens map[string]string
}

func (f *fakeVault) ListPath(context.Context, string, string) ([]string, error) {
	out := make([]string, 0, len(f.tokens))
	for k := range f.tokens {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeVault) ReadKV(_ context.Context, _, path string) (map[string]any, error) {
	parts := strings.Split(path, "/")
	sid := parts[len(parts)-1]
	if tok, ok := f.tokens[sid]; ok {
		return map[string]any{"token": tok}, nil
	}
	return nil, fmt.Errorf("fakeVault: no token for %q", sid)
}

func makeArchiveTestConfig(stagingRoot string) config.Config {
	return config.Config{
		Ingest: config.IngestConfig{
			Enabled: true,
			Port:    0,
			Auth: config.IngestAuthConfig{
				Enabled: true,
				Vault: config.VaultClientConfig{
					Addr:       "http://fake-vault.test:8200",
					K8sRole:    "test-role",
					KVMount:    "secret",
					TokensPath: "glovebox/ingest-tokens",
				},
				ReloadIntervalSeconds: 300,
				PerIPRateLimit: config.RateLimitWindowConf{
					WindowSeconds: 60,
					MaxRejected:   10,
					LRUCapacity:   100,
				},
				GlobalRateLimit: config.RateLimitWindowConf{
					WindowSeconds: 60,
					MaxRejected:   1000,
				},
			},
			Archives: config.IngestArchivesConfig{
				Enabled:                    true,
				StagingRoot:                stagingRoot,
				MaxUploadSize:              100 * 1024 * 1024,
				PerSourceMaxConcurrent:     4,
				GlobalMaxConcurrent:        32,
				PerSourceSoftCapPct:        40,
				GlobalHardCapPct:           95,
				GlobalHardCapHysteresisPct: 85,
				PatchIdleTimeoutSeconds:    300,
				CleanupIntervalSeconds:     3600,
				CleanupTmpAgeHours:         72,
				CleanupFinalizeAgeHours:    1,
				DoneRetentionDays:          7,
			},
		},
	}
}

func TestBootstrapArchives_Disabled(t *testing.T) {
	mux := http.NewServeMux()
	cfg := config.Config{
		Ingest: config.IngestConfig{
			Enabled: true,
			Auth: config.IngestAuthConfig{
				Enabled: false,
			},
			Archives: config.IngestArchivesConfig{
				Enabled: false,
			},
		},
	}
	if err := bootstrapArchives(context.Background(), cfg, mux); err != nil {
		t.Fatalf("bootstrapArchives returned error when disabled: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/archives")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nothing mounted)", resp.StatusCode)
	}
}

func TestBootstrapArchives_BadCIDR(t *testing.T) {
	mux := http.NewServeMux()
	cfg := makeArchiveTestConfig(t.TempDir())
	cfg.Ingest.Auth.TrustedProxyCIDRs = []string{"not-a-cidr"}

	err := bootstrapArchivesWithClient(context.Background(), cfg, mux, &fakeVault{tokens: map[string]string{}})
	if err == nil {
		t.Fatal("expected CIDR parse error, got nil")
	}
	if !strings.Contains(err.Error(), "trusted_proxy_cidrs") {
		t.Errorf("error %q does not mention trusted_proxy_cidrs", err.Error())
	}
}

// TestBootstrapArchives_AuthGuards is the end-to-end "did main.go
// actually wire the archive listener" test: bootstrapArchivesWithClient
// is called with a fake Vault, and a POST hits the running httptest
// server. Without auth -> 401; with the configured bearer token -> 201
// with the Location header set.
func TestBootstrapArchives_AuthGuards(t *testing.T) {
	stagingRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stagingRoot, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stagingRoot, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir .tmp-archives: %v", err)
	}

	const tokenHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const sourceID = "test-source"

	mux := http.NewServeMux()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := makeArchiveTestConfig(stagingRoot)
	fv := &fakeVault{tokens: map[string]string{sourceID: tokenHex}}

	if err := bootstrapArchivesWithClient(ctx, cfg, mux, fv); err != nil {
		t.Fatalf("bootstrapArchivesWithClient: %v", err)
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	meta := buildArchiveMetadataHeader(map[string]string{
		"archive_id":       "test-archive-001",
		"archive_filename": "archive.mbox",
		"media_type":       "archive/mbox",
		"matcher_id":       "test-matcher",
		"provider":         "recognizer",
		"sha256":           "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"size_bytes":       "1024",
	})

	// (1) POST with no Authorization -> 401.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/archives", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "1024")
	req.Header.Set("Upload-Metadata", meta)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST unauth: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST without auth: status = %d, want 401", resp.StatusCode)
	}

	// (2) POST with the valid bearer token -> 201.
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/archives", nil)
	req2.Header.Set("Tus-Resumable", "1.0.0")
	req2.Header.Set("Upload-Length", "1024")
	req2.Header.Set("Upload-Metadata", meta)
	req2.Header.Set("Authorization", "Bearer "+tokenHex)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST auth: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Errorf("POST with valid auth: status = %d, want 201", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); !strings.HasPrefix(loc, "/v1/archives/") {
		t.Errorf("Location header = %q, want /v1/archives/<id>", loc)
	}
}

// TestBootstrapArchives_VaultLoadFailureMountsFallback exercises spec
// 10 §4.1: when the initial Vault load fails, /v1/archives* mounts a
// 503 fallback instead of the real handler. We trigger this by passing
// a failingVaultClient (same sentinel that production loginVault would
// produce on K8s-auth failure).
func TestBootstrapArchives_VaultLoadFailureMountsFallback(t *testing.T) {
	stagingRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stagingRoot, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stagingRoot, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	mux := http.NewServeMux()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := makeArchiveTestConfig(stagingRoot)
	failing := &failingVaultClient{err: fmt.Errorf("simulated vault outage")}

	if err := bootstrapArchivesWithClient(ctx, cfg, mux, failing); err != nil {
		t.Fatalf("bootstrapArchivesWithClient: %v", err)
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/archives")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (fallback)", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
}

// buildArchiveMetadataHeader encodes a key->value map into the
// tus.io v1.0.0 Upload-Metadata format (key1 base64(v1),key2 base64(v2)).
// Mirrors buildUploadMetadataHeader in
// internal/ingest/archives/integration_test.go.
func buildArchiveMetadataHeader(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}
