package powerschool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WaitHealth is the stand-in health server for the time the binary spends
// waiting for a fresh session (WaitForFreshSession).
//
// That wait happens before connector.Run, so the framework's health server
// is not up yet. With nothing listening, the kubelet's liveness probe fails
// and the pod is killed and restarted into CrashLoopBackOff -- for a
// condition that is routine and that restarting cannot fix. WaitHealth
// answers on the same port so the pod sits Running and NotReady instead:
//
//	/healthz  200  the process is alive and doing what it should (waiting)
//	/readyz   503  it is not polling
//
// Stop must be called before connector.Run: the framework binds the same
// port. Stop closes the listener and returns only once the port is free.
type WaitHealth struct {
	srv *http.Server
	ln  net.Listener
}

// StartWaitHealth listens on addr (":8080", or "127.0.0.1:0" in tests) and
// serves the two endpoints.
func StartWaitHealth(addr string) (*WaitHealth, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("powerschool: wait health listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok: waiting for a fresh session\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: the session was rejected upstream; waiting for a new one\n"))
	})
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

// SessionClock keeps, in the connector's state directory, when the
// connector first saw the session it is running on and how old the last
// rejected session was. It exists to measure the portal's real session
// lifetime: the expiry in the session document is a placeholder.
//
// "Age" is time since this connector first loaded that session, which is a
// little less than time since login. Files hold a fingerprint and numbers,
// never a cookie. An empty Dir disables it.
type SessionClock struct {
	Dir string
}

func (c SessionClock) firstSeenPath() string   { return filepath.Join(c.Dir, "session-first-seen") }
func (c SessionClock) rejectedAgePath() string { return filepath.Join(c.Dir, "session-rejected-age") }

// FirstSeen returns when the session with this fingerprint was first seen,
// recording now if it has not been seen before. A disabled or unwritable
// clock returns now.
func (c SessionClock) FirstSeen(fingerprint string, now time.Time) time.Time {
	if c.Dir == "" || fingerprint == "" {
		return now
	}
	if raw, err := os.ReadFile(c.firstSeenPath()); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) == 2 && fields[0] == fingerprint {
			if t, err := time.Parse(time.RFC3339, fields[1]); err == nil && !t.After(now) {
				return t
			}
		}
	}
	_ = os.WriteFile(c.firstSeenPath(), []byte(fingerprint+" "+now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	return now
}

// SaveRejectedAge records how old the session was when it was rejected.
func (c SessionClock) SaveRejectedAge(age time.Duration) error {
	if c.Dir == "" || age < 0 {
		return nil
	}
	return os.WriteFile(c.rejectedAgePath(), []byte(strconv.FormatInt(int64(age/time.Second), 10)+"\n"), 0o600)
}

// LastRejectedAge returns the age recorded by SaveRejectedAge, if any.
func (c SessionClock) LastRejectedAge() (time.Duration, bool) {
	if c.Dir == "" {
		return 0, false
	}
	raw, err := os.ReadFile(c.rejectedAgePath())
	if err != nil {
		return 0, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}
