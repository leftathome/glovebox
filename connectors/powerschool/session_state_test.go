package powerschool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
	"github.com/prometheus/client_golang/prometheus"
)

func newTestSessionState(t *testing.T) (*connector.SessionState, *connector.Status) {
	t.Helper()
	st := connector.NewStatus()
	ss, err := connector.NewSessionState(prometheus.NewRegistry(), "powerschool", st)
	if err != nil {
		t.Fatalf("NewSessionState: %v", err)
	}
	return ss, st
}

// TestReportSessionState maps every gate answer onto the reported state.
// It only reports; the gate's answer is what decides the poll.
func TestReportSessionState(t *testing.T) {
	for _, tc := range []struct {
		res  GateResult
		want string
	}{
		{GateResult{Outcome: GateLive}, connector.SessionStateUsable},
		{GateResult{Outcome: GateDead, Reason: ReasonSignInPage}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateBlocked, Reason: "login_latched:credentials_rejected"}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateBlocked, Reason: "login_backoff"}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateSkip, Reason: "login_deferred:blackout"}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateSkip, Reason: "blackout"}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateSkip, Reason: "no_new_session", Failed: true}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateSkip, Reason: "login_broker_unreachable", Failed: true}, connector.SessionStateWaiting},
		{GateResult{Outcome: GateSkip, Reason: "session_state_unknown:http_502"}, connector.SessionStateUnknown},
	} {
		ss, st := newTestSessionState(t)
		ss.Set(connector.SessionStateUsable, time.Time{}, "start")
		c := &PowerSchoolConnector{SessionState: ss}
		c.reportSessionState(tc.res)
		cur, _ := ss.Current()
		if cur != tc.want {
			t.Errorf("%+v: state %q, want %q", tc.res, cur, tc.want)
		}
		cond, _ := st.Get(connector.SessionStateCondition)
		if cond.Degraded != (tc.want == connector.SessionStateWaiting) {
			t.Errorf("%+v: degraded = %v", tc.res, cond.Degraded)
		}
	}
	// An interruption teaches nothing.
	ss, _ := newTestSessionState(t)
	ss.Set(connector.SessionStateUsable, time.Time{}, "start")
	(&PowerSchoolConnector{SessionState: ss}).reportSessionState(GateResult{Outcome: GateSkip, Reason: "interrupted"})
	if cur, _ := ss.Current(); cur != connector.SessionStateUsable {
		t.Errorf("interrupted moved the state to %q", cur)
	}
	// No SessionState: nothing to do, nothing panics.
	(&PowerSchoolConnector{}).reportSessionState(GateResult{Outcome: GateLive})
}

// TestBrokerLatchReportsWaitingThenUsable: through real polls, a latched
// broker leaves the connector waiting for a new session -- the process up,
// the portal untouched, the state observable -- and the first live session
// makes it usable again. The time the wait began does not move while it
// lasts.
func TestBrokerLatchReportsWaitingThenUsable(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	ss, st := newTestSessionState(t)
	c.SessionState = ss
	r.file = r.session("old", SessionDead, ReasonSignInPage)

	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeLatched, Code: brokerapi.CodeCredentialsRejected})
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("poll: %v", err)
	}
	cur, began := ss.Current()
	if cur != connector.SessionStateWaiting {
		t.Fatalf("after a latched answer: state %q, want waiting_for_new_session", cur)
	}
	if cond, _ := st.Get(connector.SessionStateCondition); !cond.Degraded {
		t.Error("waiting is not reported as degraded")
	}

	r.nextWindow(9 * time.Hour)
	r.file = testSession("old", r.clock.Add(24*time.Hour))
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeLatched, Code: brokerapi.CodeCredentialsRejected})
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if cur, at := ss.Current(); cur != connector.SessionStateWaiting || !at.Equal(began) {
		t.Errorf("still waiting: state %q since %s, want waiting since %s", cur, at, began)
	}

	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("new", SessionLive, ReasonSignedIn)))
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := ss.Current(); cur != connector.SessionStateUsable {
		t.Errorf("after a live session: state %q, want usable", cur)
	}
	if cond, _ := st.Get(connector.SessionStateCondition); cond.Degraded {
		t.Error("a usable session is still reported as degraded")
	}
}

// restartState is what a restarted process does: a new SessionState on the
// same state file.
func restartState(t *testing.T, path string) (*connector.SessionState, *connector.Status) {
	t.Helper()
	ss, st := newTestSessionState(t)
	ss.PersistTo(path)
	return ss, st
}

// TestBrokerModeWaitSurvivesRestart: with a login broker the process does
// not exit while it waits, but a values change, an OOM kill or an eviction
// restarts it. The restarted process reports the wait from its original
// start, before any poll, and the next poll does not move it.
func TestBrokerModeWaitSurvivesRestart(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	path := filepath.Join(t.TempDir(), "session-state")
	c.SessionState, _ = restartState(t, path)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeLatched, Code: brokerapi.CodeCredentialsRejected})
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	cur, began := c.SessionState.Current()
	if cur != connector.SessionStateWaiting {
		t.Fatalf("state %q, want waiting", cur)
	}

	// Restart.
	time.Sleep(10 * time.Millisecond)
	r.restart()
	c.Gate = r.gate
	ss, st := restartState(t, path)
	c.SessionState = ss
	if cur, at := ss.Current(); cur != connector.SessionStateWaiting || !at.Equal(began) {
		t.Fatalf("restarted process reports %q since %s before any poll, want waiting since %s", cur, at, began)
	}
	if cond, _ := st.Get(connector.SessionStateCondition); !cond.Degraded {
		t.Error("restored wait is not degraded")
	}
	r.nextWindow(9 * time.Hour)
	r.file = testSession("old", r.clock.Add(24*time.Hour))
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeLatched, Code: brokerapi.CodeCredentialsRejected})
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if cur, at := ss.Current(); cur != connector.SessionStateWaiting || !at.Equal(began) {
		t.Errorf("after the first poll of the restarted process: %q since %s, want waiting since %s", cur, at, began)
	}
}

// TestMailboxModeWaitSurvivesRestart: the same with the request mailbox.
func TestMailboxModeWaitSurvivesRestart(t *testing.T) {
	r := newGateRig(t, true)
	cfg := windowConfig()
	cfg.LoginOnDemand = r.gate.onDemand
	cfg.FailureThreshold = 0
	c, cp, _ := newWiredConnector(t, pollFixture("111111"), cfg)
	c.client = nil
	c.Gate = r.gate
	c.now = func() time.Time { return r.clock }
	r.cp = cp
	path := filepath.Join(t.TempDir(), "session-state")
	c.SessionState, _ = restartState(t, path)

	// Dead; a login is asked for and no session arrives within the wait.
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	cur, began := c.SessionState.Current()
	if cur != connector.SessionStateWaiting {
		t.Fatalf("state %q, want waiting", cur)
	}

	time.Sleep(10 * time.Millisecond)
	ss, _ := restartState(t, path)
	c.SessionState = ss
	if cur, at := ss.Current(); cur != connector.SessionStateWaiting || !at.Equal(began) {
		t.Fatalf("restarted process reports %q since %s, want waiting since %s", cur, at, began)
	}
	// The next poll asks again, gets nothing, and the mailbox stops asking:
	// a permanent error, on which the process exits -- another restart.
	r.nextWindow(time.Hour)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil && !connector.IsPermanent(err) {
		t.Fatal(err)
	}
	if cur, at := ss.Current(); cur != connector.SessionStateWaiting || !at.Equal(began) {
		t.Errorf("after the next poll: %q since %s, want waiting since %s", cur, at, began)
	}
	ss, _ = restartState(t, path)
	c.SessionState = ss
	if cur, at := ss.Current(); cur != connector.SessionStateWaiting || !at.Equal(began) {
		t.Errorf("after the second restart: %q since %s, want waiting since %s", cur, at, began)
	}
	// A session the portal accepts ends it.
	r.nextWindow(9 * time.Hour)
	r.file = r.session("new", SessionLive, ReasonSignedIn)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := ss.Current(); cur != connector.SessionStateUsable {
		t.Errorf("state %q after a live session, want usable", cur)
	}
}
