package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Readiness selects what a connector's /readyz reports.
//
// Readiness is a Deployment rollout condition: Helm's --wait and Flux's
// upgrade and rollback block on it. A pod that is NotReady for a condition
// that is not a fault -- a connector deliberately waiting, making no
// upstream requests, for somebody else to replace its session -- holds every
// rollout of the release hostage to that wait (glovebox-wekc.17). Such a
// state belongs in Status (a "degraded but ready" condition) and in the
// connector's metrics, not in readiness.
type Readiness int

const (
	// ReadyAfterFirstPoll is the default: /readyz answers 503 until a poll
	// has completed without error. Right for a connector whose first poll is
	// ordinary, bounded work.
	ReadyAfterFirstPoll Readiness = iota
	// ReadyWhenServing: /readyz answers 200 as soon as bootstrap has
	// succeeded (config valid, backend selected, Setup done) and the health
	// server is up. For connectors whose first poll may legitimately not
	// happen, or may block for a long time on something that is not a
	// fault, for example waiting for a new session. What the connector is
	// doing is reported through Status instead.
	ReadyWhenServing
)

// Condition is one named, non-fatal state a connector reports alongside
// readiness: "the session" is "waiting_for_new_session" since 09:12, say.
// A condition never makes the pod NotReady.
type Condition struct {
	// State is a short, bounded token (it is also a metric label value in
	// the connectors that export one).
	State string `json:"state"`
	// Since is when the condition entered State.
	Since time.Time `json:"since"`
	// Degraded marks a state in which the connector is healthy but not
	// doing its whole job (it is waiting rather than polling).
	Degraded bool `json:"degraded"`
	// Message is a human-readable explanation. Never a secret.
	Message string `json:"message,omitempty"`
}

// Status is the set of conditions a connector reports. It is served as JSON
// on /statusz by the framework's health server and by WaitHealth, and
// summarised in the /readyz body. The zero value is not usable; use
// NewStatus. All methods are safe on a nil *Status (they do nothing, or
// report nothing).
type Status struct {
	mu    sync.Mutex
	conds map[string]Condition
	now   func() time.Time
}

// NewStatus returns an empty Status.
func NewStatus() *Status {
	return &Status{conds: map[string]Condition{}, now: time.Now}
}

// Set records condition name. When c.Since is zero it is filled in: kept
// from the previous value if the state has not changed, otherwise now. It
// returns the condition as stored.
func (s *Status) Set(name string, c Condition) Condition {
	if s == nil {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Since.IsZero() {
		if prev, ok := s.conds[name]; ok && prev.State == c.State {
			c.Since = prev.Since
		} else {
			c.Since = s.now()
		}
	}
	c.Since = c.Since.UTC()
	s.conds[name] = c
	return c
}

// Get returns condition name.
func (s *Status) Get(name string) (Condition, bool) {
	if s == nil {
		return Condition{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conds[name]
	return c, ok
}

// Snapshot returns a copy of every condition.
func (s *Status) Snapshot() map[string]Condition {
	out := map[string]Condition{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.conds {
		out[k] = v
	}
	return out
}

// Degraded lists "name=state" for every degraded condition, sorted.
func (s *Status) Degraded() []string {
	var out []string
	for name, c := range s.Snapshot() {
		if c.Degraded {
			out = append(out, name+"="+c.State)
		}
	}
	sort.Strings(out)
	return out
}

// statusDocument is the /statusz body.
type statusDocument struct {
	Connector  string               `json:"connector"`
	Ready      bool                 `json:"ready"`
	Degraded   bool                 `json:"degraded"`
	Conditions map[string]Condition `json:"conditions"`
}

// readyzHandler answers 200 while ready() is true, 503 otherwise. A ready
// connector with degraded conditions still answers 200; the body names them.
func readyzHandler(ready func() bool, s *Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		if d := s.Degraded(); len(d) > 0 {
			_, _ = w.Write([]byte("ok (degraded: " + strings.Join(d, ", ") + ")"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}
}

// statuszHandler serves the Status as JSON.
func statuszHandler(name string, ready func() bool, s *Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc := statusDocument{
			Connector:  name,
			Ready:      ready(),
			Degraded:   len(s.Degraded()) > 0,
			Conditions: s.Snapshot(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// WaitHealth is a stand-in health server for the time a connector binary
// spends waiting, before connector.Run, for somebody else to replace a
// session the upstream already refused.
//
// connector.Run has not started, so the framework's health server is not up.
// With nothing listening the kubelet's liveness probe fails and the pod is
// restarted for a condition that is routine and that restarting cannot fix.
// WaitHealth answers on the same port:
//
//	/healthz  200  the process is alive
//	/readyz   200  it is healthy and doing what it should: waiting, by
//	               design, making no upstream requests. The body names the
//	               degraded conditions.
//	/statusz  JSON the conditions (what it is waiting for, since when)
//	/metrics       the process's Prometheus metrics (default registry)
//
// Waiting is not NotReady: readiness gates Helm upgrades and rollbacks of
// the whole release, and a wait that lasts until the next scheduled login
// must not block them (glovebox-wekc.17).
//
// Stop must be called before connector.Run: the framework binds the same
// port. Stop closes the listener and returns only once the port is free.
type WaitHealth struct {
	srv *http.Server
	ln  net.Listener
}

// StartWaitHealth listens on addr (":8080", or "127.0.0.1:0" in tests) and
// serves the endpoints above for connector name. status may be nil.
func StartWaitHealth(addr, name string, status *Status) (*WaitHealth, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s: wait health listen: %w", name, err)
	}
	ready := func() bool { return true }
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok: waiting for a new session\n"))
	})
	mux.HandleFunc("/readyz", readyzHandler(ready, status))
	mux.HandleFunc("/statusz", statuszHandler(name, ready, status))
	mux.Handle("/metrics", promhttp.Handler())
	h := &WaitHealth{
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		ln:  ln,
	}
	go func() { _ = h.srv.Serve(ln) }()
	return h, nil
}

// Addr returns the address the server is listening on.
func (h *WaitHealth) Addr() string {
	if h == nil || h.ln == nil {
		return ""
	}
	return h.ln.Addr().String()
}

// Stop shuts the server down and releases the port. Safe on a nil receiver
// and safe to call twice.
func (h *WaitHealth) Stop() error {
	if h == nil || h.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.srv.Shutdown(ctx)
	if err != nil {
		// A probe connection that would not drain: close it under it.
		err = h.srv.Close()
	}
	h.srv = nil
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
