package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// deliveryWritable probes that dir accepts a create-and-remove. It exists to
// catch the stale-bind-mount failure mode: glovebox delivers into a shared
// agents volume (cfg.AgentsDir) that the OpenClaw gateway also mounts. When
// that volume detaches/reattaches under a rolling peer, glovebox's existing
// mount can go stale and every delivery mkdir returns EIO while the process
// stays up and listening. A tcpSocket liveness probe cannot see this -- the
// socket is fine, only the filesystem is dead -- so deliveries silently stall
// until an operator notices. Actively touching the delivery path turns that
// invisible failure into a failed liveness probe, letting Kubernetes restart
// the pod onto a fresh mount. It runs in deliveryProber's loop, never in the
// /healthz request (see below).
//
// The original trigger was a ReadWriteOnce volume shared with the gateway,
// since moved to ReadWriteMany. This probe is deliberately kept: RWX narrows
// the window but does not make a mount immune to going stale, and the failure
// mode is silent either way.
//
// The probe file is a dotfile created directly under the agents root (not
// inside any agent's inbox) and removed immediately, so it never surfaces as a
// deliverable to a downstream agent.
func deliveryWritable(dir string) error {
	if dir == "" {
		return fmt.Errorf("agents dir not configured")
	}
	f, err := os.CreateTemp(dir, ".glovebox-healthz-")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// The delivery probe runs in its own loop, never in a /healthz request
// (glovebox-q0ch). The delivery volume is networked (Longhorn RWX, NFS):
// while a replica's disk stalls, one create-and-remove there can take
// seconds, and run in the request it made /healthz miss the kubelet's 1s
// timeout three times running and got a healthy core killed. A slow mount is
// not a dead process. So the loop probes every deliveryProbeEvery, one probe
// at a time (a hung mount never piles up goroutines), and /healthz reports
// the loop's state without touching the filesystem, the database or anything
// else that can block:
//
//   - a probe has been running longer than deliveryProbeStuckAfter (a mount
//     hung for good, as a dead NFS server leaves it): 503, so the pod is
//     restarted onto a fresh mount;
//   - the latest finished probe failed (EIO from a stale mount fails fast):
//     503, as before;
//   - no probe has started or finished for deliveryProbeStuckAfter plus one
//     interval (the loop stopped): 503;
//   - otherwise 200, including while a probe is merely slow.
//
// Dependency health is not liveness: the journal's database is reported by
// the journal's own /v1/healthz and glovebox_journal_ready, never here.
//
// Variables so tests can shorten them.
var (
	deliveryProbeEvery      = 10 * time.Second
	deliveryProbeStuckAfter = 2 * time.Minute
)

// deliveryProber holds the probe loop's state for livenessHandler.
type deliveryProber struct {
	dir        string
	probe      func(string) error
	every      time.Duration
	stuckAfter time.Duration
	now        func() time.Time

	mu      sync.Mutex
	running bool      // a probe is in flight
	since   time.Time // when it started; when the last one finished if none is in flight
	err     error     // the latest finished probe's result
}

func newDeliveryProber(dir string, probe func(string) error) *deliveryProber {
	return &deliveryProber{dir: dir, probe: probe, every: deliveryProbeEvery,
		stuckAfter: deliveryProbeStuckAfter, now: time.Now, since: time.Now()}
}

// Run probes now and then every interval until ctx ends. The probe runs on
// this goroutine, so there is never more than one in flight.
func (p *deliveryProber) Run(ctx context.Context) {
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		p.runOnce()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *deliveryProber) runOnce() {
	p.mu.Lock()
	p.running, p.since = true, p.now()
	p.mu.Unlock()
	err := p.probe(p.dir)
	p.mu.Lock()
	p.running, p.since, p.err = false, p.now(), err
	p.mu.Unlock()
}

// status is nil while the process counts as alive (see above).
func (p *deliveryProber) status() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	age := p.now().Sub(p.since)
	switch {
	case p.running && age > p.stuckAfter:
		return fmt.Errorf("delivery path %s: write probe stuck for %v", p.dir, age.Round(time.Second))
	case p.running:
		return nil
	case p.err != nil:
		return fmt.Errorf("delivery path %s not writable: %w", p.dir, p.err)
	case age > p.stuckAfter+p.every:
		return fmt.Errorf("delivery path %s: write probe loop not running for %v", p.dir, age.Round(time.Second))
	}
	return nil
}

// livenessHandler answers /healthz from p's state: 200 while the process is
// alive and its delivery path is not broken, 503 when it is (see above). It
// does no I/O, so it answers at once whatever the mount or the database is
// doing. Wire a Kubernetes livenessProbe httpGet at /healthz.
func livenessHandler(p *deliveryProber) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := p.status(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "%v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
}

// metricsMux is the metrics port's routes: /metrics, /healthz, /readyz.
// Each request has its own goroutine and the three share no lock, so a slow
// scrape cannot delay a probe; and no gauge reads the database at a scrape
// (the journal's database gauges are refreshed by their own loop,
// journalGaugeLoop, and a scrape reads the stored values).
func metricsMux(metrics http.Handler, p *deliveryProber, ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.HandleFunc("/healthz", livenessHandler(p))
	mux.HandleFunc("/readyz", readinessHandler(ready))
	return mux
}

// readinessHandler returns 200 once the daemon has finished startup (scanner
// pool, watcher and result consumer running) and 503 before then. Wire a
// readinessProbe httpGet at /readyz.
func readinessHandler(ready *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready\n")
	}
}
