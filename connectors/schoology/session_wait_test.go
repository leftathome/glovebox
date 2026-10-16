package schoology

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologyauth "github.com/leftathome/schoology-go/auth"
	"github.com/prometheus/client_golang/prometheus"
)

func waitGet(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

func waitServing(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

// TestWaitForFreshCredentials_ReadyListener503AndRestart: while Schoology's
// refused session is in place the binary waits, contacting nothing. The
// stand-in is alive and Ready, answers the trigger port 503 (the pod is in
// the Service's endpoints), and reports the wait from the refusal; a
// process killed mid-wait and started again reports the same start; new
// credentials end the wait and free both ports.
func TestWaitForFreshCredentials_ReadyListener503AndRestart(t *testing.T) {
	dir := t.TempDir()
	rejected := NewRejectedSession(dir)
	if err := rejected.Mark(SessionFingerprint("synthetic-dead")); err != nil {
		t.Fatal(err)
	}
	refused := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(rejected.Path, refused, refused); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := &schoologyauth.Credentials{SessID: "synthetic-dead"}
	load := func() (*schoologyauth.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		c := *current
		return &c, nil
	}
	statePath := filepath.Join(dir, "session-state")

	type run struct {
		health, listener string
		state            *connector.SessionState
		cancel           context.CancelFunc
		done             chan error
		creds            chan *schoologyauth.Credentials
	}
	start := func() run {
		reg := prometheus.NewRegistry()
		status := connector.NewStatus()
		state, err := connector.NewSessionState(reg, "schoology", status)
		if err != nil {
			t.Fatal(err)
		}
		state.PersistTo(statePath)
		r := run{health: waitFreeAddr(t), listener: waitFreeAddr(t), state: state,
			done: make(chan error, 1), creds: make(chan *schoologyauth.Credentials, 1)}
		var ctx context.Context
		ctx, r.cancel = context.WithCancel(context.Background())
		first, _ := load()
		go func() {
			c, err := WaitForFreshCredentials(ctx, rejected, first, load, SessionWaitOptions{
				HealthAddr: r.health, ListenerAddr: r.listener, Interval: 5 * time.Millisecond,
				Status: status, State: state, Gatherer: reg,
			})
			r.creds <- c
			r.done <- err
		}()
		waitServing(t, r.health)
		waitServing(t, r.listener)
		return r
	}
	check := func(r run) {
		t.Helper()
		if code, _ := waitGet(t, "http://"+r.health+"/healthz"); code != http.StatusOK {
			t.Errorf("/healthz = %d", code)
		}
		if code, body := waitGet(t, "http://"+r.health+"/readyz"); code != http.StatusOK || !strings.Contains(body, "waiting_for_new_session") {
			t.Errorf("/readyz = %d %q, want 200 naming the wait", code, body)
		}
		if code, body := waitGet(t, "http://"+r.listener+"/v1/poll"); code != http.StatusServiceUnavailable || !strings.Contains(body, "waiting for a new Schoology session") {
			t.Errorf("trigger port = %d %q, want 503 waiting for a session", code, body)
		}
		var doc struct {
			Conditions map[string]connector.Condition `json:"conditions"`
		}
		_, body := waitGet(t, "http://"+r.health+"/statusz")
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatal(err)
		}
		if c := doc.Conditions["session"]; c.State != connector.SessionStateWaiting || !c.Since.Equal(refused.UTC()) {
			t.Errorf("session condition %+v, want waiting since %s", c, refused.UTC())
		}
		if _, m := waitGet(t, "http://"+r.health+"/metrics"); !strings.Contains(m, `schoology_session_state{state="waiting_for_new_session"} 1`) {
			t.Error("/metrics does not show the wait")
		}
	}

	r := start()
	check(r)
	r.cancel()
	if err := <-r.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}

	r = start()
	defer r.cancel()
	check(r)
	mu.Lock()
	current = &schoologyauth.Credentials{SessID: "synthetic-fresh"}
	mu.Unlock()
	select {
	case c := <-r.creds:
		if err := <-r.done; err != nil || c.SessID != "synthetic-fresh" {
			t.Fatalf("wait ended with %+v, %v", c, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end")
	}
	if cur, _ := r.state.Current(); cur != connector.SessionStateUnknown {
		t.Errorf("state after the wait %q, want unknown", cur)
	}
	for _, a := range []string{r.health, r.listener} {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Errorf("%s still bound: %v", a, err)
			continue
		}
		ln.Close()
	}
}

// TestWaitForFreshCredentials_NoWaitBindsNothing: a session that was not
// refused passes straight through; nothing is bound, the state untouched.
func TestWaitForFreshCredentials_NoWaitBindsNothing(t *testing.T) {
	state, _ := connector.NewSessionState(prometheus.NewRegistry(), "schoology", nil)
	health := waitFreeAddr(t)
	creds := &schoologyauth.Credentials{SessID: "synthetic-ok"}
	got, err := WaitForFreshCredentials(context.Background(), NewRejectedSession(t.TempDir()), creds,
		func() (*schoologyauth.Credentials, error) { return creds, nil },
		SessionWaitOptions{HealthAddr: health, Interval: time.Millisecond, State: state})
	if err != nil || got != creds {
		t.Fatalf("got %+v, %v", got, err)
	}
	if cur, _ := state.Current(); cur != connector.SessionStateUnknown {
		t.Errorf("state %q", cur)
	}
	if c, err := net.DialTimeout("tcp", health, 100*time.Millisecond); err == nil {
		c.Close()
		t.Error("something is listening on the health port")
	}
}
