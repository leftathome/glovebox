package main

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
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
	"github.com/prometheus/client_golang/prometheus"
)

// TestRunOptions_ReadyWhenServing: the binary hands connector.Run
// ReadyWhenServing and the Status the wait reported into, so neither the
// wait nor a long first poll holds readiness false (glovebox-wekc.17).
func TestRunOptions_ReadyWhenServing(t *testing.T) {
	status := connector.NewStatus()
	env := map[string]string{"GLOVEBOX_STATE_DIR": "/state", "GLOVEBOX_STAGING_DIR": "/staging"}
	o := runOptions(nil, "/etc/connector/config.json", status, func(k string) string { return env[k] })
	if o.Readiness != connector.ReadyWhenServing {
		t.Errorf("Readiness = %v, want ReadyWhenServing", o.Readiness)
	}
	if o.Status != status {
		t.Error("Status is not the one the wait reported into")
	}
	if o.Name != "powerschool" || o.Tier != connector.TierPersonal || o.StateDir != "/state" ||
		o.StagingDir != "/staging" || o.PollInterval != 30*24*time.Hour || o.Setup == nil {
		t.Errorf("options changed: %+v", o)
	}
}

func writeSession(t *testing.T, path, value string) {
	t.Helper()
	data, err := json.Marshal(pslib.SessionExport{
		Cookies:   []*http.Cookie{{Name: "JSESSIONID", Value: value}},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, url string) (int, string) {
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

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// waitUntilServing polls addr until the stand-in answers.
func waitUntilServing(t *testing.T, addr string) {
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

type statusDoc struct {
	Ready      bool                           `json:"ready"`
	Degraded   bool                           `json:"degraded"`
	Conditions map[string]connector.Condition `json:"conditions"`
}

// TestWaitForSession_ReadyAndCarriedAcrossRestart: the path without
// on-demand login. While waiting the stand-in is Ready and reports the wait
// from the refusal (the marker's time); a process killed during the wait
// and started again reports the same start; a new session ends the wait,
// reports "unknown" and frees the port.
func TestWaitForSession_ReadyAndCarriedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.json")
	writeSession(t, sessionFile, "synthetic-dead")
	getenv := func(k string) string {
		if k == "POWERSCHOOL_SESSION_FILE" {
			return sessionFile
		}
		return ""
	}
	load := func() (*pslib.SessionExport, error) { return powerschool.LoadSession(getenv, time.Now()) }
	dead, err := load()
	if err != nil {
		t.Fatal(err)
	}
	rejected := connector.NewRejectedSession(dir)
	if err := rejected.Mark(powerschool.SessionFingerprint(dead)); err != nil {
		t.Fatal(err)
	}
	refused := time.Now().Add(-20 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(rejected.Path, refused, refused); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, sessionStateFile)

	start := func() (string, *connector.SessionState, context.CancelFunc, chan error) {
		reg := prometheus.NewRegistry()
		status := connector.NewStatus()
		state, err := connector.NewSessionState(reg, "powerschool", status)
		if err != nil {
			t.Fatal(err)
		}
		state.PersistTo(statePath)
		addr := freePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := waitForSession(ctx, rejected, load, 5*time.Millisecond, addr, status, state, reg)
			done <- err
		}()
		waitUntilServing(t, addr)
		return addr, state, cancel, done
	}
	check := func(addr string) {
		t.Helper()
		if code, body := get(t, "http://"+addr+"/readyz"); code != http.StatusOK || !strings.Contains(body, "waiting_for_new_session") {
			t.Errorf("/readyz = %d %q, want 200 naming the wait", code, body)
		}
		_, body := get(t, "http://"+addr+"/statusz")
		var doc statusDoc
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("/statusz: %v", err)
		}
		c := doc.Conditions[connector.SessionStateCondition]
		if !doc.Ready || c.State != connector.SessionStateWaiting || !c.Since.Equal(refused.UTC()) {
			t.Errorf("/statusz = %+v, want ready, waiting since %s", doc, refused.UTC())
		}
		if _, m := get(t, "http://"+addr+"/metrics"); !strings.Contains(m, `powerschool_session_state{state="waiting_for_new_session"} 1`) {
			t.Error("/metrics does not show the wait")
		}
	}

	addr, state, cancel, done := start()
	check(addr)
	// The process is killed mid-wait.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v", err)
	}

	// ...and started again: same wait, same start.
	addr, state, cancel, done = start()
	defer cancel()
	check(addr)

	writeSession(t, sessionFile, "synthetic-fresh")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end when the session changed")
	}
	if cur, _ := state.Current(); cur != connector.SessionStateUnknown {
		t.Errorf("state after the wait = %q, want unknown", cur)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("health port still bound after the wait: %v", err)
	}
	ln.Close()
}
