package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// probeBudget is how long /healthz may take to answer: well inside the
// chart's liveness timeoutSeconds (1s before 0.13.2), with room for -race.
const probeBudget = 200 * time.Millisecond

// blockingProbe is a delivery probe that hangs the way a create on the
// RWX delivery volume hangs while a Longhorn replica's disk stalls (seconds
// at a time in production): it returns only when released or after max.
type blockingProbe struct {
	release chan struct{}
	once    sync.Once
	max     time.Duration
	mu      sync.Mutex
	calls   int
	running int
	overlap bool
}

func newBlockingProbe(max time.Duration) *blockingProbe {
	return &blockingProbe{release: make(chan struct{}), max: max}
}

func (b *blockingProbe) probe(string) error {
	b.mu.Lock()
	b.calls++
	b.running++
	if b.running > 1 {
		b.overlap = true
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.running--
		b.mu.Unlock()
	}()
	select {
	case <-b.release:
	case <-time.After(b.max):
	}
	return nil
}

func (b *blockingProbe) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *blockingProbe) stats() (calls int, overlap bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.overlap
}

// timedGet fetches url and returns the status and how long it took; a
// request that has not answered within 5s counts as not answering at all.
func timedGet(t *testing.T, url string) (int, time.Duration) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := c.Get(url)
	d := time.Since(start)
	if err != nil {
		return 0, d
	}
	resp.Body.Close()
	return resp.StatusCode, d
}

// glovebox-q0ch: the kubelet killed the core after three /healthz probes
// timed out (1s) while the delivery volume's writes took seconds. /metrics
// and /readyz on the same server answered in milliseconds the whole time;
// only the handler that wrote to the mount was stuck. A slow mount is not
// a dead process: /healthz must answer within the probe budget, and say
// the process is alive, however long the delivery probe takes.
func TestHealthzAnswersWhileDeliveryMountIsSlow(t *testing.T) {
	bp := newBlockingProbe(3 * time.Second)
	defer bp.unblock()
	srv := httptest.NewServer(newTestHealthz(t, t.TempDir(), bp.probe))
	defer srv.Close()

	// Five probes over about a second, all while the probe is stuck.
	for i := 0; i < 5; i++ {
		code, d := timedGet(t, srv.URL+"/healthz")
		if code != http.StatusOK || d > probeBudget {
			t.Fatalf("probe %d while the delivery mount is slow: status %d after %v, want 200 within %v",
				i, code, d.Round(time.Millisecond), probeBudget)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, overlap := bp.stats(); overlap {
		t.Fatal("a second delivery probe started while one was still stuck")
	}
}

// newTestHealthz serves /healthz as main.go wires it.
func newTestHealthz(t *testing.T, dir string, probe func(string) error) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", livenessHandler(dir, probe))
	return mux
}
