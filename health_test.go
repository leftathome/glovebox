package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestDeliveryWritable(t *testing.T) {
	t.Run("writable dir returns nil", func(t *testing.T) {
		if err := deliveryWritable(t.TempDir()); err != nil {
			t.Fatalf("deliveryWritable(tmp) = %v, want nil", err)
		}
	})

	t.Run("leaves no probe file behind", func(t *testing.T) {
		dir := t.TempDir()
		if err := deliveryWritable(dir); err != nil {
			t.Fatalf("deliveryWritable: %v", err)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, ".glovebox-healthz-*"))
		if len(matches) != 0 {
			t.Fatalf("probe file left behind: %v", matches)
		}
	})

	t.Run("nonexistent dir returns error", func(t *testing.T) {
		if err := deliveryWritable(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
			t.Fatal("deliveryWritable(missing) = nil, want error")
		}
	})

	t.Run("empty dir returns error", func(t *testing.T) {
		if err := deliveryWritable(""); err == nil {
			t.Fatal("deliveryWritable(\"\") = nil, want error (must not fall back to os.TempDir)")
		}
	})
}

func TestLivenessHandler(t *testing.T) {
	t.Run("writable delivery path -> 200", func(t *testing.T) {
		rr := httptest.NewRecorder()
		livenessHandler(t.TempDir())(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
		}
	})

	t.Run("broken delivery path -> 503", func(t *testing.T) {
		rr := httptest.NewRecorder()
		livenessHandler(filepath.Join(t.TempDir(), "missing"))(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rr.Code)
		}
	})
}

func TestReadinessHandler(t *testing.T) {
	ready := &atomic.Bool{}
	h := readinessHandler(ready)

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("before ready: status = %d, want 503", rr.Code)
	}

	ready.Store(true)
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("after ready: status = %d, want 200", rr.Code)
	}
}
