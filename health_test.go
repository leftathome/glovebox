package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func healthzCode(p *deliveryProber) (int, string) {
	rr := httptest.NewRecorder()
	livenessHandler(p)(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return rr.Code, rr.Body.String()
}

func TestLivenessHandler(t *testing.T) {
	t.Run("writable delivery path -> 200", func(t *testing.T) {
		p := newDeliveryProber(t.TempDir(), deliveryWritable)
		p.runOnce()
		if code, body := healthzCode(p); code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", code, body)
		}
	})

	t.Run("broken delivery path -> 503", func(t *testing.T) {
		p := newDeliveryProber(filepath.Join(t.TempDir(), "missing"), deliveryWritable)
		p.runOnce()
		code, body := healthzCode(p)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "not writable") {
			t.Fatalf("status = %d body %q, want 503 not writable", code, body)
		}
	})
}

// fakeClock is a settable clock for deliveryProber.now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// The liveness verdict, state by state, on a fake clock: slow is alive,
// stuck past the threshold is not, a failed write is not, a stopped loop is
// not, and a later good probe brings it back.
func TestDeliveryProberStatus(t *testing.T) {
	const every, stuck = 10 * time.Second, 2 * time.Minute
	newP := func(probe func(string) error) (*deliveryProber, *fakeClock) {
		clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
		p := newDeliveryProber("/data/agents", probe)
		p.every, p.stuckAfter, p.now, p.since = every, stuck, clk.now, clk.now()
		return p, clk
	}
	want := func(t *testing.T, p *deliveryProber, code int, contains string) {
		t.Helper()
		got, body := healthzCode(p)
		if got != code || !strings.Contains(body, contains) {
			t.Fatalf("status %d body %q, want %d containing %q", got, body, code, contains)
		}
	}

	t.Run("before the first probe: alive", func(t *testing.T) {
		p, clk := newP(nil)
		clk.add(stuck)
		want(t, p, http.StatusOK, "ok")
	})

	t.Run("in flight up to the threshold: alive; past it: stuck", func(t *testing.T) {
		p, clk := newP(nil)
		p.running = true
		clk.add(stuck)
		want(t, p, http.StatusOK, "ok")
		clk.add(time.Second)
		want(t, p, http.StatusServiceUnavailable, "write probe stuck for 2m1s")
	})

	t.Run("a failed write: 503 until a good one", func(t *testing.T) {
		fail := true
		p, _ := newP(func(string) error {
			if fail {
				return errors.New("input/output error")
			}
			return nil
		})
		p.runOnce()
		want(t, p, http.StatusServiceUnavailable, "not writable: input/output error")
		fail = false
		p.runOnce()
		want(t, p, http.StatusOK, "ok")
	})

	t.Run("a failed write while the next probe is in flight: alive until stuck", func(t *testing.T) {
		p, clk := newP(nil)
		p.err, p.running = errors.New("input/output error"), true
		clk.add(time.Second)
		want(t, p, http.StatusOK, "ok")
	})

	t.Run("no probe for longer than threshold plus interval: loop not running", func(t *testing.T) {
		p, clk := newP(func(string) error { return nil })
		p.runOnce()
		clk.add(stuck + every)
		want(t, p, http.StatusOK, "ok")
		clk.add(time.Second)
		want(t, p, http.StatusServiceUnavailable, "write probe loop not running")
	})
}

// Run probes at once and then every interval, one probe at a time even when
// each takes longer than the interval, and stops with its context.
func TestDeliveryProberRun(t *testing.T) {
	var mu sync.Mutex
	calls, running, overlap := 0, 0, false
	p := newDeliveryProber(t.TempDir(), func(string) error {
		mu.Lock()
		calls++
		running++
		overlap = overlap || running > 1
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // three intervals
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	})
	p.every = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("probe ran %d times in 200ms at a 10ms interval, want several", calls)
	}
	if overlap {
		t.Fatal("two probes ran at once")
	}
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
