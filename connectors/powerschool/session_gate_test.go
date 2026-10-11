package powerschool

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// --- fakes ------------------------------------------------------------------

func la(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// sixAM is a Monday 06:10 Pacific: a poll window, far from the blackout.
func sixAM(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 10, 12, 6, 10, 0, 0, la(t))
}

func testSession(cookie string, expires time.Time) *pslib.SessionExport {
	return &pslib.SessionExport{
		Cookies:   []*http.Cookie{{Name: "JSESSIONID", Value: cookie}, {Name: "MRHSession", Value: "mrh-" + cookie}},
		ExpiresAt: expires,
	}
}

// fakeRequester records the requests the connector writes.
type fakeRequester struct {
	err      error
	requests []string
	// onRequest runs when a request is written.
	onRequest func()
}

func (f *fakeRequester) RequestLogin(_ context.Context, request string) error {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return f.err
	}
	if f.onRequest != nil {
		f.onRequest()
	}
	return nil
}

// gateRig is a SessionGate on fakes: a session "file", a portal that knows
// which sessions it accepts, a request sink and a clock.
type gateRig struct {
	t    *testing.T
	gate *SessionGate
	kube *fakeRequester
	cp   connector.Checkpoint

	clock time.Time
	slept time.Duration

	// file is the session document in the mounted file; nil = no file.
	file    *pslib.SessionExport
	fileAge time.Duration
	// states maps a session's JSESSIONID to what the portal says of it.
	// A session not listed is unknown.
	states map[string]SessionVerdict
	// arrives, when set, replaces the file once this much has been slept.
	arriveAfter time.Duration
	arrives     *pslib.SessionExport

	probes, builds, resets int
	deadAges               []time.Duration
	clients                map[string]*fakeClient
}

func newGateRig(t *testing.T, onDemand bool) *gateRig {
	t.Helper()
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &gateRig{
		t: t, kube: &fakeRequester{}, cp: cp, clock: sixAM(t),
		states:  map[string]SessionVerdict{},
		clients: map[string]*fakeClient{},
		fileAge: time.Hour,
	}
	source := SessionSource{
		Load: func(now time.Time) (*pslib.SessionExport, bool, error) {
			if r.file == nil {
				return nil, false, errors.New("read session file: no such file")
			}
			return r.file, !r.file.ExpiresAt.After(now), nil
		},
		Build: func(s *pslib.SessionExport) (PowerSchoolClient, ProbeFunc, func(), error) {
			r.builds++
			id := s.Cookies[0].Value
			client := pollFixture("111111")
			r.clients[id] = client
			probe := func(context.Context) SessionVerdict {
				r.probes++
				if v, ok := r.states[id]; ok {
					return v
				}
				return SessionVerdict{SessionUnknown, ReasonNetwork}
			}
			return client, probe, func() { r.resets++ }, nil
		},
		FileAge: func(time.Time) (time.Duration, bool) { return r.fileAge, r.file != nil },
	}
	c := Config{LoginOnDemand: LoginOnDemandConfig{Enabled: onDemand, ConfigMapName: "glovebox-glovebox-powerschool-login-request"}}
	ApplyDefaults(&c)
	var requester LoginRequester
	if onDemand {
		requester = r.kube
	}
	r.gate = NewSessionGate(source, c.LoginOnDemand, requester)
	r.gate.Now = func() time.Time { return r.clock }
	r.gate.Sleep = func(_ context.Context, d time.Duration) error {
		r.clock = r.clock.Add(d)
		r.slept += d
		if r.arrives != nil && r.slept >= r.arriveAfter {
			r.file, r.arrives = r.arrives, nil
			r.fileAge = 0
		}
		return nil
	}
	r.gate.OnDead = func(age time.Duration, _ string) { r.deadAges = append(r.deadAges, age) }
	return r
}

func (r *gateRig) session(id string, state SessionState, reason string) *pslib.SessionExport {
	r.states[id] = SessionVerdict{state, reason}
	return testSession(id, r.clock.Add(24*time.Hour))
}

func (r *gateRig) ensure() GateResult {
	r.t.Helper()
	return r.gate.Ensure(context.Background(), r.cp)
}

func (r *gateRig) expect(res GateResult, outcome GateOutcome, reason string, requests int) {
	r.t.Helper()
	if res.Outcome != outcome || (reason != "" && res.Reason != reason) {
		r.t.Errorf("Ensure = outcome %d reason %q, want outcome %d reason %q", res.Outcome, res.Reason, outcome, reason)
	}
	if len(r.kube.requests) != requests {
		r.t.Errorf("login requests written = %d, want %d", len(r.kube.requests), requests)
	}
}

// nextWindow moves the clock on by d and makes the session file look old
// again, as it would be at the next poll.
func (r *gateRig) nextWindow(d time.Duration) {
	r.clock = r.clock.Add(d)
	r.slept = 0
	r.fileAge = time.Hour
}

// --- A: classification ------------------------------------------------------

// Dead is a strong claim: it leads to a login request. Only the portal's
// own sign-in page, positively identified, or the gateway's "no session"
// page earn it. The status is looked at before the path.
func TestClassifyHomeResponse(t *testing.T) {
	home := `<ul id="students-list"><li class="selected"><a href="javascript:switchStudent(111111);">One</a></li></ul>`
	signIn := `<form id="LoginForm" action="/guardian/home.html"><input type="text" name="account"><input type="password" name="pw"></form>`
	for _, c := range []struct {
		name   string
		path   string
		status int
		body   string
		state  SessionState
		reason string
	}{
		{"the signed-in home page", "/guardian/home.html", 200, home, SessionLive, ReasonSignedIn},

		{"the sign-in page under /public/", "/public/home.html", 200, signIn, SessionDead, ReasonSignInPage},
		{"the gateway's no-session page", "/my.logout.php3", 200, "<html>Your session could not be established.</html>", SessionDead, ReasonGatewayPage},

		{"under /public/ but no password form", "/public/", 200, "<html>Down for maintenance</html>", SessionUnknown, ReasonUnexpectedPage},
		{"under /public/, an empty page", "/public/home.html", 200, "", SessionUnknown, ReasonUnexpectedPage},
		{"a sign-in form somewhere that is not /public/", "/guardian/home.html", 200, signIn, SessionUnknown, ReasonUnexpectedPage},
		{"the gateway's policy endpoint", "/my.policy", 200, "<html><noscript>js</noscript></html>", SessionUnknown, ReasonGatewayPolicy},
		{"401", "/guardian/home.html", 401, "", SessionUnknown, "http_401"},
		{"403", "/guardian/home.html", 403, "", SessionUnknown, "http_403"},
		{"403 from the sign-in page", "/public/home.html", 403, signIn, SessionUnknown, "http_403"},
		{"403 at the gateway's no-session page", "/my.logout.php3", 403, "", SessionUnknown, "http_403"},
		{"404 at the gateway's no-session page", "/my.logout.php3", 404, "", SessionUnknown, "http_404"},
		{"500 at the gateway's no-session page", "/my.logout.php3", 500, "", SessionUnknown, ReasonServerError},
		{"503", "/guardian/home.html", 503, "", SessionUnknown, ReasonServerError},
		{"500 with a sign-in form under /public/", "/public/home.html", 500, signIn, SessionUnknown, ReasonServerError},
		{"500 with the switcher in it", "/guardian/home.html", 500, home, SessionUnknown, ReasonServerError},
		{"404", "/guardian/home.html", 404, "", SessionUnknown, "http_404"},
		{"a maintenance notice", "/guardian/home.html", 200, "<html>Down for maintenance</html>", SessionUnknown, ReasonUnexpectedPage},
		{"a page that only mentions passwords", "/public/help.html", 200, "<p>Forgot your password? Call the office.</p>", SessionUnknown, ReasonUnexpectedPage},
		{"an empty body", "/guardian/home.html", 200, "", SessionUnknown, ReasonUnexpectedPage},
	} {
		got := ClassifyHomeResponse(c.path, c.status, c.body)
		if got.State != c.state || got.Reason != c.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, got.State, got.Reason, c.state, c.reason)
		}
	}
}

// --- B: asking, and its limits ------------------------------------------------

func TestGateLiveSessionPollsWithoutAsking(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	res := r.ensure()
	r.expect(res, GateLive, "", 0)
	if res.Client != r.clients["s1"] || r.probes != 1 {
		t.Errorf("client set = %v, probes = %d", res.Client != nil, r.probes)
	}
	if len(r.deadAges) != 0 {
		t.Error("a live session was reported dead")
	}
}

// Unknown is not dead: the poll is skipped, nothing changes, nobody is
// asked for a login, and it is nobody's failure.
func TestGateUnknownSkipsAndAsksNothing(t *testing.T) {
	for _, onDemand := range []bool{true, false} {
		r := newGateRig(t, onDemand)
		r.file = r.session("s1", SessionUnknown, ReasonServerError)
		res := r.ensure()
		r.expect(res, GateSkip, "session_state_unknown:server_error", 0)
		if res.Failed || len(r.deadAges) != 0 {
			t.Errorf("on-demand %v: failed = %v, dead reports = %d", onDemand, res.Failed, len(r.deadAges))
		}
		if _, ok := r.cp.Load(lastLoginRequestKey); ok {
			t.Error("a login request was recorded")
		}
	}
}

func TestGateDeadWithoutOnDemandIsLeftToTheRejectedPath(t *testing.T) {
	r := newGateRig(t, false)
	r.file = r.session("s1", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateDead, ReasonSignInPage, 0)
	if len(r.deadAges) != 1 {
		t.Errorf("dead reports = %d, want 1", len(r.deadAges))
	}
}

// The whole on-demand path: dead -> one request -> the new session arrives
// -> it is checked once -> poll.
func TestGateDeadSessionAsksOnceAndWaitsForTheNewOne(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	oldFP := SessionFingerprint(r.file)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 7*time.Minute+30*time.Second

	res := r.ensure()
	r.expect(res, GateLive, "", 1)
	if res.Client != r.clients["new"] {
		t.Error("the poll was not given the new session's client")
	}
	if r.slept != 7*time.Minute+30*time.Second {
		t.Errorf("waited %s, want 7m30s in 5s steps", r.slept)
	}
	if r.probes != 2 {
		t.Errorf("portal reads = %d, want 2: the dead session once, the new one once", r.probes)
	}

	// What was written is exactly a request: the time, and a prefix of the
	// dead session's fingerprint. Nothing else.
	req, err := psportal.ParseLoginRequest(r.kube.requests[0])
	if err != nil {
		t.Fatalf("the request written does not parse: %v (%q)", err, r.kube.requests[0])
	}
	if !req.At.Equal(sixAM(t)) || req.Fingerprint != oldFP[:16] {
		t.Errorf("request = %+v, want %s and %s", req, sixAM(t).UTC(), oldFP[:16])
	}
	for _, secret := range []string{"old", "mrh-old", "JSESSIONID"} {
		if strings.Contains(strings.TrimPrefix(r.kube.requests[0], req.At.Format(time.RFC3339)), secret) {
			t.Errorf("the request carries %q", secret)
		}
	}
	raw, ok := r.cp.Load(lastLoginRequestKey)
	if !ok || raw != sixAM(t).UTC().Format(time.RFC3339) {
		t.Errorf("recorded request time = %q, %v", raw, ok)
	}
	if n, _ := loadUnanswered(r.cp); n != 0 {
		t.Errorf("unanswered = %d after a live session", n)
	}
}

// The default wait is ten minutes: the login workload's cadence (up to 3
// min), the login (up to ~100 s), ESO (1 min) and the kubelet (up to ~90 s)
// add up to about seven and a half.
func TestGateNewSessionNeverAppears(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	res := r.ensure()
	r.expect(res, GateSkip, "no_new_session", 1)
	if !res.Failed {
		t.Error("a request that led nowhere is not marked as a failure")
	}
	if r.slept != 10*time.Minute {
		t.Errorf("waited %s, want the default 10m", r.slept)
	}
	if r.probes != 1 {
		t.Errorf("portal reads while waiting: %d, want only the first check", r.probes)
	}
	if n, fp := loadUnanswered(r.cp); n != 1 || fp != SessionFingerprint(r.file) {
		t.Errorf("unanswered = %d against %q", n, fp)
	}
}

// A session that arrives at the very end of the wait, long after the poll
// window's nominal end, still gets its one poll.
func TestGateSessionArrivingLateInTheWaitStillPolls(t *testing.T) {
	r := newGateRig(t, true)
	r.clock = time.Date(2026, 10, 12, 6, 29, 50, 0, la(t)) // ten seconds before 06:30
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 9*time.Minute+55*time.Second
	r.expect(r.ensure(), GateLive, "", 1)
	if got := r.clock.In(la(t)).Format("15:04:05"); got != "06:39:45" {
		t.Errorf("the poll would start at %s, want 06:39:45 (after the window, inside the wait)", got)
	}
}

func TestGateWaitIsConfigurable(t *testing.T) {
	r := newGateRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateSkip, "no_new_session", 1)
	if r.slept != 60*time.Second {
		t.Errorf("waited %s, want 60s", r.slept)
	}
}

// A new session arrives but the portal refuses it too. One request per
// poll: no second request.
func TestGateNewSessionAppearsButIsDead(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("also-dead", SessionDead, ReasonGatewayPage), 30*time.Second
	res := r.ensure()
	r.expect(res, GateSkip, "new_session_not_accepted", 1)
	if !res.Failed || len(r.deadAges) != 2 {
		t.Errorf("failed = %v, dead reports = %d", res.Failed, len(r.deadAges))
	}
	if n, fp := loadUnanswered(r.cp); n != 1 || fp != SessionFingerprint(r.file) {
		t.Errorf("unanswered = %d against %q, want 1 against the new session", n, fp)
	}
}

// At most one request per 20 minutes, across polls and across restarts (the
// time is in the checkpoint).
func TestGateAsksAtMostOncePerTwentyMinutes(t *testing.T) {
	r := newGateRig(t, true)
	r.gate.onDemand.WaitSeconds = 300
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateSkip, "no_new_session", 1) // 5 minutes pass

	r.nextWindow(5 * time.Minute) // 10 minutes after the request
	res := r.ensure()
	r.expect(res, GateSkip, "login_requested_recently", 1)
	if res.Failed {
		t.Error("declining to ask again was counted as a failure")
	}

	// A new process on the same checkpoint.
	r2 := newGateRig(t, true)
	r2.cp = r.cp
	r2.clock = sixAM(t).Add(19*time.Minute + 59*time.Second)
	r2.file = r2.session("old", SessionDead, ReasonSignInPage)
	r2.expect(r2.ensure(), GateSkip, "login_requested_recently", 0)

	r2.clock = sixAM(t).Add(20 * time.Minute)
	r2.arrives, r2.arriveAfter = r2.session("new", SessionLive, ReasonSignedIn), 10*time.Second
	r2.expect(r2.ensure(), GateLive, "", 1)
}

// A recorded request time in the future (a clock that jumped back, or a
// damaged checkpoint) must not open the gate: it is treated as just made.
func TestGateTreatsAFutureRequestTimeAsRecent(t *testing.T) {
	for _, ahead := range []time.Duration{time.Second, time.Hour, 400 * 24 * time.Hour} {
		r := newGateRig(t, true)
		if err := r.cp.Save(lastLoginRequestKey, r.clock.Add(ahead).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		r.file = r.session("old", SessionDead, ReasonSignInPage)
		r.expect(r.ensure(), GateSkip, "login_requested_recently", 0)
		if r.slept != 0 {
			t.Errorf("%s ahead: waited %s", ahead, r.slept)
		}
	}
}

// A restart in the middle of a wait: the request was made a minute ago.
// No second request; the rest of the wait is waited.
func TestGateRestartInMidWaitDoesNotAskAgain(t *testing.T) {
	r := newGateRig(t, true)
	if err := r.cp.Save(lastLoginRequestKey, r.clock.Add(-time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 40*time.Second
	r.expect(r.ensure(), GateLive, "", 0)

	r = newGateRig(t, true)
	_ = r.cp.Save(lastLoginRequestKey, r.clock.Add(-time.Minute).UTC().Format(time.RFC3339))
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateSkip, "no_new_session", 0)
	if r.slept != 9*time.Minute {
		t.Errorf("waited %s, want the remaining 9m of the 10m", r.slept)
	}
}

// Nothing is asked from 15:57:30 to 21:00 Pacific: the login workload would
// not act, and outside the test the owner may be signed in.
func TestGateNeverAsksDuringTheBlackout(t *testing.T) {
	for _, c := range []struct {
		h, m, s int
		blocked bool
	}{
		{15, 57, 29, false}, {15, 57, 30, true}, {15, 59, 59, true}, {16, 0, 0, true},
		{18, 30, 0, true}, {20, 59, 59, true}, {21, 0, 0, false}, {21, 30, 0, false}, {15, 10, 0, false},
	} {
		for _, day := range [][2]int{{10, 12}, {3, 8}, {11, 1}, {1, 14}} { // incl. both DST changes
			r := newGateRig(t, true)
			r.clock = time.Date(2026, time.Month(day[0]), day[1], c.h, c.m, c.s, 0, la(t)).UTC()
			r.file = r.session("old", SessionDead, ReasonSignInPage)
			r.gate.onDemand.WaitSeconds = 30
			res := r.ensure()
			if c.blocked {
				r.expect(res, GateSkip, "blackout", 0)
				if res.Failed {
					t.Error("the blackout was counted as a failure")
				}
				if _, ok := r.cp.Load(lastLoginRequestKey); ok {
					t.Error("a request was recorded during the blackout")
				}
			} else if len(r.kube.requests) != 1 {
				t.Errorf("%d/%d %02d:%02d:%02d: requests = %d, want 1", day[0], day[1], c.h, c.m, c.s, len(r.kube.requests))
			}
		}
	}
}

// The session file was written less than two minutes ago and is already
// refused: a login has just happened. Another one will not help.
func TestGateDoesNotAskWhenTheSessionFileIsFresh(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("just-written", SessionDead, ReasonSignInPage)
	r.fileAge = 119 * time.Second
	r.expect(r.ensure(), GateSkip, "session_file_fresh", 0)

	r.fileAge = 2 * time.Minute
	r.gate.onDemand.WaitSeconds = 30
	r.expect(r.ensure(), GateSkip, "no_new_session", 1)
}

// No session file yet (first start), or one past its expires_at: the same
// path as a dead session, without spending a request on asking the portal.
func TestGateNoSessionOrExpiredDocumentAsksForALogin(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		r := newGateRig(t, true)
		r.arrives, r.arriveAfter = r.session("first", SessionLive, ReasonSignedIn), 60*time.Second
		res := r.ensure()
		r.expect(res, GateLive, "", 1)
		if r.probes != 1 {
			t.Errorf("portal reads = %d, want only the new session's check", r.probes)
		}
		if !strings.HasSuffix(r.kube.requests[0], " none") {
			t.Errorf("request without a session = %q, want the fingerprint \"none\"", r.kube.requests[0])
		}
	})
	t.Run("expired document", func(t *testing.T) {
		r := newGateRig(t, true)
		r.states["stale"] = SessionVerdict{SessionLive, ReasonSignedIn} // never asked
		r.file = testSession("stale", r.clock.Add(-time.Minute))
		r.arrives, r.arriveAfter = r.session("fresh", SessionLive, ReasonSignedIn), 60*time.Second
		res := r.ensure()
		r.expect(res, GateLive, "", 1)
		if res.Client != r.clients["fresh"] || r.probes != 1 {
			t.Errorf("probes = %d; the expired session must not be presented", r.probes)
		}
	})
	t.Run("expired document, on-demand off", func(t *testing.T) {
		r := newGateRig(t, false)
		r.file = testSession("stale", r.clock.Add(-time.Minute))
		r.expect(r.ensure(), GateDead, ReasonExpiredDocument, 0)
	})
}

// The request cannot be written (the Role, the ConfigMap, the network):
// the poll is lost, visibly, and nothing is waited for.
func TestGateRequestWriteFailures(t *testing.T) {
	for name, err := range map[string]error{
		"forbidden":      &kubecm.Error{Op: "patch configmap", Status: 403, Reason: "Forbidden"},
		"no configmap":   &kubecm.Error{Op: "patch configmap", Status: 404, Reason: "NotFound"},
		"conflict":       &kubecm.Error{Op: "patch configmap", Status: 409, Reason: "Conflict"},
		"server error":   &kubecm.Error{Op: "patch configmap", Status: 500, Reason: "InternalError"},
		"no path to API": errors.New("dial tcp 10.96.0.1:443: i/o timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newGateRig(t, true)
			r.file = r.session("old", SessionDead, ReasonSignInPage)
			r.kube.err = err
			res := r.ensure()
			if res.Outcome != GateSkip || res.Reason != "login_request_failed" || !res.Failed {
				t.Errorf("Ensure = %d %q failed=%v", res.Outcome, res.Reason, res.Failed)
			}
			if r.slept != 0 {
				t.Errorf("waited %s for a session nobody was asked for", r.slept)
			}
			// It still counts as a request for the interval: its answer
			// may have been lost on the way back.
			if _, recorded := r.cp.Load(lastLoginRequestKey); !recorded {
				t.Error("the attempt was not recorded")
			}
		})
	}
}

// --- finding 3: backoff --------------------------------------------------------

// Two requests in a row that did not lead to a session the portal accepts:
// the connector stops asking. It stays stopped -- across polls, intervals
// and restarts -- until the session file changes or the operator resets it.
func TestGateStopsAskingAfterTwoUnansweredRequests(t *testing.T) {
	r := newGateRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	r.file = r.session("old", SessionDead, ReasonSignInPage)

	first := r.ensure()
	r.expect(first, GateSkip, "no_new_session", 1)

	r.nextWindow(9 * time.Hour) // the afternoon poll
	second := r.ensure()
	r.expect(second, GateBlocked, "login_backoff", 2)
	if !second.Failed {
		t.Error("GateBlocked is not marked as a failure")
	}

	// Blocked: no more requests, however long it goes on.
	for i := 0; i < 5; i++ {
		r.nextWindow(8 * time.Hour)
		if r.gate.Now().In(la(t)).Hour() >= 16 && r.gate.Now().In(la(t)).Hour() < 21 {
			r.nextWindow(6 * time.Hour)
		}
		res := r.ensure()
		if res.Outcome != GateBlocked || len(r.kube.requests) != 2 {
			t.Fatalf("poll %d after the block: outcome %d, requests %d", i, res.Outcome, len(r.kube.requests))
		}
		if r.slept != 0 {
			t.Fatalf("a blocked poll waited %s", r.slept)
		}
	}

	// A new process finds the same block in the checkpoint.
	r2 := newGateRig(t, true)
	r2.cp = r.cp
	r2.clock = r.clock.Add(24 * time.Hour)
	if h := r2.clock.In(la(t)).Hour(); h >= 15 && h < 21 {
		r2.clock = r2.clock.Add(7 * time.Hour)
	}
	r2.file = r2.session("old", SessionDead, ReasonSignInPage)
	r2.expect(r2.ensure(), GateBlocked, "login_backoff", 0)

	// The session file changes (an operator ran the login by hand) and the
	// new session works: back to normal, the count is gone.
	r2.file = r2.session("by-hand", SessionLive, ReasonSignedIn)
	r2.expect(r2.ensure(), GateLive, "", 0)
	if n, _ := loadUnanswered(r2.cp); n != 0 {
		t.Errorf("unanswered = %d after a live session", n)
	}
}

// A session the login workload confirmed but the connector's check refuses
// counts the same: after two of those, stop.
func TestGateStopsAskingWhenNewSessionsKeepBeingRefused(t *testing.T) {
	r := newGateRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	r.file = r.session("s0", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("s1", SessionDead, ReasonSignInPage), 20*time.Second
	r.expect(r.ensure(), GateSkip, "new_session_not_accepted", 1)

	r.nextWindow(9 * time.Hour)
	r.arrives, r.arriveAfter = r.session("s2", SessionDead, ReasonSignInPage), 20*time.Second
	r.expect(r.ensure(), GateBlocked, "login_backoff", 2)

	r.nextWindow(6*time.Hour + 30*time.Minute)
	r.expect(r.ensure(), GateBlocked, "login_backoff", 2)
}

// The file changed since the block, but to another dead session: that is
// still somebody's intervention, so asking resumes (and counts afresh).
func TestGateResumesWhenTheSessionFileChanges(t *testing.T) {
	r := newGateRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.ensure()
	r.nextWindow(9 * time.Hour)
	r.expect(r.ensure(), GateBlocked, "login_backoff", 2)

	r.nextWindow(6*time.Hour + 30*time.Minute)
	r.file = r.session("changed-but-dead", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateSkip, "no_new_session", 3)
	if n, _ := loadUnanswered(r.cp); n != 1 {
		t.Errorf("unanswered = %d, want counting to start again at 1", n)
	}
}

// The operator's reset: a new token clears the count once.
func TestGateBackoffResetToken(t *testing.T) {
	r := newGateRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.ensure()
	r.nextWindow(9 * time.Hour)
	r.expect(r.ensure(), GateBlocked, "login_backoff", 2)

	r.nextWindow(6*time.Hour + 30*time.Minute)
	r.gate.BackoffResetToken = "2026-10-13-try-again"
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 20*time.Second
	r.expect(r.ensure(), GateLive, "", 3)

	// The same token again does nothing: it is not a standing override.
	r.nextWindow(9 * time.Hour)
	r.file = r.session("dead-again", SessionDead, ReasonSignInPage)
	r.ensure()
	r.nextWindow(24 * time.Hour)
	r.expect(r.ensure(), GateBlocked, "login_backoff", 5)
	r.nextWindow(24 * time.Hour)
	r.expect(r.ensure(), GateBlocked, "login_backoff", 5)
}

// --- finding 5: the cookie overlay lasts one poll ---------------------------------

func TestGateResetsTheGatewayOverlayAtEveryPoll(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	r.ensure()
	// Built during the first poll, so the first reset is at the second.
	for i := 1; i <= 3; i++ {
		r.ensure()
		if r.resets != i {
			t.Fatalf("after poll %d: overlay resets = %d, want %d", i+1, r.resets, i)
		}
	}
}

// --- adoption, age --------------------------------------------------------------

// The gate adopts a session that changed under it at the next poll,
// without asking for anything.
func TestGateAdoptsAChangedSessionFile(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	first := r.ensure()
	r.file = r.session("s2", SessionLive, ReasonSignedIn)
	second := r.ensure()
	if first.Client == second.Client || second.Client != r.clients["s2"] || r.builds != 2 {
		t.Errorf("the new session's client was not adopted (builds = %d)", r.builds)
	}
	// An unchanged file does not rebuild the client.
	r.ensure()
	if r.builds != 2 {
		t.Errorf("builds = %d after an unchanged file", r.builds)
	}
	// A file that cannot be read keeps the session in use.
	r.file = nil
	r.expect(r.ensure(), GateLive, "", 0)
}

func TestGateReportsTheDeadSessionsAge(t *testing.T) {
	r := newGateRig(t, false)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	r.ensure()
	r.clock = r.clock.Add(31 * time.Minute)
	r.states["s1"] = SessionVerdict{SessionDead, ReasonSignInPage}
	r.ensure()
	if len(r.deadAges) != 1 || r.deadAges[0] != 31*time.Minute {
		t.Errorf("dead ages = %v, want [31m]", r.deadAges)
	}
	if got := r.gate.SessionAge(); got != 31*time.Minute {
		t.Errorf("SessionAge = %s", got)
	}
	var nilGate *SessionGate
	if nilGate.SessionAge() >= 0 || nilGate.OnDemand() {
		t.Error("a nil gate reports a session or on-demand login")
	}
}

// --- session document and config -------------------------------------------

func TestLoadSessionAdvisoryAndFileAge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	getenv := func(k string) string {
		if k == "POWERSCHOOL_SESSION_FILE" {
			return path
		}
		return ""
	}
	now := time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)

	if _, _, err := LoadSessionAdvisory(getenv, now); err == nil {
		t.Error("a missing file loaded")
	}
	if _, ok := SessionFileAge(getenv, now); ok {
		t.Error("a missing file has an age")
	}

	doc := `{"cookies":[{"Name":"JSESSIONID","Value":"v"}],"expires_at":"2026-10-12T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, expired, err := LoadSessionAdvisory(getenv, now)
	if err != nil || !expired || len(s.Cookies) != 1 {
		t.Errorf("an expired document: session %v, expired %v, err %v; want it returned and flagged", s != nil, expired, err)
	}
	// The strict loader still refuses it.
	if _, err := LoadSession(getenv, now); err == nil {
		t.Error("LoadSession accepted an expired document")
	}
	if _, expired, _ := LoadSessionAdvisory(getenv, now.Add(-2*time.Hour)); expired {
		t.Error("flagged expired before its expires_at")
	}

	stamp := now.Add(-90 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if age, ok := SessionFileAge(getenv, now); !ok || age != 90*time.Second {
		t.Errorf("file age = %s, %v", age, ok)
	}

	for _, bad := range []string{"{not json", `{"cookies":[],"expires_at":"2099-01-01T00:00:00Z"}`, `{"cookies":[{"Name":"a","Value":"b"}]}`} {
		_ = os.WriteFile(path, []byte(bad), 0o600)
		if _, _, err := LoadSessionAdvisory(getenv, now); err == nil {
			t.Errorf("malformed document %q loaded", bad)
		}
	}
	// Not delivered as a file: no age.
	if _, ok := SessionFileAge(func(string) string { return "" }, now); ok {
		t.Error("a session from the environment has a file age")
	}
}

func TestLoginOnDemandConfig(t *testing.T) {
	var c Config
	ApplyDefaults(&c)
	if c.LoginOnDemand.Enabled || c.LoginOnDemand.WaitSeconds != 600 || c.LoginOnDemand.MinIntervalMinutes != 20 {
		t.Errorf("defaults = %+v", c.LoginOnDemand)
	}
	if got := (LoginOnDemandConfig{MinIntervalMinutes: 5}).minInterval(); got != 20*time.Minute {
		t.Errorf("minInterval below the floor = %s", got)
	}
	if got := (LoginOnDemandConfig{MinIntervalMinutes: 45}).minInterval(); got != 45*time.Minute {
		t.Errorf("minInterval = %s", got)
	}
	for name, c := range map[string]struct {
		cfg LoginOnDemandConfig
		ok  bool
	}{
		"off, empty":         {LoginOnDemandConfig{}, true},
		"off, nonsense":      {LoginOnDemandConfig{WaitSeconds: 1}, true},
		"on, complete":       {LoginOnDemandConfig{Enabled: true, ConfigMapName: "x-login-request"}, true},
		"on, with namespace": {LoginOnDemandConfig{Enabled: true, ConfigMapName: "x", Namespace: "openclaw", WaitSeconds: 900, MinIntervalMinutes: 30}, true},
		"on, no configmap":   {LoginOnDemandConfig{Enabled: true}, false},
		"on, path in name":   {LoginOnDemandConfig{Enabled: true, ConfigMapName: "a/b"}, false},
		"on, path in ns":     {LoginOnDemandConfig{Enabled: true, ConfigMapName: "a", Namespace: "x/y"}, false},
		"on, wait too short": {LoginOnDemandConfig{Enabled: true, ConfigMapName: "a", WaitSeconds: 5}, false},
		"on, wait too long":  {LoginOnDemandConfig{Enabled: true, ConfigMapName: "a", WaitSeconds: 7200}, false},
		"on, interval low":   {LoginOnDemandConfig{Enabled: true, ConfigMapName: "a", MinIntervalMinutes: 5}, false},
	} {
		if err := validateLoginOnDemand(c.cfg); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", name, err, c.ok)
		}
	}
}

// --- D: the whole poll ------------------------------------------------------

// windowConfig is testConfig with the morning poll window the rig's clock
// sits in.
func windowConfig() Config {
	cfg := testConfig()
	cfg.PollSchedule = PollSchedule{WeekdaysOnly: true, Windows: []PollWindow{
		{Start: "06:00", End: "06:30"}, {Start: "15:00", End: "15:30"}, {Start: "21:30", End: "22:00"},
	}}
	return cfg
}

// newPollRig is a wired connector on a gate.
func newPollRig(t *testing.T, onDemand bool) (*gateRig, *PowerSchoolConnector, connector.Checkpoint, string) {
	t.Helper()
	r := newGateRig(t, onDemand)
	cfg := windowConfig()
	cfg.LoginOnDemand = r.gate.onDemand
	c, cp, staging := newWiredConnector(t, pollFixture("111111"), cfg)
	c.client = nil
	c.Gate = r.gate
	c.now = func() time.Time { return r.clock }
	r.cp = cp
	return r, c, cp, staging
}

// Dead session -> login requested -> new session -> items staged. The
// process does not exit and nothing is marked rejected.
func TestPollWithADeadSessionAsksForALoginAndThenStages(t *testing.T) {
	r, c, cp, staging := newPollRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 100*time.Second
	rejected := 0
	c.OnSessionRejected = func(time.Duration) { rejected++ }
	var ages []time.Duration
	c.OnSessionAge = func(age time.Duration) { ages = append(ages, age) }

	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if len(r.kube.requests) != 1 {
		t.Errorf("login requests = %d, want 1", len(r.kube.requests))
	}
	if n := len(readItems(t, staging)); n == 0 {
		t.Error("nothing was staged after the new session arrived")
	}
	if old := r.clients["old"]; old.HomeCalls != 0 || len(old.SwitchCalls) != 0 {
		t.Errorf("the dead session's client was used for a poll: %v", old.Log)
	}
	if r.clients["new"].HomeCalls == 0 {
		t.Error("the new session's client was not used")
	}
	if rejected != 0 {
		t.Error("the session was marked rejected although a login was requested")
	}
	if len(ages) != 1 {
		t.Errorf("dead-session ages reported = %v", ages)
	}
	if c.failedPolls != 0 {
		t.Errorf("failedPolls = %d after a poll that staged", c.failedPolls)
	}
}

// A request that leads nowhere loses the poll, VISIBLY: it is a failed
// poll (counted, logged as one), and the second in a row raises the
// connector's permanent-failure path.
func TestPollsLostToUnansweredRequestsAreFailuresAndThenPermanent(t *testing.T) {
	r, c, cp, staging := newPollRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	rec := recordMetrics(t, c)
	r.file = r.session("old", SessionDead, ReasonSignInPage)

	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("first lost poll: %v", err)
	}
	if c.failedPolls != 1 {
		t.Errorf("failedPolls = %d after one lost poll, want 1", c.failedPolls)
	}

	r.nextWindow(9 * time.Hour)
	err := c.pollNow(context.Background(), cp, "scheduled")
	if !connector.IsPermanent(err) {
		t.Fatalf("second lost poll: err = %v, want a PermanentError", err)
	}
	if !strings.Contains(err.Error(), "login requests in a row") || !strings.Contains(err.Error(), "POWERSCHOOL_LOGIN_BACKOFF_RESET") {
		t.Errorf("the error does not say what happened or what to do: %v", err)
	}

	// And it stays that way: no request, same error.
	r.nextWindow(6*time.Hour + 30*time.Minute)
	if err := c.pollNow(context.Background(), cp, "scheduled"); !connector.IsPermanent(err) {
		t.Fatalf("third poll: err = %v", err)
	}
	if len(r.kube.requests) != 2 {
		t.Errorf("requests = %d, want 2 and no more", len(r.kube.requests))
	}
	if n := len(readItems(t, staging)); n != 0 {
		t.Errorf("%d items staged without a session", n)
	}
	counters := rec.counters()
	if counters["powerschool_polls_skipped_total{reason=no_new_session}"] != 1 ||
		counters["powerschool_polls_skipped_total{reason=login_backoff}"] != 2 {
		t.Errorf("skip metrics = %v", counters)
	}
}

// failure_threshold applies to lost polls as to any poll that read nobody.
func TestLostPollsCountTowardTheFailureThreshold(t *testing.T) {
	r, c, cp, _ := newPollRig(t, true)
	c.cfg.FailureThreshold = 2
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.kube.err = &kubecm.Error{Op: "patch configmap", Status: 403, Reason: "Forbidden"}

	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("first: %v", err)
	}
	r.nextWindow(9 * time.Hour)
	_ = cp.Delete(lastLoginRequestKey) // let it try (and fail) again
	err := c.pollNow(context.Background(), cp, "scheduled")
	if !connector.IsPermanent(err) || !strings.Contains(err.Error(), "consecutive polls") {
		t.Fatalf("err = %v, want the failure-threshold PermanentError", err)
	}
}

// Unknown: skip, change nothing, ask for nothing, and it is not a failure.
func TestPollIsSkippedWhenTheSessionStateIsUnknown(t *testing.T) {
	for _, onDemand := range []bool{true, false} {
		r, c, cp, staging := newPollRig(t, onDemand)
		r.file = r.session("s1", SessionUnknown, ReasonNetwork)
		rejected := 0
		c.OnSessionRejected = func(time.Duration) { rejected++ }
		for i := 0; i < 8; i++ { // more than failure_threshold
			if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
				t.Fatalf("pollNow: %v", err)
			}
		}
		if len(r.kube.requests) != 0 || rejected != 0 || len(readItems(t, staging)) != 0 || r.clients["s1"].HomeCalls != 0 || c.failedPolls != 0 {
			t.Errorf("on-demand %v: requests %d, rejected %d, home calls %d, failed polls %d",
				onDemand, len(r.kube.requests), rejected, r.clients["s1"].HomeCalls, c.failedPolls)
		}
	}
}

// A lost poll consumes its window. A restart inside that window does not
// poll again, so it does not ask again; the next window does.
func TestASkippedPollConsumesItsWindowAndCatchUpDoesNotAskAgain(t *testing.T) {
	r, c, cp, _ := newPollRig(t, true)
	r.gate.onDemand.WaitSeconds = 60
	r.file = r.session("old", SessionDead, ReasonSignInPage)

	// The process starts at 06:10 having missed nothing recorded: catch-up.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(r.kube.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(r.kube.requests))
	}
	last, ok := loadLastPoll(cp)
	if !ok || !last.Equal(sixAM(t)) {
		t.Fatalf("the lost poll was not recorded as this window's poll: %v %v", last, ok)
	}
	// The next scheduled poll is not in this window.
	if next := c.nextPoll(c.scheduleFrom(r.clock)); next.In(la(t)).Hour() != 15 {
		t.Errorf("next scheduled poll at %s, want the 15:00 window", next.In(la(t)).Format("15:04"))
	}

	// Restart at 06:20, same checkpoint: the catch-up Poll does nothing.
	r2, c2, _, _ := newPollRig(t, true)
	r2.cp = cp
	r2.clock = sixAM(t).Add(10 * time.Minute)
	r2.file = r2.session("old", SessionDead, ReasonSignInPage)
	if err := c2.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll after restart: %v", err)
	}
	if len(r2.kube.requests) != 0 || r2.probes != 0 {
		t.Errorf("the restarted process asked again in the same window: requests %d, probes %d", len(r2.kube.requests), r2.probes)
	}
	// The framework's periodic re-poll is a no-op too.
	if err := c2.Poll(context.Background(), cp); err != nil || len(r2.kube.requests) != 0 {
		t.Errorf("a framework re-poll asked for a login: %v", err)
	}
}

// Without on-demand login a dead session is still found BEFORE any kid is
// read, reported as a session and not as a parse error, and takes the
// rejected path.
func TestPollWithADeadSessionAndNoOnDemandTakesTheRejectedPath(t *testing.T) {
	r, c, cp, _ := newPollRig(t, false)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	r.clock = r.clock.Add(64 * time.Minute)
	r.states["s1"] = SessionVerdict{SessionDead, ReasonGatewayPage}
	homeBefore := r.clients["s1"].HomeCalls
	var ages []time.Duration
	c.OnSessionRejected = func(age time.Duration) { ages = append(ages, age) }

	err := c.pollNow(context.Background(), cp, "scheduled")
	if !connector.IsPermanent(err) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "parse") {
		t.Errorf("a dead session was reported as a parse failure: %v", err)
	}
	if len(ages) != 1 || ages[0] != 64*time.Minute {
		t.Errorf("rejected ages = %v, want [1h4m]", ages)
	}
	if r.clients["s1"].HomeCalls != homeBefore {
		t.Error("a kid was read with the dead session")
	}
	if len(r.kube.requests) != 0 {
		t.Error("a login was requested with on-demand off")
	}
}

// The session dies in the middle of a poll. With on-demand login the
// process does not exit; the next poll asks for a new session.
func TestSessionDyingMidPollDoesNotExitWithOnDemand(t *testing.T) {
	r, c, cp, _ := newPollRig(t, true)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	r.clients["s1"].HomeFunc = func(context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
	rejected := 0
	c.OnSessionRejected = func(time.Duration) { rejected++ }
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("mid-poll expiry with on-demand: %v", err)
	}
	if rejected != 0 {
		t.Error("marked rejected")
	}

	// Next poll: the check finds it dead and asks.
	r.states["s1"] = SessionVerdict{SessionDead, ReasonSignInPage}
	r.arrives, r.arriveAfter = r.session("s2", SessionLive, ReasonSignedIn), 30*time.Second
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if len(r.kube.requests) != 1 || r.clients["s2"].HomeCalls == 0 {
		t.Errorf("requests = %d; new client used = %v", len(r.kube.requests), r.clients["s2"] != nil && r.clients["s2"].HomeCalls > 0)
	}
}

func TestNewConnectorWithGate(t *testing.T) {
	if _, err := NewConnectorWithGate(nil, testConfig(), "v"); err == nil {
		t.Error("a nil gate was accepted")
	}
	r := newGateRig(t, true)
	c, err := NewConnectorWithGate(r.gate, testConfig(), "v")
	if err != nil || c.Gate != r.gate || c.client != nil {
		t.Errorf("NewConnectorWithGate = %v, %v", c, err)
	}
}
