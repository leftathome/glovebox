package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// gaugeValue reads one gauge sample from reg: name, and the state label
// when state is not empty.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name, state string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := state == ""
			for _, l := range m.GetLabel() {
				if l.GetName() == "state" && l.GetValue() == state {
					match = true
				}
			}
			if match {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("no sample %s{state=%q}", name, state)
	return 0
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type statusDoc struct {
	Connector  string               `json:"connector"`
	Ready      bool                 `json:"ready"`
	Degraded   bool                 `json:"degraded"`
	Conditions map[string]Condition `json:"conditions"`
}

// TestNewFramework_ReadyWhenServing_WaitingIsReady: a connector that opts
// into ReadyWhenServing answers /readyz 200 before any poll, even while it
// reports a degraded "waiting for a new session" condition; /statusz and
// the /readyz body carry the condition (glovebox-wekc.17).
func TestNewFramework_ReadyWhenServing_WaitingIsReady(t *testing.T) {
	port := pickPort(t)
	opts := frameworkTestOpts(t, "fw-ready-serving", port, &mockPollConnector{})
	opts.Readiness = ReadyWhenServing
	since := time.Date(2026, 10, 12, 9, 12, 0, 0, time.UTC)
	opts.Setup = func(cc ConnectorContext) error {
		if cc.Status == nil {
			return fmt.Errorf("ConnectorContext.Status is nil")
		}
		cc.Status.Set("session", Condition{State: SessionStateWaiting, Since: since, Degraded: true, Message: "waiting"})
		return nil
	}

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()
	waitForPort(t, port, 2*time.Second)

	code, body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	if code != http.StatusOK {
		t.Fatalf("/readyz before any poll = %d, want 200 with ReadyWhenServing", code)
	}
	if !strings.Contains(body, "degraded: session=waiting_for_new_session") {
		t.Errorf("/readyz body %q does not name the degraded condition", body)
	}

	code, body = getBody(t, fmt.Sprintf("http://127.0.0.1:%d/statusz", port))
	if code != http.StatusOK {
		t.Fatalf("/statusz = %d", code)
	}
	var doc statusDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("/statusz not JSON: %v (%q)", err, body)
	}
	c := doc.Conditions["session"]
	if doc.Connector != "fw-ready-serving" || !doc.Ready || !doc.Degraded ||
		c.State != SessionStateWaiting || !c.Since.Equal(since) {
		t.Errorf("/statusz = %+v", doc)
	}
}

// TestNewFramework_DefaultReadinessUnchanged: without the opt-in, /readyz
// is 503 until the first successful poll, as before; /statusz is served and
// reports not ready with no conditions.
func TestNewFramework_DefaultReadinessUnchanged(t *testing.T) {
	port := pickPort(t)
	opts := frameworkTestOpts(t, "fw-ready-default", port, &mockPollConnector{})
	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()
	waitForPort(t, port, 2*time.Second)

	if code, body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", port)); code != http.StatusServiceUnavailable || body != "not ready" {
		t.Errorf("/readyz = %d %q, want 503 \"not ready\"", code, body)
	}
	_, body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/statusz", port))
	var doc statusDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("/statusz not JSON: %v", err)
	}
	if doc.Ready || doc.Degraded || len(doc.Conditions) != 0 {
		t.Errorf("/statusz = %+v, want not ready, nothing degraded", doc)
	}
	fw.Ready.Store(true)
	if code, body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", port)); code != http.StatusOK || body != "ok" {
		t.Errorf("/readyz after ready = %d %q, want 200 \"ok\"", code, body)
	}
}

// TestNewFramework_StatusCarriesOver: conditions recorded before
// connector.Run (while a binary waited) are the ones the framework serves,
// and later changes through the same Status show up there too.
func TestNewFramework_StatusCarriesOver(t *testing.T) {
	port := pickPort(t)
	st := NewStatus()
	began := time.Date(2026, 10, 12, 9, 12, 0, 0, time.UTC)
	st.Set("session", Condition{State: SessionStateWaiting, Since: began, Degraded: true})
	opts := frameworkTestOpts(t, "fw-status-carry", port, &mockPollConnector{})
	opts.Status = st
	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()
	// Let the health server bind before Shutdown, so no listener outlives
	// the test and collides with another test's port.
	waitForPort(t, port, 2*time.Second)
	statusz := func() statusDoc {
		t.Helper()
		_, body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/statusz", port))
		var doc statusDoc
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("/statusz not JSON: %v", err)
		}
		return doc
	}
	doc := statusz()
	if c := doc.Conditions["session"]; c.State != SessionStateWaiting || !c.Since.Equal(began) || !doc.Degraded {
		t.Errorf("/statusz = %+v, want the pre-run waiting condition since %s", doc, began)
	}
	st.Set("session", Condition{State: SessionStateUsable})
	if doc := statusz(); doc.Conditions["session"].State != SessionStateUsable || doc.Degraded {
		t.Errorf("/statusz after a change = %+v, want usable, not degraded", doc)
	}
}

// TestSessionState_PersistsTheWaitAcrossRestarts: a waiting state, and when
// it began, survive a restart (a new SessionState on the same file); other
// states are not restored.
func TestSessionState_PersistsTheWaitAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-state")
	ss, _ := NewSessionState(prometheus.NewRegistry(), "p1", nil)
	ss.PersistTo(path)
	began := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	ss.Set(SessionStateWaiting, began, "waiting")

	st := NewStatus()
	reg := prometheus.NewRegistry()
	again, _ := NewSessionState(reg, "p1", st)
	again.PersistTo(path)
	if cur, at := again.Current(); cur != SessionStateWaiting || !at.Equal(began) {
		t.Fatalf("after restart: %q since %s, want waiting since %s", cur, at, began)
	}
	if c, _ := st.Get(SessionStateCondition); !c.Degraded || !c.Since.Equal(began) {
		t.Errorf("restored condition = %+v", c)
	}
	if v := gaugeValue(t, reg, "p1_session_state_since_timestamp_seconds", SessionStateWaiting); v != float64(began.Unix()) {
		t.Errorf("restored since gauge = %v", v)
	}
	// Still waiting at the next check: the start does not move.
	again.Set(SessionStateWaiting, time.Time{}, "still")
	if _, at := again.Current(); !at.Equal(began) {
		t.Errorf("since moved to %s", at)
	}

	// Usable is written but not restored.
	again.Set(SessionStateUsable, time.Time{}, "ok")
	third, _ := NewSessionState(prometheus.NewRegistry(), "p1", nil)
	third.PersistTo(path)
	if cur, _ := third.Current(); cur != SessionStateUnknown {
		t.Errorf("restored %q, want unknown (only a wait is restored)", cur)
	}

	// A garbled or future file restores nothing.
	for _, content := range []string{"garbage", SessionStateWaiting + " not-a-time", SessionStateWaiting + " " + time.Now().Add(time.Hour).Format(time.RFC3339Nano)} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		ss, _ := NewSessionState(prometheus.NewRegistry(), "p1", nil)
		ss.PersistTo(path)
		if cur, _ := ss.Current(); cur != SessionStateUnknown {
			t.Errorf("file %q restored %q", content, cur)
		}
	}
}

// TestWaitHealth_ListenerPortAnswers503: a Listener connector waiting for a
// session is Ready, so its trigger port is in the Service; the stand-in
// answers there 503 with Retry-After instead of refusing the connection,
// and frees the port on Stop.
func TestWaitHealth_ListenerPortAnswers503(t *testing.T) {
	reg := prometheus.NewRegistry()
	h, err := StartWaitHealthWith(WaitHealthOptions{
		Addr: "127.0.0.1:0", Name: "lst", Gatherer: reg,
		UnavailableAddr: "127.0.0.1:0", UnavailableMessage: "waiting for a session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := getBody(t, "http://"+h.Addr()+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d, want 200", code)
	}
	resp, err := http.Post("http://"+h.UnavailableAddr()+"/v1/poll", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST listener port: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(string(b), "waiting for a session") {
		t.Errorf("listener port answered %d %q (Retry-After %q)", resp.StatusCode, b, resp.Header.Get("Retry-After"))
	}
	addrs := []string{h.Addr(), h.UnavailableAddr()}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Errorf("%s still bound after Stop: %v", a, err)
			continue
		}
		ln.Close()
	}
	// A listener port that is taken fails the start and leaves nothing bound.
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if h, err := StartWaitHealthWith(WaitHealthOptions{Addr: "127.0.0.1:0", UnavailableAddr: busy.Addr().String()}); err == nil {
		h.Stop()
		t.Error("start succeeded with the listener port in use")
	}
}

func TestStatus_SetKeepsSinceForSameState(t *testing.T) {
	st := NewStatus()
	t0 := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return t0 }
	st.Set("session", Condition{State: "a"})
	st.now = func() time.Time { return t0.Add(time.Hour) }
	if c := st.Set("session", Condition{State: "a"}); !c.Since.Equal(t0) {
		t.Errorf("same state: since = %s, want %s", c.Since, t0)
	}
	if c := st.Set("session", Condition{State: "b"}); !c.Since.Equal(t0.Add(time.Hour)) {
		t.Errorf("new state: since = %s, want now", c.Since)
	}
	var none *Status
	none.Set("x", Condition{State: "y"})
	if _, ok := none.Get("x"); ok || len(none.Degraded()) != 0 {
		t.Error("nil Status reported something")
	}
}

func TestSessionState_GaugesAndCondition(t *testing.T) {
	reg := prometheus.NewRegistry()
	st := NewStatus()
	ss, err := NewSessionState(reg, "testconn", st)
	if err != nil {
		t.Fatalf("NewSessionState: %v", err)
	}
	if cur, _ := ss.Current(); cur != SessionStateUnknown {
		t.Errorf("initial state %q, want unknown", cur)
	}
	since := time.Date(2026, 10, 12, 9, 12, 0, 0, time.UTC)
	ss.Set(SessionStateWaiting, since, "waiting")
	if v := gaugeValue(t, reg, "testconn_session_state", SessionStateWaiting); v != 1 {
		t.Errorf("waiting gauge = %v, want 1", v)
	}
	if v := gaugeValue(t, reg, "testconn_session_state", SessionStateUsable); v != 0 {
		t.Errorf("usable gauge = %v, want 0", v)
	}
	if v := gaugeValue(t, reg, "testconn_session_state_since_timestamp_seconds", SessionStateWaiting); v != float64(since.Unix()) {
		t.Errorf("since gauge = %v, want %d", v, since.Unix())
	}
	c, ok := st.Get(SessionStateCondition)
	if !ok || c.State != SessionStateWaiting || !c.Degraded || !c.Since.Equal(since) {
		t.Errorf("condition = %+v", c)
	}
	// Same state again with no time: the wait's start is kept.
	ss.Set(SessionStateWaiting, time.Time{}, "still waiting")
	if _, at := ss.Current(); !at.Equal(since) {
		t.Errorf("since moved to %s on an unchanged state", at)
	}
	ss.Set(SessionStateUsable, time.Time{}, "ok")
	if c, _ := st.Get(SessionStateCondition); c.Degraded || c.State != SessionStateUsable {
		t.Errorf("usable condition = %+v", c)
	}
	if _, err := NewSessionState(reg, "testconn", nil); err == nil {
		t.Error("registering the same prefix twice succeeded")
	}
	var none *SessionState
	none.Set(SessionStateUsable, time.Time{}, "")
}

func TestRejectedSession_MarkedAt(t *testing.T) {
	r := NewRejectedSession(t.TempDir())
	if _, ok := r.MarkedAt(); ok {
		t.Error("MarkedAt without a marker")
	}
	if err := r.Mark("fp"); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 12, 9, 12, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Clean(r.Path), when, when); err != nil {
		t.Fatal(err)
	}
	if at, ok := r.MarkedAt(); !ok || !at.Equal(when) {
		t.Errorf("MarkedAt = %s, %v; want %s", at, ok, when)
	}
	if _, ok := (RejectedSession{}).MarkedAt(); ok {
		t.Error("disabled marker reported a time")
	}
}

func TestWaitHealth_ReadyAndServesStatus(t *testing.T) {
	st := NewStatus()
	st.Set("session", Condition{State: SessionStateWaiting, Degraded: true})
	h, err := StartWaitHealth("127.0.0.1:0", "waiter", st)
	if err != nil {
		t.Fatalf("StartWaitHealth: %v", err)
	}
	defer h.Stop()
	base := "http://" + h.Addr()
	if code, _ := getBody(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d", code)
	}
	if code, body := getBody(t, base+"/readyz"); code != http.StatusOK || !strings.Contains(body, "session=waiting_for_new_session") {
		t.Errorf("/readyz = %d %q, want 200 naming the degraded session", code, body)
	}
	_, body := getBody(t, base+"/statusz")
	var doc statusDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil || doc.Connector != "waiter" || !doc.Ready || !doc.Degraded {
		t.Errorf("/statusz = %q (%v)", body, err)
	}
	if code, _ := getBody(t, base+"/metrics"); code != http.StatusOK {
		t.Errorf("/metrics = %d", code)
	}
}
