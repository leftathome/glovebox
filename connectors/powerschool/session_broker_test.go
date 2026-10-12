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
// even at 18:00 Pacific and twice in ten minutes, because only the broker
// knows whether its stored session is alive -- and that answer needs no
// login.
func TestBrokerDeferralSkipsThePollWithoutFailingIt(t *testing.T) {
	r, b := newBrokerRig(t)
	r.clock = time.Date(2026, 10, 12, 18, 0, 0, 0, la(t))
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	until := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeDeferred, Code: brokerapi.CodeBlackout, RetryAfter: &until})
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeDeferred, Code: brokerapi.CodeMinInterval, RetryAfter: &until})

	res := r.ensure()
	r.expect(res, GateSkip, "login_deferred:blackout", 0)
	if res.Failed {
		t.Error("a deferral was counted as a failed poll")
	}
	r.nextWindow(10 * time.Minute)
	res = r.ensure()
	r.expect(res, GateSkip, "login_deferred:min_interval", 0)
	if len(b.calls) != 2 {
		t.Errorf("broker calls = %d, want 2", len(b.calls))
	}
	if n, _ := loadUnanswered(r.cp); n != 0 {
		t.Errorf("unanswered = %d after deferrals", n)
	}
}

// The broker's latch is the permanent-failure path, with its own message:
// nothing on the connector's side can clear it.
func TestBrokerLatchEndsInAPermanentError(t *testing.T) {
	r, b, c, cp, _ := newBrokerPollRig(t)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	b.answer(&brokerapi.Response{Outcome: brokerapi.OutcomeLatched, Code: brokerapi.CodeCredentialsRejected})

	err := c.pollNow(context.Background(), cp, "scheduled")
	if !connector.IsPermanent(err) || !strings.Contains(err.Error(), "latch") {
		t.Fatalf("err = %v, want the latch PermanentError", err)
	}
	if n, _ := loadUnanswered(cp); n != 0 {
		t.Errorf("unanswered = %d: the latch is the broker's count, not this one", n)
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
// refused. Two in a row stop the asking, exactly as on the mailbox path,
// and the operator's reset token clears it the same way.
func TestBrokerSessionsThatDoNotWorkStopTheAsking(t *testing.T) {
	r, b := newBrokerRig(t)
	old := r.session("old", SessionDead, ReasonSignInPage)
	r.file = old
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("bad1", SessionDead, ReasonSignInPage)))
	res := r.ensure()
	r.expect(res, GateSkip, "new_session_not_accepted", 0)
	if n, _ := loadUnanswered(r.cp); n != 1 || !res.Failed {
		t.Fatalf("after one refused session: unanswered = %d, failed = %v", n, res.Failed)
	}

	// The gate now holds bad1; the broker hands the same one back.
	r.nextWindow(9 * time.Hour)
	b.answer(sessionAnswer(t, brokerapi.OutcomeCurrent, testSession("bad1", r.clock.Add(24*time.Hour))))
	res = r.ensure()
	if res.Outcome != GateBlocked {
		t.Fatalf("after two: outcome %d reason %q, want GateBlocked", res.Outcome, res.Reason)
	}

	// Stopped: no further call.
	r.nextWindow(9 * time.Hour)
	if res := r.ensure(); res.Outcome != GateBlocked || len(b.calls) != 2 {
		t.Errorf("a stopped gate asked again: outcome %d, broker calls %d", res.Outcome, len(b.calls))
	}

	// The operator's reset opens it again.
	r.gate.BackoffResetToken = "2026-10-13"
	b.answer(sessionAnswer(t, brokerapi.OutcomeRefreshed, r.session("good", SessionLive, ReasonSignedIn)))
	r.nextWindow(time.Hour)
	if res := r.ensure(); res.Outcome != GateLive || len(b.calls) != 3 {
		t.Errorf("after the reset: outcome %d, broker calls %d", res.Outcome, len(b.calls))
	}
}

// With the broker, "could not tell" does not go on for ever in silence:
// the third poll in a row is a failed poll. Nobody is asked for a login --
// a login would end the session if it is alive.
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
