package powerschool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
	pslib "github.com/leftathome/powerschool-go"
)

// fakeBroker stands in for the login broker: it records what it was asked
// and answers with whatever the test queued.
type fakeBroker struct {
	t       *testing.T
	calls   []brokerCall
	answers []func() (*brokerapi.Response, error)
}

type brokerCall struct{ reason, fingerprint string }

func (f *fakeBroker) Refresh(_ context.Context, reason, fullFingerprint string) (*brokerapi.Response, error) {
	f.calls = append(f.calls, brokerCall{reason, fullFingerprint})
	if len(f.answers) == 0 {
		f.t.Fatalf("the broker was asked %d times; the test queued fewer answers", len(f.calls))
	}
	next := f.answers[0]
	f.answers = f.answers[1:]
	return next()
}

func (f *fakeBroker) answer(resp *brokerapi.Response) {
	f.answers = append(f.answers, func() (*brokerapi.Response, error) { return resp, nil })
}

func (f *fakeBroker) fail(err error) {
	f.answers = append(f.answers, func() (*brokerapi.Response, error) { return nil, err })
}

// sessionAnswer is what the broker sends with a session: the document under
// SessionField, as its PowerSchool driver writes it.
func sessionAnswer(t *testing.T, outcome string, s *pslib.SessionExport) *brokerapi.Response {
	t.Helper()
	doc, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return &brokerapi.Response{
		Site: "powerschool", Outcome: outcome,
		Session:     map[string]string{SessionField: string(doc), "logged_in_at": "2026-10-12T13:10:00Z"},
		Fingerprint: brokerapi.ShortFingerprint(SessionFingerprint(s)),
	}
}

// newBrokerRig is a gateRig whose gate asks a fakeBroker instead of the
// request mailbox.
func newBrokerRig(t *testing.T) (*gateRig, *fakeBroker) {
	t.Helper()
	r := newGateRig(t, false)
	b := &fakeBroker{t: t}
	cfg := LoginOnDemandConfig{Enabled: true, Broker: LoginBrokerConfig{
		URL: "http://glovebox-login-broker-powerschool:8080", TokenFile: "/var/run/secrets/login-broker/token"}}
	g := NewBrokerSessionGate(r.gate.source, cfg, b)
	g.Now, g.Sleep, g.OnDead = r.gate.Now, r.gate.Sleep, r.gate.OnDead
	r.gate = g
	return r, b
}

// restart replaces the gate with a new one on the same checkpoint and the
// same broker: what a process restart leaves is exactly the checkpoint and
// the session file.
func (r *gateRig) restart() {
	r.t.Helper()
	old := r.gate
	g := NewBrokerSessionGate(old.source, old.onDemand, old.broker)
	g.Now, g.Sleep, g.OnDead = old.Now, old.Sleep, old.OnDead
	g.BackoffResetToken = old.BackoffResetToken
	r.gate = g
}

func newBrokerPollRig(t *testing.T) (*gateRig, *fakeBroker, *PowerSchoolConnector, connector.Checkpoint, string) {
	t.Helper()
	r, b := newBrokerRig(t)
	cfg := windowConfig()
	cfg.LoginOnDemand = r.gate.onDemand
	c, cp, staging := newWiredConnector(t, pollFixture("111111"), cfg)
	c.client = nil
	c.Gate = r.gate
	c.now = func() time.Time { return r.clock }
	r.cp = cp
	return r, b, c, cp, staging
}

// The whole broker path: dead -> one call -> the session in the answer is
// put into use and checked once -> poll. Nothing is written to a ConfigMap
// and nothing is waited for.
func TestBrokerDeadSessionIsReplacedFromTheAnswer(t *testing.T) {
	r, b := newBrokerRig(t)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	oldFP := SessionFingerprint(r.file)
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("new", SessionLive, ReasonSignedIn)))

	res := r.ensure()
	r.expect(res, GateLive, "", 0)
	if res.Client != r.clients["new"] {
		t.Error("the poll was not given the client of the session the broker returned")
	}
	if len(b.calls) != 1 || b.calls[0] != (brokerCall{brokerapi.ReasonSessionDead, oldFP}) {
		t.Errorf("broker calls = %+v, want one about the dead session", b.calls)
	}
	if r.slept != 0 {
		t.Errorf("slept %s; the broker's answer carries the session, there is nothing to wait for", r.slept)
	}
	if r.probes != 2 {
		t.Errorf("portal reads = %d, want 2: the dead session once, the new one once", r.probes)
	}
	if !r.gate.Synchronous() || !r.gate.OnDemand() {
		t.Error("a gate with a broker does not report itself as on-demand and synchronous")
	}
	if _, ok := r.cp.Load(lastLoginRequestKey); ok {
		t.Error("the mailbox's request time was recorded on the broker path")
	}
}

// Until Vault, ESO and the kubelet have carried the new session into the
// mounted file, the file still holds the one the broker replaced. It must
// not displace the newer one in use.
func TestBrokerSessionIsKeptWhileTheFileStillHoldsTheReplacedOne(t *testing.T) {
	r, b := newBrokerRig(t)
	old := r.session("old", SessionDead, ReasonSignInPage)
	r.file = old
	fresh := r.session("new", SessionLive, ReasonSignedIn)
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, fresh))
	r.expect(r.ensure(), GateLive, "", 0)

	// Next poll: the file has not caught up.
	r.nextWindow(9 * time.Hour)
	res := r.ensure()
	r.expect(res, GateLive, "", 0)
	if res.Client != r.clients["new"] || len(b.calls) != 1 {
		t.Errorf("the stale file displaced the broker's session (broker calls = %d)", len(b.calls))
	}

	// The file catches up: same session, nothing rebuilt, nobody asked.
	builds := r.builds
	r.file = fresh
	r.nextWindow(time.Hour)
	if res := r.ensure(); res.Outcome != GateLive || r.builds != builds || len(b.calls) != 1 {
		t.Errorf("after the file caught up: outcome %d, builds %d -> %d, broker calls %d", res.Outcome, builds, r.builds, len(b.calls))
	}

	// A session nobody here has seen (an operator's manual login) wins.
	r.file = r.session("manual", SessionLive, ReasonSignedIn)
	r.nextWindow(time.Hour)
	if res := r.ensure(); res.Client != r.clients["manual"] {
		t.Error("a new session in the file was not picked up")
	}
}

// No session file at all is a request too, with no fingerprint.
func TestBrokerIsAskedWhenThereIsNoSession(t *testing.T) {
	r, b := newBrokerRig(t)
	b.answer(sessionAnswer(t, brokerapi.OutcomeCurrent, r.session("stored", SessionLive, ReasonSignedIn)))
	res := r.ensure()
	r.expect(res, GateLive, "", 0)
	if len(b.calls) != 1 || b.calls[0] != (brokerCall{brokerapi.ReasonNoSession, ""}) {
		t.Errorf("broker calls = %+v", b.calls)
	}
}

// "Not now" is the broker's decision and nobody's failure. The connector
// applies no blackout and no interval of its own on this path: it asks,
// even at 18:00 Pacific, because only the broker knows whether its stored
// session is alive -- and that answer needs no login.
//
// Having been told when a login may next be made, it then sends NOTHING
// until that time: not a probe of the dead session to the portal, not a
// question to the broker.
func TestBrokerDeferralIsHonouredWithoutProbingOrAsking(t *testing.T) {
	r, b := newBrokerRig(t)
	r.clock = time.Date(2026, 10, 12, 18, 0, 0, 0, la(t))
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	until := time.Date(2026, 10, 12, 21, 0, 0, 0, la(t))
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeDeferred, Code: brokerapi.CodeBlackout, RetryAfter: &until})

	res := r.ensure()
	r.expect(res, GateSkip, "login_deferred:blackout", 0)
	if res.Failed {
		t.Error("a deferral was counted as a failed poll")
	}
	if r.probes != 1 || len(b.calls) != 1 {
		t.Fatalf("first poll: probes %d, broker calls %d", r.probes, len(b.calls))
	}

	// Polls (and restarts) during the blackout: nothing leaves the pod.
	for i := 0; i < 5; i++ {
		r.nextWindow(30 * time.Minute)
		if i == 2 {
			r.restart()
		}
		res := r.ensure()
		r.expect(res, GateSkip, "login_deferred:blackout", 0)
		if res.Failed {
			t.Error("a held poll was counted as failed")
		}
	}
	if r.probes != 1 || len(b.calls) != 1 {
		t.Fatalf("while deferred: portal probes %d (want 1 in all), broker calls %d (want 1 in all)", r.probes, len(b.calls))
	}

	// After the time the broker named: the broker is asked -- the portal
	// still is not, about a session already known dead.
	r.clock = until.Add(31 * time.Minute)
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("new", SessionLive, ReasonSignedIn)))
	r.expect(r.ensure(), GateLive, "", 0)
	if len(b.calls) != 2 || r.probes != 2 { // the second probe is of the NEW session
		t.Errorf("after the deferral: broker calls %d, probes %d", len(b.calls), r.probes)
	}
	if _, ok := r.cp.Load(brokerHoldKey); ok {
		t.Error("the dead-session record outlived a live session")
	}
}

// The day's budget is a deferral until local midnight, under its own name.
func TestBrokerBudgetExhaustionIsHeldUntilItsTime(t *testing.T) {
	r, b := newBrokerRig(t)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	midnight := time.Date(2026, 10, 13, 0, 0, 0, 0, la(t))
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeBudgetExhausted, RetryAfter: &midnight})
	res := r.ensure()
	r.expect(res, GateSkip, "login_deferred:budget_exhausted", 0)
	r.nextWindow(9 * time.Hour) // the afternoon poll
	r.expect(r.ensure(), GateSkip, "login_deferred:budget_exhausted", 0)
	if res.Failed || len(b.calls) != 1 || r.probes != 1 {
		t.Errorf("failed %v, broker calls %d, probes %d", res.Failed, len(b.calls), r.probes)
	}
	// A "not before" years away is believed for a day.
	far := r.clock.Add(24 * 365 * time.Hour)
	r2, b2 := newBrokerRig(t)
	r2.file = r2.session("old", SessionDead, ReasonSignInPage)
	b2.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeDeferred, Code: brokerapi.CodeMinInterval, RetryAfter: &far})
	r2.ensure()
	if hold, ok := loadBrokerHold(r2.cp); !ok || hold.until.After(r2.clock.Add(maxBrokerHold)) {
		t.Errorf("hold = %+v", hold)
	}
}

// The broker's latch does NOT end the process. An exit is a restart, and a
// restart loop would put a poll -- a request to the portal with the dead
// session, and a call to the broker -- on a timer, round the clock. The
// connector stays up: each poll window it asks the broker once, and the
// portal nothing.
func TestBrokerLatchDoesNotCrashLoopOrTouchThePortal(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	c.cfg.FailureThreshold = 2
	r.file = r.session("old", SessionDead, ReasonSignInPage)

	const polls = 8 // far past failure_threshold
	for i := 0; i < polls; i++ {
		b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeLatched, Code: brokerapi.CodeCredentialsRejected})
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatalf("poll %d: %v -- a latched broker must not end the process", i+1, err)
		}
		r.nextWindow(9 * time.Hour)
		r.file = testSession("old", r.clock.Add(24*time.Hour)) // the same dead session, still in the file
	}
	if r.probes != 1 {
		t.Errorf("portal requests with the dead session over %d polls = %d, want 1 (the first poll's)", polls, r.probes)
	}
	if len(b.calls) != polls {
		t.Errorf("broker calls = %d, want one per poll (%d)", len(b.calls), polls)
	}
	if n, _ := loadUnanswered(cp); n != 0 {
		t.Errorf("unanswered = %d: the latch is the broker's count, not this one", n)
	}
	if old := r.clients["old"]; old != nil && old.HomeCalls != 0 {
		t.Error("a kid was read with the dead session")
	}

	// The latch is cleared and the broker logs in: the very next poll runs.
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("new", SessionLive, ReasonSignedIn)))
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if r.clients["new"] == nil || r.clients["new"].HomeCalls == 0 {
		t.Error("the poll did not run after the latch was cleared")
	}
}

// The same for every other way the broker can fail to produce a session:
// the process stays up, whatever failure_threshold says.
func TestBrokerFailuresNeverEndTheProcess(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	c.cfg.FailureThreshold = 2
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	for i := 0; i < 6; i++ {
		switch i % 3 {
		case 0:
			b.fail(errors.New("login broker: connection refused"))
		case 1:
			b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeFailed, Code: brokerapi.CodeLoginFailed})
		default:
			b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeInconclusive, Code: brokerapi.CodeProbeInconclusive})
		}
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatalf("poll %d: %v", i+1, err)
		}
		r.nextWindow(time.Hour)
	}
	if r.probes != 1 || len(b.calls) != 6 {
		t.Errorf("portal probes %d (want 1), broker calls %d (want 6)", r.probes, len(b.calls))
	}
}

// A login the broker could not make, a state it could not establish and a
// broker that did not answer are failed polls -- they count toward
// failure_threshold -- but not strikes: the broker counts its own failures.
func TestBrokerFailuresAreFailedPollsButNotStrikes(t *testing.T) {
	for name, queue := range map[string]func(b *fakeBroker){
		"failed": func(b *fakeBroker) {
			b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeFailed, Code: brokerapi.CodeLoginNotSubmitted, Detail: "signin_page_unreachable"})
		},
		"inconclusive": func(b *fakeBroker) {
			b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeInconclusive, Code: brokerapi.CodeProbeInconclusive})
		},
		"unreachable": func(b *fakeBroker) { b.fail(errors.New("login broker: connection refused")) },
		"forbidden":   func(b *fakeBroker) { b.fail(&brokerapi.StatusError{Status: 403, Code: "forbidden"}) },
		"no session in the answer": func(b *fakeBroker) {
			b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeRefreshed})
		},
		"garbage for a session": func(b *fakeBroker) {
			b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeCurrent, Session: map[string]string{SessionField: "{not json"}})
		},
	} {
		r, b := newBrokerRig(t)
		r.file = r.session("old", SessionDead, ReasonSignInPage)
		queue(b)
		res := r.ensure()
		if res.Outcome != GateSkip || !res.Failed {
			t.Errorf("%s: outcome %d failed %v, want a failed skip", name, res.Outcome, res.Failed)
		}
		if n, _ := loadUnanswered(r.cp); n != 0 {
			t.Errorf("%s: unanswered = %d", name, n)
		}
		if len(b.calls) != 1 {
			t.Errorf("%s: broker calls = %d, want exactly one per poll", name, len(b.calls))
		}
	}
}

// What does count as a strike: a session that was delivered and is then
// refused here, and the broker handing back the very session that was just
// refused. Two in a row stop the asking -- and a restart does not start it
// again: the count is in the checkpoint and is not tied to whichever
// session happens to be in place. While stopped, a poll sends nothing to
// the broker and nothing to the portal. The operator's reset token, or a
// session the portal accepts, clears it.
func TestBrokerSessionsThatDoNotWorkStopTheAskingAcrossRestarts(t *testing.T) {
	r, b := newBrokerRig(t)
	old := r.session("old", SessionDead, ReasonSignInPage)
	r.file = old
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("bad1", SessionDead, ReasonSignInPage)))
	res := r.ensure()
	r.expect(res, GateSkip, "new_session_not_accepted", 0)
	if n, _ := loadUnanswered(r.cp); n != 1 || !res.Failed {
		t.Fatalf("after one refused session: unanswered = %d, failed = %v", n, res.Failed)
	}

	// The process restarts (as it would have, before, on the way to
	// "forgetting" the strike). The file still holds the old session; the
	// strike is still there.
	r.restart()
	r.nextWindow(9 * time.Hour)
	r.file = testSession("old", r.clock.Add(24*time.Hour))
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("bad2", SessionDead, ReasonSignInPage)))
	res = r.ensure()
	if res.Outcome != GateBlocked {
		t.Fatalf("after two, across a restart: outcome %d reason %q, want GateBlocked", res.Outcome, res.Reason)
	}
	if n, _ := loadUnanswered(r.cp); n != 2 {
		t.Fatalf("unanswered = %d, want 2", n)
	}

	// Stopped, and it stays stopped through restarts and through the
	// session file changing to the sessions the broker delivered: no call
	// to the broker at all.
	calls, probes := len(b.calls), r.probes
	for i, file := range []string{"old", "bad1", "bad2", "bad2", "bad2"} {
		r.restart()
		r.nextWindow(9 * time.Hour)
		r.file = testSession(file, r.clock.Add(24*time.Hour))
		if res := r.ensure(); res.Outcome != GateBlocked {
			t.Fatalf("poll %d with %q in the file: outcome %d reason %q", i, file, res.Outcome, res.Reason)
		}
	}
	if len(b.calls) != calls {
		t.Errorf("a stopped gate asked the broker %d more times", len(b.calls)-calls)
	}
	// Each session is asked about at most once more after a restart that
	// lost track of it; a session already recorded dead is not probed.
	if got := r.probes - probes; got > 3 {
		t.Errorf("portal probes while stopped = %d", got)
	}
	last := r.probes
	r.nextWindow(9 * time.Hour)
	r.ensure()
	r.nextWindow(9 * time.Hour)
	r.file = testSession("bad2", r.clock.Add(24*time.Hour))
	r.ensure()
	if r.probes != last {
		t.Errorf("a stopped gate kept probing a session it knows is dead (%d more)", r.probes-last)
	}

	// The operator's reset opens it again.
	r.gate.BackoffResetToken = "2026-10-13"
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("good", SessionLive, ReasonSignedIn)))
	r.nextWindow(time.Hour)
	r.file = testSession("bad2", r.clock.Add(24*time.Hour))
	if res := r.ensure(); res.Outcome != GateLive || len(b.calls) != calls+1 {
		t.Errorf("after the reset: outcome %d reason %q, broker calls %d", res.Outcome, res.Reason, len(b.calls))
	}
	if n, _ := loadUnanswered(r.cp); n != 0 {
		t.Errorf("unanswered = %d after a live session", n)
	}
}

// The broker handing back the very session this connector was just refused
// with is a strike too.
func TestBrokerReturningTheDeadSessionIsAStrike(t *testing.T) {
	r, b := newBrokerRig(t)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	b.answer(sessionAnswer(t, brokerapi.OutcomeCurrent, testSession("old", r.clock.Add(24*time.Hour))))
	res := r.ensure()
	r.expect(res, GateSkip, "broker_session_is_the_dead_one", 0)
	if n, _ := loadUnanswered(r.cp); n != 1 {
		t.Errorf("unanswered = %d", n)
	}
}

// A new session appearing in the file while the gate is stopped (an
// operator's manual login) is looked at once; if the portal accepts it,
// everything is cleared.
func TestAStoppedBrokerGateRecoversOnALiveSessionInTheFile(t *testing.T) {
	r, b := newBrokerRig(t)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	if err := r.cp.Save(unansweredKey, "2 "+SessionFingerprint(r.file)); err != nil {
		t.Fatal(err)
	}
	if res := r.ensure(); res.Outcome != GateBlocked {
		t.Fatalf("outcome %d", res.Outcome)
	}
	r.nextWindow(9 * time.Hour)
	r.file = r.session("manual", SessionLive, ReasonSignedIn)
	if res := r.ensure(); res.Outcome != GateLive || len(b.calls) != 0 {
		t.Fatalf("outcome %d reason %q, broker calls %d", res.Outcome, res.Reason, len(b.calls))
	}
	if n, _ := loadUnanswered(r.cp); n != 0 {
		t.Errorf("unanswered = %d", n)
	}
}

// One poll asks the broker at most once: a session the broker delivered at
// the start of the poll and that dies in the middle of it is not answered
// with a second request in the same poll.
func TestBrokerIsAskedAtMostOncePerPoll(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.gate.source.Build = wrapBuild(r.gate.source.Build, func(id string, client *fakeClient) {
		if id == "s2" {
			client.HomeFunc = func(context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
		}
	})
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("s2", SessionLive, ReasonSignedIn)))
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if len(b.calls) != 1 {
		t.Fatalf("broker calls in one poll = %d, want 1", len(b.calls))
	}
	// The next poll takes it up -- without probing s2, which is known dead.
	probes := r.probes
	r.nextWindow(9 * time.Hour)
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("s3", SessionLive, ReasonSignedIn)))
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 2 || b.calls[1].fingerprint != SessionFingerprint(testSession("s2", r.clock)) {
		t.Errorf("second poll: broker calls %+v", b.calls)
	}
	if r.probes != probes+1 { // only the new session s3
		t.Errorf("second poll probed %d times, want 1 (the new session only)", r.probes-probes)
	}
}

// With the broker, "could not tell" does not go on for ever in silence:
// the third poll in a row is reported as a failed poll. Nobody is asked for
// a login -- a login would end the session if it is alive.
func TestBrokerModeEscalatesAnUnknownSessionState(t *testing.T) {
	r, b := newBrokerRig(t)
	r.file = r.session("s1", SessionUnknown, ReasonNetwork)
	for i := 1; i <= 4; i++ {
		res := r.ensure()
		if res.Outcome != GateSkip || res.Failed != (i >= unknownStreakFails) {
			t.Errorf("poll %d: outcome %d failed %v", i, res.Outcome, res.Failed)
		}
		r.nextWindow(time.Hour)
	}
	if len(b.calls) != 0 {
		t.Errorf("the broker was asked %d times about a session that may be alive", len(b.calls))
	}
	// One answered check ends the streak.
	r.states["s1"] = SessionVerdict{SessionLive, ReasonSignedIn}
	r.ensure()
	r.states["s1"] = SessionVerdict{SessionUnknown, ReasonNetwork}
	if res := r.ensure(); res.Failed {
		t.Error("the streak was not reset by a live answer")
	}
}

// The session dies in the middle of a poll: the connector asks the broker
// and runs the poll again on the new session -- once.
func TestBrokerMidPollDeathAsksAndRetriesThePollOnce(t *testing.T) {
	r, b, c, cp, staging := newBrokerPollRig(t)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	// s1 passes the pre-poll check and is then refused on the first read.
	r.gate.source.Build = wrapBuild(r.gate.source.Build, func(id string, client *fakeClient) {
		if id == "s1" {
			client.HomeFunc = func(context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
		}
	})
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("s2", SessionLive, ReasonSignedIn)))

	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if len(b.calls) != 1 || b.calls[0].reason != brokerapi.ReasonAuthFailure || b.calls[0].fingerprint != SessionFingerprint(r.file) {
		t.Errorf("broker calls = %+v, want one auth_failure about s1", b.calls)
	}
	if r.clients["s2"] == nil || r.clients["s2"].HomeCalls == 0 {
		t.Error("the poll was not retried on the new session")
	}
	if len(readItems(t, staging)) == 0 {
		t.Error("nothing was staged by the retried poll")
	}
}

// ... and only once: a second refusal in the same poll is not answered
// with a second request, and the process does not exit.
func TestBrokerMidPollDeathIsNotRetriedTwice(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	r.gate.source.Build = wrapBuild(r.gate.source.Build, func(_ string, client *fakeClient) {
		client.HomeFunc = func(context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
	})
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("s2", SessionLive, ReasonSignedIn)))

	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if len(b.calls) != 1 {
		t.Errorf("broker calls = %d, want exactly 1", len(b.calls))
	}
}

// Without a broker a mid-poll death is left to the next poll, as before.
func TestRenewDoesNothingWithoutABroker(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	r.ensure()
	if res := r.gate.Renew(context.Background(), r.cp); res.Outcome != GateSkip || len(r.kube.requests) != 0 {
		t.Errorf("Renew on the mailbox path: outcome %d, requests %d", res.Outcome, len(r.kube.requests))
	}
}

// wrapBuild lets a test adjust the fake client a session gets.
func wrapBuild(build func(*pslib.SessionExport) (PowerSchoolClient, ProbeFunc, func(), error),
	adjust func(id string, client *fakeClient)) func(*pslib.SessionExport) (PowerSchoolClient, ProbeFunc, func(), error) {
	return func(s *pslib.SessionExport) (PowerSchoolClient, ProbeFunc, func(), error) {
		client, probe, reset, err := build(s)
		if fc, ok := client.(*fakeClient); ok {
			adjust(s.Cookies[0].Value, fc)
		}
		return client, probe, reset, err
	}
}

// Exactly one way to ask is configured.
func TestLoginOnDemandBrokerConfig(t *testing.T) {
	good := LoginOnDemandConfig{Enabled: true, Broker: LoginBrokerConfig{
		URL: "http://glovebox-login-broker-powerschool:8080", TokenFile: "/var/run/secrets/login-broker/token"}}
	if err := validateLoginOnDemand(good); err != nil {
		t.Fatalf("a broker configuration was refused: %v", err)
	}
	if !good.UsesBroker() || good.BrokerTimeout() != 420*time.Second {
		t.Errorf("UsesBroker = %v, timeout = %s", good.UsesBroker(), good.BrokerTimeout())
	}
	for name, mutate := range map[string]func(c *LoginOnDemandConfig){
		"both ways at once":   func(c *LoginOnDemandConfig) { c.ConfigMapName = "glovebox-powerschool-login-request" },
		"no token file":       func(c *LoginOnDemandConfig) { c.Broker.TokenFile = "" },
		"relative token file": func(c *LoginOnDemandConfig) { c.Broker.TokenFile = "token" },
		"a path in the URL":   func(c *LoginOnDemandConfig) { c.Broker.URL += "/v1/session/refresh" },
		"not http":            func(c *LoginOnDemandConfig) { c.Broker.URL = "ftp://broker" },
		"userinfo":            func(c *LoginOnDemandConfig) { c.Broker.URL = "http://u:p@broker:8080" },
		"timeout too short":   func(c *LoginOnDemandConfig) { c.Broker.TimeoutSeconds = 5 },
		"timeout too long":    func(c *LoginOnDemandConfig) { c.Broker.TimeoutSeconds = 3600 },
	} {
		c := good
		mutate(&c)
		if err := validateLoginOnDemand(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Neither: the message names both.
	err := validateLoginOnDemand(LoginOnDemandConfig{Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "broker.url") || !strings.Contains(err.Error(), "configmap_name") {
		t.Errorf("neither way configured: %v", err)
	}
}
