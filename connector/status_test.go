package connector

import (
	"encoding/json"
	"fmt"
	"io"
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
// connector.Run (while a binary waited) are the ones the framework serves.
func TestNewFramework_StatusCarriesOver(t *testing.T) {
	port := pickPort(t)
	st := NewStatus()
	st.Set("session", Condition{State: SessionStateUnknown})
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
	if fw.Status != st {
		t.Fatal("Framework.Status is not the Status passed in Options")
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
