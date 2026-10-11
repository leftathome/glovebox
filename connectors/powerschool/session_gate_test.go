package powerschool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
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

// fakeRequester is the Kubernetes side.
type fakeRequester struct {
	jobs      []KubeJob // what ListJobs returns
	listErr   error
	createErr error
	// jobState is what GetJob reports for a created Job.
	jobState KubeJob
	getErr   error

	lists, creates, gets int
	createdFrom          []string
	selectors            []string
	// onCreate runs when a Job is created.
	onCreate func()
}

func (f *fakeRequester) ListJobs(_ context.Context, selector string) ([]KubeJob, error) {
	f.lists++
	f.selectors = append(f.selectors, selector)
	return f.jobs, f.listErr
}

func (f *fakeRequester) CreateJobFromCronJob(_ context.Context, cronJob string, now time.Time) (string, error) {
	f.creates++
	f.createdFrom = append(f.createdFrom, cronJob)
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.onCreate != nil {
		f.onCreate()
	}
	return onDemandJobName(cronJob, now), nil
}

func (f *fakeRequester) GetJob(_ context.Context, name string) (KubeJob, error) {
	f.gets++
	j := f.jobState
	j.Name = name
	return j, f.getErr
}

// gateRig is a SessionGate on fakes: a session "file", a portal that knows
// which sessions it accepts, a Kubernetes API and a clock.
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

	probes, builds int
	deadAges       []time.Duration
	clients        map[string]*fakeClient
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
		Build: func(s *pslib.SessionExport) (PowerSchoolClient, ProbeFunc, error) {
			r.builds++
			id := s.Cookies[0].Value
			client := pollFixture("111111")
			r.clients[id] = client
			return client, func(context.Context) SessionVerdict {
				r.probes++
				if v, ok := r.states[id]; ok {
					return v
				}
				return SessionVerdict{SessionUnknown, ReasonNetwork}
			}, nil
		},
		FileAge: func(time.Time) (time.Duration, bool) { return r.fileAge, r.file != nil },
	}
	cfg := LoginOnDemandConfig{Enabled: onDemand, CronJobName: "glovebox-glovebox-powerschool-login-morning"}
	c := Config{LoginOnDemand: cfg}
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

func (r *gateRig) expect(res GateResult, outcome GateOutcome, reason string, creates int) {
	r.t.Helper()
	if res.Outcome != outcome || (reason != "" && res.Reason != reason) {
		r.t.Errorf("Ensure = outcome %d reason %q, want outcome %d reason %q", res.Outcome, res.Reason, outcome, reason)
	}
	if r.kube.creates != creates {
		r.t.Errorf("login Jobs created = %d, want %d", r.kube.creates, creates)
	}
}

// --- A: classification ------------------------------------------------------

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
		{"redirected to the sign-in page", "/public/home.html", 200, signIn, SessionDead, ReasonSignInPage},
		{"redirected under /public/, whatever is there", "/public/", 200, "<html></html>", SessionDead, ReasonSignInPage},
		{"the sign-in form served at the home URL", "/guardian/home.html", 200, signIn, SessionDead, ReasonSignInPage},
		{"the gateway's error page", "/my.logout.php3", 200, "<html>Your session could not be established.</html>", SessionDead, ReasonGatewayPage},
		{"the gateway's policy page", "/my.policy", 200, "<html><noscript>js</noscript></html>", SessionDead, ReasonGatewayPage},
		{"401", "/guardian/home.html", 401, "", SessionDead, ReasonRefusedStatus},
		{"403", "/guardian/home.html", 403, "", SessionDead, ReasonRefusedStatus},
		{"503", "/guardian/home.html", 503, "", SessionUnknown, ReasonServerError},
		{"500 with a sign-in form in it", "/guardian/home.html", 500, signIn, SessionUnknown, ReasonServerError},
		{"404", "/guardian/home.html", 404, "", SessionUnknown, "http_404"},
		{"a maintenance notice", "/guardian/home.html", 200, "<html>Down for maintenance</html>", SessionUnknown, ReasonUnexpectedPage},
		{"a page that only mentions passwords", "/guardian/home.html", 200, "<p>Forgot your password? Call the office.</p>", SessionUnknown, ReasonUnexpectedPage},
		{"an empty body", "/guardian/home.html", 200, "", SessionUnknown, ReasonUnexpectedPage},
	} {
		got := ClassifyHomeResponse(c.path, c.status, c.body)
		if got.State != c.state || got.Reason != c.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, got.State, got.Reason, c.state, c.reason)
		}
	}
}

// --- B: request limits ------------------------------------------------------

func TestGateLiveSessionPollsWithoutTouchingKubernetes(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("s1", SessionLive, ReasonSignedIn)
	res := r.ensure()
	r.expect(res, GateLive, "", 0)
	if res.Client != r.clients["s1"] || r.probes != 1 || r.kube.lists != 0 {
		t.Errorf("client/probes/lists = %v/%d/%d", res.Client != nil, r.probes, r.kube.lists)
	}
	if len(r.deadAges) != 0 {
		t.Error("a live session was reported dead")
	}
}

// Unknown is not dead: the poll is skipped, nothing changes, nobody is
// asked for a login.
func TestGateUnknownSkipsAndRequestsNothing(t *testing.T) {
	for _, onDemand := range []bool{true, false} {
		r := newGateRig(t, onDemand)
		r.file = r.session("s1", SessionUnknown, ReasonServerError)
		res := r.ensure()
		r.expect(res, GateSkip, "session_state_unknown:server_error", 0)
		if r.kube.lists != 0 || len(r.deadAges) != 0 {
			t.Errorf("on-demand %v: lists = %d, dead reports = %d", onDemand, r.kube.lists, len(r.deadAges))
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

// The whole on-demand path: dead -> one Job -> the new session arrives ->
// it is checked -> poll.
func TestGateDeadSessionAsksForALoginAndWaitsForTheNewOne(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 95*time.Second

	res := r.ensure()
	r.expect(res, GateLive, "", 1)
	if res.Client != r.clients["new"] {
		t.Error("the poll was not given the new session's client")
	}
	if r.kube.createdFrom[0] != "glovebox-glovebox-powerschool-login-morning" {
		t.Errorf("Job created from %q", r.kube.createdFrom[0])
	}
	if r.kube.selectors[0] != "glovebox.io/powerschool-login=true" {
		t.Errorf("active-Job check used selector %q", r.kube.selectors[0])
	}
	if r.slept != 95*time.Second {
		t.Errorf("waited %s, want 95s in 5s steps", r.slept)
	}
	if r.probes != 2 {
		t.Errorf("portal reads = %d, want 2: the dead session once, the new one once", r.probes)
	}
	raw, ok := r.cp.Load(lastLoginRequestKey)
	if !ok || raw != sixAM(t).UTC().Format(time.RFC3339) {
		t.Errorf("recorded request time = %q, %v", raw, ok)
	}
	if len(r.deadAges) != 1 {
		t.Errorf("dead reports = %v", r.deadAges)
	}
}

func TestGateNewSessionNeverAppears(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.kube.jobState = KubeJob{Complete: true, Succeeded: 1}
	res := r.ensure()
	r.expect(res, GateSkip, "no_new_session", 1)
	if r.slept != 360*time.Second {
		t.Errorf("waited %s, want the default 360s", r.slept)
	}
	if r.probes != 1 {
		t.Errorf("portal reads while waiting: %d, want only the first check", r.probes)
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
// poll: no second Job.
func TestGateNewSessionAppearsButIsDead(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("also-dead", SessionDead, ReasonGatewayPage), 30*time.Second
	r.expect(r.ensure(), GateSkip, "new_session_not_accepted", 1)
	if len(r.deadAges) != 2 {
		t.Errorf("dead reports = %d, want 2", len(r.deadAges))
	}
}

func TestGateLoginJobFailedEndsTheWaitEarly(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.kube.jobState = KubeJob{FailedCondition: true, Failed: 1, Reason: "BackoffLimitExceeded"}
	r.expect(r.ensure(), GateSkip, "login_job_failed", 1)
	if r.slept >= 60*time.Second {
		t.Errorf("waited %s after the Job failed", r.slept)
	}
}

// At most one request per 20 minutes, across polls and across restarts (the
// time is in the checkpoint).
func TestGateAsksAtMostOncePerTwentyMinutes(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateSkip, "no_new_session", 1) // 6 minutes pass

	r.clock = r.clock.Add(4 * time.Minute) // 10 minutes after the request
	r.expect(r.ensure(), GateSkip, "login_requested_recently", 1)
	if r.kube.lists != 1 {
		t.Errorf("Jobs were listed again inside the interval: %d", r.kube.lists)
	}

	// A new process, same checkpoint directory contents (same cp).
	r2 := newGateRig(t, true)
	r2.cp = r.cp
	r2.clock = r.clock.Add(9*time.Minute + 59*time.Second) // 19:59 after
	r2.file = r2.session("old", SessionDead, ReasonSignInPage)
	r2.expect(r2.ensure(), GateSkip, "login_requested_recently", 0)

	r2.clock = sixAM(t).Add(20 * time.Minute)
	r2.arrives, r2.arriveAfter = r2.session("new", SessionLive, ReasonSignedIn), 10*time.Second
	r2.expect(r2.ensure(), GateLive, "", 1)
}

// A restart in the middle of a wait: the request was made a minute ago.
// No second Job; the rest of the wait is waited.
func TestGateRestartInMidWaitDoesNotAskAgain(t *testing.T) {
	r := newGateRig(t, true)
	if err := r.cp.Save(lastLoginRequestKey, r.clock.Add(-time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 40*time.Second
	r.expect(r.ensure(), GateLive, "", 0)
	if r.kube.lists != 0 {
		t.Error("Jobs were listed although a request is outstanding")
	}

	r = newGateRig(t, true)
	_ = r.cp.Save(lastLoginRequestKey, r.clock.Add(-time.Minute).UTC().Format(time.RFC3339))
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.expect(r.ensure(), GateSkip, "no_new_session", 0)
	if r.slept != 5*time.Minute {
		t.Errorf("waited %s, want the remaining 5m of the 6m", r.slept)
	}
}

// No Job is created from 15:57:30 to 21:00 Pacific: the login workload
// would refuse, and outside the test the owner may be signed in.
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
				if r.kube.lists != 0 {
					t.Errorf("%d/%d %02d:%02d:%02d: the API was contacted during the blackout", day[0], day[1], c.h, c.m, c.s)
				}
				if _, ok := r.cp.Load(lastLoginRequestKey); ok {
					t.Error("a request was recorded during the blackout")
				}
			} else if r.kube.creates != 1 {
				t.Errorf("%d/%d %02d:%02d:%02d: creates = %d, want 1", day[0], day[1], c.h, c.m, c.s, r.kube.creates)
			}
		}
	}
}

// A login Job, scheduled or on demand, is already running: wait for its
// session instead of starting another login.
func TestGateDoesNotAskWhileALoginJobIsActive(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.kube.jobs = []KubeJob{
		{Name: "glovebox-glovebox-powerschool-login-morning-29001234", Complete: true, Succeeded: 1},
		{Name: "glovebox-glovebox-powerschool-login-evening-29005678", Active: 1},
	}
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 20*time.Second
	r.expect(r.ensure(), GateLive, "", 0)
	if _, ok := r.cp.Load(lastLoginRequestKey); ok {
		t.Error("waiting for somebody else's Job was recorded as a request")
	}

	// Finished Jobs do not count as active.
	r = newGateRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.kube.jobs = []KubeJob{{Name: "a", Complete: true}, {Name: "b", FailedCondition: true}}
	r.gate.onDemand.WaitSeconds = 30
	r.expect(r.ensure(), GateSkip, "no_new_session", 1)
}

// The session file was written less than two minutes ago and is already
// refused: a login has just happened. Another one will not help.
func TestGateDoesNotAskWhenTheSessionFileIsFresh(t *testing.T) {
	r := newGateRig(t, true)
	r.file = r.session("just-written", SessionDead, ReasonSignInPage)
	r.fileAge = 119 * time.Second
	r.expect(r.ensure(), GateSkip, "session_file_fresh", 0)
	if r.kube.lists != 0 {
		t.Error("the API was contacted")
	}

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

func TestGateKubernetesFailuresSkipThePoll(t *testing.T) {
	forbidden := &KubeError{Op: "create job", Status: 403, Reason: "Forbidden"}
	for name, setup := range map[string]func(*fakeRequester){
		"list forbidden":   func(k *fakeRequester) { k.listErr = &KubeError{Op: "list jobs", Status: 403, Reason: "Forbidden"} },
		"list unavailable": func(k *fakeRequester) { k.listErr = errors.New("dial tcp: connection refused") },
		"create forbidden": func(k *fakeRequester) { k.createErr = forbidden },
		"create conflict": func(k *fakeRequester) {
			k.createErr = &KubeError{Op: "create job", Status: 409, Reason: "AlreadyExists"}
		},
		"create unavailable": func(k *fakeRequester) {
			k.createErr = &KubeError{Op: "create job", Status: 503, Reason: "ServiceUnavailable"}
		},
		"cronjob missing": func(k *fakeRequester) { k.createErr = &KubeError{Op: "get cronjob", Status: 404, Reason: "NotFound"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newGateRig(t, true)
			r.file = r.session("old", SessionDead, ReasonSignInPage)
			setup(r.kube)
			res := r.ensure()
			if res.Outcome != GateSkip || res.Reason != "login_request_failed" {
				t.Errorf("Ensure = %d %q", res.Outcome, res.Reason)
			}
			if r.slept != 0 {
				t.Errorf("waited %s for a session nobody is producing", r.slept)
			}
			// A create that failed is still a request as far as the limit
			// goes: its answer may have been lost on the way back.
			_, recorded := r.cp.Load(lastLoginRequestKey)
			if wantRecorded := strings.HasPrefix(name, "create") || name == "cronjob missing"; recorded != wantRecorded {
				t.Errorf("request recorded = %v, want %v", recorded, wantRecorded)
			}
		})
	}
}

// The gate adopts a session that changed under it (a scheduled login) at
// the next poll, without asking for anything.
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
	r.clock = r.clock.Add(63 * time.Minute)
	r.states["s1"] = SessionVerdict{SessionDead, ReasonSignInPage}
	r.ensure()
	if len(r.deadAges) != 1 || r.deadAges[0] != 63*time.Minute {
		t.Errorf("dead ages = %v, want [1h3m]", r.deadAges)
	}
	if got := r.gate.SessionAge(); got != 63*time.Minute {
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
	if c.LoginOnDemand.Enabled || c.LoginOnDemand.WaitSeconds != 360 || c.LoginOnDemand.MinIntervalMinutes != 20 {
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
		"on, complete":       {LoginOnDemandConfig{Enabled: true, CronJobName: "x-login-morning"}, true},
		"on, with namespace": {LoginOnDemandConfig{Enabled: true, CronJobName: "x", Namespace: "openclaw", WaitSeconds: 600, MinIntervalMinutes: 30}, true},
		"on, no cronjob":     {LoginOnDemandConfig{Enabled: true}, false},
		"on, path in name":   {LoginOnDemandConfig{Enabled: true, CronJobName: "a/b"}, false},
		"on, path in ns":     {LoginOnDemandConfig{Enabled: true, CronJobName: "a", Namespace: "x/y"}, false},
		"on, wait too short": {LoginOnDemandConfig{Enabled: true, CronJobName: "a", WaitSeconds: 5}, false},
		"on, wait too long":  {LoginOnDemandConfig{Enabled: true, CronJobName: "a", WaitSeconds: 7200}, false},
		"on, interval low":   {LoginOnDemandConfig{Enabled: true, CronJobName: "a", MinIntervalMinutes: 5}, false},
	} {
		if err := validateLoginOnDemand(c.cfg); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", name, err, c.ok)
		}
	}
}

// --- D: the whole poll ------------------------------------------------------

// pollRig is a wired connector on a gate.
func newPollRig(t *testing.T, onDemand bool) (*gateRig, *PowerSchoolConnector, connector.Checkpoint, string) {
	t.Helper()
	r := newGateRig(t, onDemand)
	cfg := testConfig()
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
func TestPollWithADeadSessionRequestsALoginAndThenStages(t *testing.T) {
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
	if r.kube.creates != 1 {
		t.Errorf("login Jobs created = %d, want 1", r.kube.creates)
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
}

// The login does not produce a session in time: the poll is skipped, it is
// not an error, it is not a failed poll, and the process stays up.
func TestPollIsSkippedWhenNoSessionCanBeHad(t *testing.T) {
	r, c, cp, staging := newPollRig(t, true)
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	for i := 0; i < 8; i++ { // more than failure_threshold
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		r.clock = r.clock.Add(time.Hour)
	}
	if c.failedPolls != 0 {
		t.Errorf("skipped polls were counted as failed: %d", c.failedPolls)
	}
	if n := len(readItems(t, staging)); n != 0 {
		t.Errorf("%d items staged without a session", n)
	}
	if r.clients["old"].HomeCalls != 0 {
		t.Error("the dead session was used to read a kid")
	}
}

// Unknown: skip, change nothing, ask for nothing.
func TestPollIsSkippedWhenTheSessionStateIsUnknown(t *testing.T) {
	for _, onDemand := range []bool{true, false} {
		r, c, cp, staging := newPollRig(t, onDemand)
		r.file = r.session("s1", SessionUnknown, ReasonNetwork)
		rejected := 0
		c.OnSessionRejected = func(time.Duration) { rejected++ }
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatalf("pollNow: %v", err)
		}
		if r.kube.creates != 0 || rejected != 0 || len(readItems(t, staging)) != 0 || r.clients["s1"].HomeCalls != 0 {
			t.Errorf("on-demand %v: creates %d, rejected %d, home calls %d", onDemand, r.kube.creates, rejected, r.clients["s1"].HomeCalls)
		}
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
	if r.kube.creates != 0 {
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
	if r.kube.creates != 1 || r.clients["s2"].HomeCalls == 0 {
		t.Errorf("creates = %d; new client used = %v", r.kube.creates, r.clients["s2"] != nil && r.clients["s2"].HomeCalls > 0)
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
	_ = fmt.Sprint(c)
}
