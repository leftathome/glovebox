package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
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
// the pod onto a fresh mount.
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

// livenessHandler returns 200 while the delivery path is writable and 503 when
// it is not. Wire a Kubernetes livenessProbe httpGet at /healthz so a stuck
// delivery mount triggers an automatic pod restart instead of a silent stall.
func livenessHandler(agentsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deliveryWritable(agentsDir); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "delivery path %s not writable: %v\n", agentsDir, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
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
