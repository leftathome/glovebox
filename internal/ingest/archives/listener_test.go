// Tests for the spec 13 archive-listener wire-up (Wave C Task C3).
//
// Coverage:
//
//   TestVerifySameFilesystem_HappyPath
//   TestVerifySameFilesystem_CreatesMissingDirs
//   TestStartArchiveListener_TokenLoadFails_503Fallback
//   TestStartArchiveListener_HappyPath_MountsReal
//   TestStartTokenReloadGoroutine_PeriodicTick
//   TestStartTokenReloadGoroutine_SIGHUP
//   TestStartTokenReloadGoroutine_PeriodicFailureDoesNotCrash
//   TestStartTokenReloadGoroutine_ExitsOnCtxCancel

package archives

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/ingest/auth"
)

// fakeListenerVault is an in-memory VaultClient for the C3 listener
// tests. The reload counter lets tests assert that SIGHUP / periodic
// tick actually fired the Reload call. Kept separate from the
// archives-package helpers because the test fixtures here are
// listener-specific.
type fakeListenerVault struct {
	mu      sync.Mutex
	data    map[string]map[string]any
	listErr error
	calls   atomic.Int64
}

func (f *fakeListenerVault) ListPath(_ context.Context, _, _ string) ([]string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeListenerVault) ReadKV(_ context.Context, _, path string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(path, "/")
	sid := parts[len(parts)-1]
	if d, ok := f.data[sid]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("no data for %q", sid)
}

func newFakeListenerVault() *fakeListenerVault {
	canonicalHex := "ab" + strings.Repeat("00", 31)
	return &fakeListenerVault{
		data: map[string]map[string]any{
			"alpha": {"token": canonicalHex},
		},
	}
}

func TestVerifySameFilesystem_HappyPath(t *testing.T) {
	root := t.TempDir()
	if err := verifySameFilesystem(root); err != nil {
		t.Errorf("verifySameFilesystem: %v", err)
	}
	for _, sub := range []string{"archives", ".tmp-archives"} {
		if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
			t.Errorf("subdir %s missing: %v", sub, err)
		}
	}
}

func TestVerifySameFilesystem_CreatesMissingDirs(t *testing.T) {
	root := t.TempDir()
	if err := verifySameFilesystem(root); err != nil {
		t.Fatalf("verifySameFilesystem: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "archives")); err != nil {
		t.Errorf("archives subdir not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tmp-archives")); err != nil {
		t.Errorf(".tmp-archives subdir not created: %v", err)
	}
}

func newArchiveTestCfg(t *testing.T, vc auth.VaultClient) (*http.ServeMux, ArchiveListenerConfig) {
	t.Helper()
	mux := http.NewServeMux()
	tel, err := NewTelemetry("test-listener", "test-listener")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	cfg := ArchiveListenerConfig{
		Mux:              mux,
		StagingRoot:      t.TempDir(),
		PVCCapacityBytes: 1_000_000_000,
		TusMaxSize:       1024 * 1024 * 1024,
		PatchIdleTimeout: 5 * time.Second,
		Store: NewStore(StoreConfig{
			PerSourceMaxConcurrent: 4,
			GlobalMaxConcurrent:    32,
		}),
		Telemetry:  tel,
		TokenStore: auth.NewTokenStore(),
		AuthReloadConfig: auth.ReloadConfig{
			Source: auth.NewVaultTokenSource(vc, "secret", "glovebox/ingest-tokens"),
		},
		RateLimiter: auth.NewRateLimiter(auth.RateLimitConfig{
			Window:            time.Minute,
			PerIPMaxRejected:  100,
			GlobalMaxRejected: 1000,
		}),
		ProxyResolver:       &auth.ProxyResolver{},
		TokenReloadInterval: time.Hour,
		CleanupInterval:     time.Hour,
		QuotaInterval:       time.Minute,
	}
	return mux, cfg
}

func TestStartArchiveListener_TokenLoadFails_503Fallback(t *testing.T) {
	vc := newFakeListenerVault()
	vc.listErr = errors.New("vault unreachable")
	mux, cfg := newArchiveTestCfg(t, vc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := StartArchiveListener(ctx, cfg); got != nil {
		t.Errorf("StartArchiveListener returned non-nil QuotaState on token-load failure: %+v", got)
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/archives", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60", resp.Header.Get("Retry-After"))
	}
}

func TestStartArchiveListener_HappyPath_MountsReal(t *testing.T) {
	vc := newFakeListenerVault()
	mux, cfg := newArchiveTestCfg(t, vc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := StartArchiveListener(ctx, cfg); got == nil {
		t.Fatal("StartArchiveListener returned nil QuotaState on happy path")
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// OPTIONS /v1/archives with NO Authorization should 401 (the auth
	// middleware kicks in before the handler's OPTIONS branch).
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/archives", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("OPTIONS unauthenticated status = %d, want 401", resp.StatusCode)
	}

	// OPTIONS with the canonical token should 200.
	canonicalHex := "ab" + strings.Repeat("00", 31)
	req2, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/archives", nil)
	req2.Header.Set("Authorization", "Bearer "+canonicalHex)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("OPTIONS w/ auth: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("OPTIONS authenticated status = %d, want 200", resp2.StatusCode)
	}
	if resp2.Header.Get("Tus-Version") != "1.0.0" {
		t.Errorf("Tus-Version = %q, want 1.0.0", resp2.Header.Get("Tus-Version"))
	}
}

func TestStartTokenReloadGoroutine_PeriodicTick(t *testing.T) {
	vc := newFakeListenerVault()
	store := auth.NewTokenStore()
	cfg := auth.ReloadConfig{
		Source: auth.NewVaultTokenSource(vc, "secret", "glovebox/ingest-tokens"),
	}
	if err := store.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("priming Reload: %v", err)
	}
	primed := vc.calls.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		StartTokenReloadGoroutine(ctx, store, cfg, 20*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if vc.calls.Load() >= primed+3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := vc.calls.Load(); got < primed+3 {
		t.Errorf("vault calls = %d, want >= %d (primed=%d)", got, primed+3, primed)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not exit within 2s after cancel")
	}
}

func TestStartTokenReloadGoroutine_SIGHUP(t *testing.T) {
	vc := newFakeListenerVault()
	store := auth.NewTokenStore()
	cfg := auth.ReloadConfig{
		Source: auth.NewVaultTokenSource(vc, "secret", "glovebox/ingest-tokens"),
	}
	if err := store.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("priming Reload: %v", err)
	}
	primed := vc.calls.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Long interval so SIGHUP is the only trigger.
		StartTokenReloadGoroutine(ctx, store, cfg, time.Hour)
		close(done)
	}()

	// Give the goroutine a moment to install the signal handler.
	time.Sleep(50 * time.Millisecond)

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill SIGHUP: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if vc.calls.Load() > primed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := vc.calls.Load(); got <= primed {
		t.Errorf("SIGHUP did not trigger reload: calls=%d, primed=%d", got, primed)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not exit within 2s after cancel")
	}
}

func TestStartTokenReloadGoroutine_PeriodicFailureDoesNotCrash(t *testing.T) {
	vc := newFakeListenerVault()
	store := auth.NewTokenStore()
	cfg := auth.ReloadConfig{
		Source: auth.NewVaultTokenSource(vc, "secret", "glovebox/ingest-tokens"),
	}
	if err := store.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("priming Reload: %v", err)
	}
	vc.mu.Lock()
	vc.listErr = errors.New("transient vault outage")
	vc.mu.Unlock()
	primed := vc.calls.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		StartTokenReloadGoroutine(ctx, store, cfg, 20*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if vc.calls.Load() >= primed+3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := vc.calls.Load(); got < primed+3 {
		t.Errorf("expected the goroutine to keep firing reloads despite failure: calls=%d, primed=%d", got, primed)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not exit within 2s after cancel")
	}
}

func TestStartTokenReloadGoroutine_ExitsOnCtxCancel(t *testing.T) {
	vc := newFakeListenerVault()
	store := auth.NewTokenStore()
	cfg := auth.ReloadConfig{
		Source: auth.NewVaultTokenSource(vc, "secret", "glovebox/ingest-tokens"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartTokenReloadGoroutine(ctx, store, cfg, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not exit within 1s after cancel")
	}
}
