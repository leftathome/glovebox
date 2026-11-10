package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// postItem sends one valid ingest request and returns the status code.
func postItem(t *testing.T, url string) int {
	t.Helper()
	body, contentType := buildMultipart(validMetadataJSON("home-agent"), []byte("data"))
	resp, err := http.Post(url+"/v1/ingest", contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// drainStaging removes every staged item, as the scanner does once it has
// dispatched and routed them.
func drainStaging(t *testing.T, stagingDir string) {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' {
			if err := os.RemoveAll(filepath.Join(stagingDir, e.Name())); err != nil {
				t.Fatalf("remove %s: %v", e.Name(), err)
			}
		}
	}
}

// glovebox-0dfx: queueDepth only ever went up (DecrementQueue had no
// production caller), so once BackpressureThreshold items had been accepted
// over the pod's lifetime every request got 429 -- even with staging empty.
func TestBackpressureClearsOnceStagingDrains(t *testing.T) {
	cfg := defaultIngestConfig()
	cfg.BackpressureThreshold = 2
	h, stagingDir := newTestHandler(t, cfg, []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for i := 0; i < 2; i++ {
		if code := postItem(t, ts.URL); code != http.StatusAccepted {
			t.Fatalf("post %d: expected 202, got %d", i, code)
		}
	}
	if code := postItem(t, ts.URL); code != http.StatusTooManyRequests {
		t.Fatalf("at threshold: expected 429, got %d", code)
	}

	drainStaging(t, stagingDir)
	if err := h.ReconcileQueueDepth(); err != nil {
		t.Fatalf("ReconcileQueueDepth: %v", err)
	}

	if got := h.queueDepth.Load(); got != 0 {
		t.Errorf("after drain + reconcile: expected depth 0, got %d", got)
	}
	if code := postItem(t, ts.URL); code != http.StatusAccepted {
		t.Fatalf("after drain: expected 202, got %d (backpressure stuck)", code)
	}
}

// The recount must mirror InitQueueDepth: only non-hidden directories are
// items. .ingest-tmp (in-flight writes) and stray files do not count.
func TestReconcileQueueDepthCountsOnlyItemDirs(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	for _, d := range []string{"item-a", "item-b", "item-c", ".ingest-tmp", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(stagingDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.queueDepth.Store(5000) // drifted value from a long-lived pod

	if err := h.ReconcileQueueDepth(); err != nil {
		t.Fatalf("ReconcileQueueDepth: %v", err)
	}
	if got := h.queueDepth.Load(); got != 3 {
		t.Errorf("expected depth 3 (item dirs only), got %d", got)
	}
	// Unlike InitQueueDepth, a periodic recount must not delete in-flight
	// writes under .ingest-tmp.
	if _, err := os.Stat(filepath.Join(stagingDir, ".ingest-tmp")); err != nil {
		t.Errorf(".ingest-tmp removed by reconcile: %v", err)
	}
}

// StartDepthReconciler heals a drifted counter on its own, without a caller
// invoking ReconcileQueueDepth, and stops when its context ends.
func TestStartDepthReconcilerHealsDrift(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	if err := os.MkdirAll(filepath.Join(stagingDir, "item-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.queueDepth.Store(4999)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.StartDepthReconciler(ctx, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for h.queueDepth.Load() != 1 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("reconciler did not heal drift: depth %d, want 1", h.queueDepth.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartDepthReconciler did not return after context cancel")
	}
}

// A recount that cannot read staging keeps the last value rather than
// dropping backpressure to zero.
func TestReconcileQueueDepthKeepsValueWhenStagingUnreadable(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	h.queueDepth.Store(7)
	if err := os.RemoveAll(stagingDir); err != nil {
		t.Fatal(err)
	}

	if err := h.ReconcileQueueDepth(); err == nil {
		t.Fatal("expected an error for a missing staging dir")
	}
	if got := h.queueDepth.Load(); got != 7 {
		t.Errorf("depth after failed recount = %d, want 7 (unchanged)", got)
	}
}
