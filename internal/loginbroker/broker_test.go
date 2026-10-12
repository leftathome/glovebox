package loginbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/dailywindow"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
	"github.com/leftathome/glovebox/internal/loginvault"
)

// Values that must never reach a log line or an answer's code.
const (
	testUsername = "parent-account-7731"
	testPassword = "correct-horse-battery-staple-9"
	cookiePrefix = "cookie-secret-"
)

// --- fakes ------------------------------------------------------------------

// backing is what survives a broker: the store's two secrets.
type backing struct {
	mu      sync.Mutex
	fields  map[string]any
	version int
	state   *loginvault.State
	// stateVersion is the KV version of the state record; writes are
	// check-and-set against it, as in Vault.
	stateVersion int
	// onStateWrite, when set, runs before each state write is applied (to
	// let "somebody else" write in between).
	onStateWrite       func(n int)
	stateWriteAttempts int

	sessionWrites, stateWrites int
	failSessionWrites          int   // this many session writes fail (5xx-like)
	failStateWrites            error // every state write fails with this
	casMismatchOnce            bool
	cannotWrite                bool
	connects, closes           int
	failConnect                error
}

type memStore struct{ b *backing }

func (s memStore) ReadFields(context.Context) (map[string]any, time.Time, int, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	var at time.Time
	if raw, ok := s.b.fields["logged_in_at"].(string); ok {
		at, _ = time.Parse(time.RFC3339, raw)
	}
	return s.b.fields, at, s.b.version, nil
}

func (s memStore) CanWrite(context.Context) (bool, error) { return !s.b.cannotWrite, nil }

func (s memStore) Write(_ context.Context, fields map[string]any, cas int) (int, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.b.failSessionWrites > 0 {
		s.b.failSessionWrites--
		return 0, errors.New("vault: 503 sealed")
	}
	if s.b.casMismatchOnce {
		s.b.casMismatchOnce = false
		s.b.version++ // somebody else wrote
		return 0, errors.New("check-and-set parameter did not match the current version")
	}
	if cas != s.b.version {
		return 0, errors.New("check-and-set parameter did not match the current version")
	}
	s.b.fields = fields
	s.b.version++
	s.b.sessionWrites++
	return s.b.version, nil
}

func (s memStore) ReadState(context.Context) (*loginvault.State, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.b.state == nil {
		return &loginvault.State{Version: s.b.stateVersion}, nil
	}
	c := *s.b.state
	c.Version = s.b.stateVersion
	return &c, nil
}

func (s memStore) WriteState(_ context.Context, state *loginvault.State) error {
	s.b.mu.Lock()
	s.b.stateWriteAttempts++
	hook, n := s.b.onStateWrite, s.b.stateWriteAttempts
	s.b.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.b.failStateWrites != nil {
		return s.b.failStateWrites
	}
	if state.Version != s.b.stateVersion {
		return errors.New("check-and-set parameter did not match the current version")
	}
	s.b.stateVersion++
	state.Version = s.b.stateVersion
	c := *state
	s.b.state = &c
	s.b.stateWrites++
	return nil
}

// otherWriter changes the state record as another workload would: a write
// of its own, which moves the version on.
func (b *backing) otherWriter(change func(s *loginvault.State)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil {
		b.state = &loginvault.State{}
	}
	change(b.state)
	b.stateVersion++
}

func (s memStore) Close() {
	s.b.mu.Lock()
	s.b.closes++
	s.b.mu.Unlock()
}

type fakeSession struct{ id string }

// fakeDriver is a site whose sessions are names and whose portal is a map.
type fakeDriver struct {
	mu     sync.Mutex
	states map[string]ProbeState // session id -> what the site says
	// logins are consumed one per Login call; none left = an unclassified
	// error.
	logins     []func(creds Credentials) (Session, error)
	loginCalls int
	probeCalls []string
	seenCreds  []Credentials
	limits     *Constraints
	// onProbe and onLogin run at the start of each call (to let time pass).
	onProbe func(id string)
	onLogin func(ctx context.Context)
}

func (d *fakeDriver) Site() string { return "testsite" }

func (d *fakeDriver) Login(ctx context.Context, creds Credentials) (Session, error) {
	if d.onLogin != nil {
		d.onLogin(ctx)
	}
	d.mu.Lock()
	d.loginCalls++
	d.seenCreds = append(d.seenCreds, creds)
	var next func(Credentials) (Session, error)
	if len(d.logins) > 0 {
		next, d.logins = d.logins[0], d.logins[1:]
	}
	d.mu.Unlock()
	if next == nil {
		return nil, errors.New("unexpected login with password " + creds.Password)
	}
	return next(creds)
}

func (d *fakeDriver) Probe(_ context.Context, s Session) ProbeResult {
	if d.onProbe != nil {
		d.onProbe(s.(*fakeSession).id)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	fs := s.(*fakeSession)
	d.probeCalls = append(d.probeCalls, fs.id)
	switch d.states[fs.id] {
	case ProbeAlive:
		return ProbeResult{ProbeAlive, "signed_in"}
	case ProbeDead:
		return ProbeResult{ProbeDead, "sign_in_page"}
	}
	return ProbeResult{ProbeUnknown, "network"}
}

func (d *fakeDriver) Encode(s Session, _ time.Time) (map[string]string, error) {
	fs := s.(*fakeSession)
	return map[string]string{"session": cookiePrefix + fs.id}, nil
}

func (d *fakeDriver) Decode(fields map[string]string) (Session, error) {
	id, ok := strings.CutPrefix(fields["session"], cookiePrefix)
	if !ok {
		return nil, errors.New("not a session")
	}
	return &fakeSession{id: id}, nil
}

func (d *fakeDriver) Fingerprint(s Session) string {
	sum := sha256.Sum256([]byte(s.(*fakeSession).id))
	return hex.EncodeToString(sum[:])
}

func (d *fakeDriver) set(id string, state ProbeState) {
	d.mu.Lock()
	d.states[id] = state
	d.mu.Unlock()
}

// succeed queues a login that produces the session id, alive.
func (d *fakeDriver) succeed(id string) {
	d.logins = append(d.logins, func(Credentials) (Session, error) {
		d.set(id, ProbeAlive)
		return &fakeSession{id: id}, nil
	})
}

func (d *fakeDriver) failWith(kind LoginFailure, detail string) {
	d.logins = append(d.logins, func(c Credentials) (Session, error) {
		// The wrapped error quotes the password, as a browser driver's
		// might. It must go nowhere.
		return nil, &LoginError{Kind: kind, Detail: detail, Err: errors.New("typed " + c.Password + " for " + c.Username)}
	})
}

type constrainedDriver struct{ *fakeDriver }

func (d constrainedDriver) Constraints() Constraints { return *d.limits }

func fp(id string) string {
	sum := sha256.Sum256([]byte(id))
	return brokerapi.ShortFingerprint(hex.EncodeToString(sum[:]))
}

// rig is a Broker on fakes with a clock the test moves.
type rig struct {
	t      *testing.T
	b      *Broker
	driver *fakeDriver
	back   *backing
	logs   *bytes.Buffer
	clock  time.Time
	slept  []time.Duration
	creds  Credentials
	tune   func(*Config)
}

var losAngeles = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// morning is a Monday 06:05 Pacific, far from any blackout used here.
func morning() time.Time { return time.Date(2026, 10, 12, 6, 5, 0, 0, losAngeles) }

func newRig(t *testing.T, tune func(*Config)) *rig {
	t.Helper()
	r := &rig{
		t:      t,
		driver: &fakeDriver{states: map[string]ProbeState{}},
		back:   &backing{},
		logs:   &bytes.Buffer{},
		clock:  morning(),
		creds:  Credentials{Username: testUsername, Password: testPassword},
		tune:   tune,
	}
	r.start()
	return r
}

// start builds a Broker on the rig's backing: a first start, or a restart.
func (r *rig) start() {
	r.t.Helper()
	cfg := Config{
		Driver:      r.driver,
		Credentials: r.creds,
		Connect: func(context.Context) (Store, error) {
			r.back.mu.Lock()
			defer r.back.mu.Unlock()
			r.back.connects++
			if r.back.failConnect != nil {
				return nil, r.back.failConnect
			}
			return memStore{r.back}, nil
		},
		StatePath: "secret/test/login-state",
		Logger:    slog.New(slog.NewJSONHandler(&syncWriter{w: r.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:   NewMetrics("testsite"),
		Now:       func() time.Time { return r.clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			r.slept = append(r.slept, d)
			r.clock = r.clock.Add(d)
			return nil
		},
		NewSalt: func() (string, error) { return "salt-for-tests", nil },
	}
	if r.tune != nil {
		r.tune(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	r.b = b
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (r *rig) ask(fingerprint string) *brokerapi.Response {
	r.t.Helper()
	if fingerprint == "" {
		fingerprint = brokerapi.NoFingerprint
	}
	resp, err := r.b.Refresh(context.Background(), brokerapi.Request{Reason: brokerapi.ReasonSessionDead, Fingerprint: fingerprint})
	if err != nil {
		r.t.Fatalf("Refresh: %v", err)
	}
	return resp
}

func (r *rig) want(resp *brokerapi.Response, outcome, code string) {
	r.t.Helper()
	if resp.Outcome != outcome || resp.Code != code {
		r.t.Errorf("answer = %s/%s (detail %q), want %s/%s", resp.Outcome, resp.Code, resp.Detail, outcome, code)
	}
	if (resp.Session != nil) != (outcome == brokerapi.OutcomeRefreshed || outcome == brokerapi.OutcomeCurrent) {
		r.t.Errorf("outcome %s: session present = %v", outcome, resp.Session != nil)
	}
}

// store puts a session into the store as an earlier login would have.
func (r *rig) store(id string, state ProbeState, age time.Duration) {
	r.driver.set(id, state)
	r.back.fields = map[string]any{
		"session":      cookiePrefix + id,
		"logged_in_at": r.clock.Add(-age).UTC().Format(time.RFC3339),
	}
	r.back.version++
}

func (r *rig) storedID() string {
	s, _ := r.back.fields["session"].(string)
	return strings.TrimPrefix(s, cookiePrefix)
}

// --- probe before login -----------------------------------------------------

// The stored session is alive: it is returned, and nobody logs in. This is
// the rule that keeps the broker from ever ending a live session.
func TestALiveStoredSessionIsReturnedWithoutLoggingIn(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeAlive, 3*time.Hour)

	// Whether the caller reports that very session dead, another one, or
	// none at all: the site says it is alive, and the site is who knows.
	for _, callerFP := range []string{fp("s1"), fp("older"), ""} {
		resp := r.ask(callerFP)
		r.want(resp, brokerapi.OutcomeCurrent, "")
		if resp.Session["session"] != cookiePrefix+"s1" || resp.Fingerprint != fp("s1") {
			t.Errorf("caller %q: got session %q fingerprint %q", callerFP, resp.Session["session"], resp.Fingerprint)
		}
	}
	if r.driver.loginCalls != 0 {
		t.Fatalf("logins = %d over a live session", r.driver.loginCalls)
	}
	if len(r.driver.probeCalls) != 3 || r.back.sessionWrites != 0 || r.back.stateWrites != 0 {
		t.Errorf("probes = %d, session writes = %d, state writes = %d", len(r.driver.probeCalls), r.back.sessionWrites, r.back.stateWrites)
	}
}

// Could not tell: no login. A login would end the session if it is alive.
func TestAnUnansweredProbeNeverLeadsToALogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeUnknown, 3*time.Hour)
	resp := r.ask(fp("s1"))
	r.want(resp, brokerapi.OutcomeInconclusive, brokerapi.CodeProbeInconclusive)
	if resp.Detail != "network" || r.driver.loginCalls != 0 {
		t.Errorf("detail = %q, logins = %d", resp.Detail, r.driver.loginCalls)
	}
}

// Dead session -> one login -> confirmed -> stored where the scheduled job
// stores it -> returned.
func TestADeadSessionIsReplacedByOneLogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("new")

	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeRefreshed, "")
	if resp.Session["session"] != cookiePrefix+"new" || resp.Fingerprint != fp("new") || resp.LoggedInAt == nil {
		t.Errorf("answer = %+v", resp)
	}
	if r.driver.loginCalls != 1 {
		t.Errorf("logins = %d, want 1", r.driver.loginCalls)
	}
	if got := r.driver.seenCreds[0]; got != r.creds {
		t.Error("the driver was not given the configured credentials")
	}
	// Probed: the old one (dead), then the new one (confirmation).
	if strings.Join(r.driver.probeCalls, ",") != "old,new" {
		t.Errorf("probes = %v", r.driver.probeCalls)
	}
	if r.storedID() != "new" || r.back.fields["logged_in_at"] != r.clock.UTC().Format(time.RFC3339) {
		t.Errorf("stored = %v", r.back.fields)
	}
	st := r.back.state
	if st == nil || st.Blocked || st.UnconfirmedCount != 0 || !st.AttemptStartedAt.IsZero() ||
		!st.RequestLoginAt.Equal(morning()) || st.CredentialFingerprint == "" || st.Salt == "" {
		t.Errorf("state = %+v", st)
	}
	if strings.Contains(st.CredentialFingerprint, testPassword) {
		t.Error("the credential fingerprint is the credential")
	}
	if r.back.connects != r.back.closes {
		t.Errorf("store opened %d times, closed %d", r.back.connects, r.back.closes)
	}
}

// With nothing stored the broker logs in without asking the site anything
// first.
func TestNoStoredSessionLogsIn(t *testing.T) {
	r := newRig(t, nil)
	r.driver.succeed("first")
	r.want(r.ask(""), brokerapi.OutcomeRefreshed, "")
	if strings.Join(r.driver.probeCalls, ",") != "first" {
		t.Errorf("probes = %v, want only the confirmation", r.driver.probeCalls)
	}
}

// A request about a session that was replaced moments ago gets the current
// one, with no login and without even asking the site: that login was
// confirmed seconds ago.
func TestARequestAboutAnAlreadyReplacedSessionGetsTheCurrentOne(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("new")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	probes := len(r.driver.probeCalls)

	r.clock = r.clock.Add(40 * time.Second)
	resp := r.ask(fp("old")) // a second caller, or the first one's retry
	r.want(resp, brokerapi.OutcomeCurrent, "")
	if resp.Fingerprint != fp("new") || r.driver.loginCalls != 1 || len(r.driver.probeCalls) != probes {
		t.Errorf("fingerprint %q, logins %d, probes %d -> %d", resp.Fingerprint, r.driver.loginCalls, probes, len(r.driver.probeCalls))
	}

	// The caller that HOLDS the new session and reports it dead is another
	// matter: the site is asked.
	r.ask(fp("new"))
	if len(r.driver.probeCalls) != probes+1 {
		t.Error("a caller reporting the current session dead was answered without asking the site")
	}
}

// --- single flight ----------------------------------------------------------

func TestConcurrentRequestsShareOneLogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	release := make(chan struct{})
	var entered atomic.Int32
	r.driver.logins = append(r.driver.logins, func(Credentials) (Session, error) {
		entered.Add(1)
		<-release
		r.driver.set("new", ProbeAlive)
		return &fakeSession{id: "new"}, nil
	})

	const callers = 12
	answers := make(chan *brokerapi.Response, callers)
	for i := 0; i < callers; i++ {
		go func() {
			resp, err := r.b.Refresh(context.Background(), brokerapi.Request{Reason: brokerapi.ReasonSessionDead, Fingerprint: fp("old")})
			if err != nil {
				t.Errorf("Refresh: %v", err)
			}
			answers <- resp
		}()
	}
	// Wait until the one login is in progress and every caller is waiting
	// on it.
	deadline := time.Now().Add(5 * time.Second)
	for entered.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no login started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)

	var first *brokerapi.Response
	for i := 0; i < callers; i++ {
		resp := <-answers
		if first == nil {
			first = resp
		}
		if resp != first {
			t.Error("two callers of one refresh got different answers")
		}
	}
	r.want(first, brokerapi.OutcomeRefreshed, "")
	if r.driver.loginCalls != 1 {
		t.Fatalf("logins = %d for %d concurrent requests, want 1", r.driver.loginCalls, callers)
	}
	r.b.Wait()
}

// A caller that stops waiting does not abort the login, and the session it
// produced is there for the next request.
func TestACallerGivingUpDoesNotAbortTheLogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	release := make(chan struct{})
	started := make(chan struct{})
	r.driver.logins = append(r.driver.logins, func(Credentials) (Session, error) {
		close(started)
		<-release
		r.driver.set("new", ProbeAlive)
		return &fakeSession{id: "new"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := r.b.Refresh(ctx, brokerapi.Request{Reason: brokerapi.ReasonSessionDead, Fingerprint: fp("old")})
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Refresh after cancel = %v", err)
	}
	close(release)
	r.b.Wait()
	if r.storedID() != "new" {
		t.Fatalf("the abandoned login's session was not stored (stored %q)", r.storedID())
	}
	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeCurrent, "")
	if r.driver.loginCalls != 1 {
		t.Errorf("logins = %d", r.driver.loginCalls)
	}
}

// --- minimum interval -------------------------------------------------------

func TestLoginsKeepAMinimumInterval(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("s1")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	loggedIn := r.clock

	// The new session dies at once (the account's owner signed in).
	r.driver.set("s1", ProbeDead)
	for _, after := range []time.Duration{3 * time.Minute, 19*time.Minute + 59*time.Second} {
		r.clock = loggedIn.Add(after)
		resp := r.ask(fp("s1"))
		r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)
		if resp.RetryAfter == nil || !resp.RetryAfter.Equal(loggedIn.Add(20*time.Minute)) {
			t.Errorf("after %s: retry_after = %v, want %s", after, resp.RetryAfter, loggedIn.Add(20*time.Minute).UTC())
		}
	}
	if r.driver.loginCalls != 1 {
		t.Fatalf("logins = %d inside the interval", r.driver.loginCalls)
	}

	r.clock = loggedIn.Add(20 * time.Minute)
	r.driver.succeed("s2")
	r.want(r.ask(fp("s1")), brokerapi.OutcomeRefreshed, "")
	if r.driver.loginCalls != 2 {
		t.Errorf("logins = %d", r.driver.loginCalls)
	}
}

// The interval is measured from the last login by ANYBODY, not only the
// broker's own: a session the scheduled job made five minutes ago and that
// is already dead was ended by the owner signing in.
func TestTheIntervalCountsALoginSomebodyElseMade(t *testing.T) {
	r := newRig(t, nil)
	r.store("cron", ProbeDead, 5*time.Minute)
	resp := r.ask(fp("cron"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)
	if r.driver.loginCalls != 0 {
		t.Error("logged in five minutes after another login")
	}
}

// A recorded login time in the future (a clock that jumped) does not open
// the gate.
func TestAFutureLoginTimeDoesNotOpenTheGate(t *testing.T) {
	r := newRig(t, nil)
	r.back.state = &loginvault.State{Salt: "s", RequestLoginAt: morning().Add(72 * time.Hour)}
	r.want(r.ask(""), brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)
	if r.driver.loginCalls != 0 {
		t.Error("logged in")
	}
}

func TestTheIntervalCannotBeConfiguredBelowTheFloor(t *testing.T) {
	d := &fakeDriver{states: map[string]ProbeState{}}
	base := Config{
		Driver: d, Credentials: Credentials{Username: "u", Password: "p"},
		Connect: func(context.Context) (Store, error) { return nil, nil },
		NewSalt: func() (string, error) { return "s", nil },
	}
	c := base
	c.MinInterval = time.Minute
	if _, err := New(c); err == nil {
		t.Error("an interval of one minute was accepted")
	}
	// A driver's floor is higher than the core's.
	c = base
	c.MinInterval = 10 * time.Minute
	if _, err := New(c); err != nil {
		t.Errorf("10m refused without a driver floor: %v", err)
	}
	d.limits = &Constraints{MinInterval: 20 * time.Minute}
	c.Driver = constrainedDriver{d}
	if _, err := New(c); err == nil {
		t.Error("10m accepted below the driver's 20m floor")
	}
	for _, n := range []int{-1, 6} {
		c = base
		c.MaxFailures = n
		if _, err := New(c); err == nil {
			t.Errorf("failure limit %d accepted", n)
		}
	}
}

// --- blackout ---------------------------------------------------------------

func afternoonBlackout(c *Config) {
	c.Blackout = dailywindow.Schedule{{
		Windows: []dailywindow.Window{{Start: 16 * 3600, End: 21 * 3600}},
		Loc:     losAngeles,
	}}
}

// Inside a window -- and within the time a login may take before one --
// the answer is "deferred until", and nothing logs in.
func TestNoLoginInsideABlackoutWindow(t *testing.T) {
	end := time.Date(2026, 10, 12, 21, 0, 0, 0, losAngeles)
	for _, at := range []time.Time{
		time.Date(2026, 10, 12, 15, 57, 30, 0, losAngeles), // a login started now could still be running at 16:00
		time.Date(2026, 10, 12, 16, 0, 0, 0, losAngeles),
		time.Date(2026, 10, 12, 18, 30, 0, 0, losAngeles),
		time.Date(2026, 10, 12, 20, 59, 59, 0, losAngeles),
	} {
		r := newRig(t, afternoonBlackout)
		r.clock = at
		r.store("old", ProbeDead, 9*time.Hour)
		resp := r.ask(fp("old"))
		r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeBlackout)
		if resp.RetryAfter == nil || !resp.RetryAfter.Equal(end) {
			t.Errorf("%s: retry_after = %v, want 21:00 Pacific", at.Format("15:04:05"), resp.RetryAfter)
		}
		if r.driver.loginCalls != 0 {
			t.Fatalf("%s: logged in inside the blackout", at.Format("15:04:05"))
		}
		if r.back.stateWrites != 0 {
			t.Errorf("%s: the record was written for a login that was never started", at.Format("15:04:05"))
		}
	}
}

func TestLoginIsAllowedRightUpToTheHeadroomAndFromTheEnd(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 10, 12, 15, 57, 29, 0, losAngeles),
		time.Date(2026, 10, 12, 21, 0, 0, 0, losAngeles),
	} {
		r := newRig(t, afternoonBlackout)
		r.clock = at
		r.store("old", ProbeDead, 9*time.Hour)
		r.driver.succeed("new")
		r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	}
}

// The blackout forbids logins, not answers: a stored session that is alive
// is still returned at 18:00.
func TestALiveSessionIsStillReturnedDuringTheBlackout(t *testing.T) {
	r := newRig(t, afternoonBlackout)
	r.clock = time.Date(2026, 10, 12, 18, 0, 0, 0, losAngeles)
	r.store("s1", ProbeAlive, time.Hour)
	r.want(r.ask(fp("gone")), brokerapi.OutcomeCurrent, "")
}

// The windows follow the wall clock of their zone across a daylight-saving
// change: 16:00 Pacific is 23:00 UTC in summer and 00:00 UTC in winter.
func TestTheBlackoutFollowsTheWallClockOfItsZone(t *testing.T) {
	for _, utc := range []time.Time{
		time.Date(2026, 7, 6, 23, 30, 0, 0, time.UTC),  // 16:30 PDT
		time.Date(2026, 12, 8, 0, 30, 0, 0, time.UTC),  // 16:30 PST
		time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC), // 15:59 PST on the day the clocks went back
	} {
		r := newRig(t, afternoonBlackout)
		r.clock = utc
		r.store("old", ProbeDead, 9*time.Hour)
		r.want(r.ask(fp("old")), brokerapi.OutcomeDeferred, brokerapi.CodeBlackout)
	}
}

// A driver's own windows apply whatever is configured, and a retry time
// that the interval opens is pushed past a window that would close it.
func TestADriversBlackoutCannotBeConfiguredAway(t *testing.T) {
	r := newRig(t, nil)
	r.driver.limits = &Constraints{
		Blackout: dailywindow.Schedule{{Windows: []dailywindow.Window{{Start: 16 * 3600, End: 21 * 3600}}, Loc: losAngeles}},
	}
	r.tune = func(c *Config) { c.Driver = constrainedDriver{r.driver} } // no Blackout configured at all
	r.start()
	r.clock = time.Date(2026, 10, 12, 17, 0, 0, 0, losAngeles)
	r.store("old", ProbeDead, 9*time.Hour)
	r.want(r.ask(fp("old")), brokerapi.OutcomeDeferred, brokerapi.CodeBlackout)

	// 15:50: outside the window, inside the interval of a login at 15:45.
	// The interval ends at 16:05 -- in the window. Retry at 21:00.
	r.clock = time.Date(2026, 10, 12, 15, 50, 0, 0, losAngeles)
	r.store("recent", ProbeDead, 5*time.Minute)
	resp := r.ask(fp("recent"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)
	if want := time.Date(2026, 10, 12, 21, 0, 0, 0, losAngeles); resp.RetryAfter == nil || !resp.RetryAfter.Equal(want) {
		t.Errorf("retry_after = %v, want %s", resp.RetryAfter, want.UTC())
	}
}

// --- the latch --------------------------------------------------------------

// ANY rejection of the credentials stops everything at once: the next
// wrong password could lock the account.
func TestARejectedLoginSetsTheLatchAtOnce(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.failWith(LoginRejected, "credentials_rejected")

	r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)
	if r.driver.loginCalls != 1 {
		t.Fatalf("logins = %d: a rejected login was retried", r.driver.loginCalls)
	}
	if st := r.back.state; st == nil || !st.Blocked || st.BlockedReason != "credentials_rejected" || st.BlockedAt.IsZero() {
		t.Fatalf("state = %+v", r.back.state)
	}

	// Latched: nothing is sent to the site at all -- no login, no probe --
	// however long anyone waits and whoever asks.
	probes := len(r.driver.probeCalls)
	for i := 0; i < 5; i++ {
		r.clock = r.clock.Add(13 * time.Hour)
		r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)
	}
	if r.driver.loginCalls != 1 || len(r.driver.probeCalls) != probes {
		t.Errorf("while latched: logins %d, probes %d -> %d", r.driver.loginCalls, probes, len(r.driver.probeCalls))
	}
}

// The latch and the time of the last login are in the store, not in the
// process: a restarted broker is exactly as stopped as the one that died.
func TestTheLatchAndTheLastLoginSurviveARestart(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("s1")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	r.driver.set("s1", ProbeDead)

	r.start() // the pod was replaced
	r.clock = r.clock.Add(5 * time.Minute)
	r.want(r.ask(fp("s1")), brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)

	r.clock = r.clock.Add(time.Hour)
	r.driver.failWith(LoginRejected, "credentials_rejected")
	r.want(r.ask(fp("s1")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)

	r.start()
	r.clock = r.clock.Add(48 * time.Hour)
	r.want(r.ask(fp("s1")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)
	if r.driver.loginCalls != 2 {
		t.Errorf("logins = %d; a restart cleared the latch", r.driver.loginCalls)
	}
}

// A person clears the latch in the store, or changes the credentials; then,
// and only then, the broker logs in again.
func TestTheLatchIsClearedByAPersonOrByNewCredentials(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.failWith(LoginRejected, "credentials_rejected")
	r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)

	// New credentials (the operator fixed the password; the pod restarted
	// with them).
	r.clock = r.clock.Add(time.Hour)
	r.creds.Password = "a-different-password-entirely"
	r.start()
	r.driver.succeed("s1")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if r.back.state.Blocked || r.back.state.UnconfirmedCount != 0 {
		t.Errorf("state after a good login with new credentials = %+v", r.back.state)
	}

	// Latch again, then clear it by hand (the record deleted in Vault).
	r.driver.set("s1", ProbeDead)
	r.clock = r.clock.Add(time.Hour)
	r.driver.failWith(LoginRejected, "credentials_rejected")
	r.want(r.ask(fp("s1")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)
	r.back.state = nil
	r.clock = r.clock.Add(time.Hour)
	r.driver.succeed("s2")
	r.want(r.ask(fp("s1")), brokerapi.OutcomeRefreshed, "")
}

// N failed logins in a row set the latch, whatever kind of failure each
// was: one that may have submitted the credentials, or one that returned a
// session the site then refuses.
func TestConsecutiveFailedLoginsSetTheLatch(t *testing.T) {
	r := newRig(t, nil) // the default limit: 2
	r.store("old", ProbeDead, 9*time.Hour)

	r.driver.failWith(LoginUnknown, "after_or_during_submit")
	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeFailed, brokerapi.CodeLoginFailed)
	if r.back.state.UnconfirmedCount != 1 || r.back.state.Blocked {
		t.Fatalf("after one failure: %+v", r.back.state)
	}
	if r.driver.loginCalls != 1 {
		t.Fatalf("logins = %d: a failure that may have submitted the credentials was retried", r.driver.loginCalls)
	}

	// The failed login still counts for the interval.
	r.clock = r.clock.Add(5 * time.Minute)
	r.want(r.ask(fp("old")), brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)

	// The second: a login that "worked" and whose session the site refuses.
	r.clock = r.clock.Add(time.Hour)
	r.driver.logins = append(r.driver.logins, func(Credentials) (Session, error) {
		r.driver.set("ghost", ProbeDead)
		return &fakeSession{id: "ghost"}, nil
	})
	resp = r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeLatched, brokerapi.CodeConsecutiveFailures)
	if !r.back.state.Blocked || r.back.state.BlockedReason != "consecutive_failures" {
		t.Errorf("state = %+v", r.back.state)
	}
	if r.storedID() != "old" {
		t.Errorf("an unconfirmed session was stored: %q", r.storedID())
	}
	r.clock = r.clock.Add(time.Hour)
	r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeConsecutiveFailures)
	if r.driver.loginCalls != 2 {
		t.Errorf("logins = %d", r.driver.loginCalls)
	}
}

// A good login in between starts the count again.
func TestAConfirmedLoginResetsTheFailureCount(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxFailures = 3 })
	r.store("old", ProbeDead, 9*time.Hour)
	for i := 0; i < 2; i++ {
		r.driver.failWith(LoginUnknown, "after_or_during_submit")
		r.want(r.ask(fp("old")), brokerapi.OutcomeFailed, brokerapi.CodeLoginFailed)
		r.clock = r.clock.Add(time.Hour)
	}
	r.driver.succeed("s1")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if r.back.state.UnconfirmedCount != 0 {
		t.Errorf("count = %d after a confirmed login", r.back.state.UnconfirmedCount)
	}
}

// The scheduled login job shares the record. Its latch ("login_unconfirmed")
// stops the broker too.
func TestALatchSetByTheScheduledJobStopsTheBroker(t *testing.T) {
	r := newRig(t, nil)
	r.back.state = &loginvault.State{
		Salt: "salt-for-tests", Blocked: true, BlockedReason: "login_unconfirmed", BlockedAt: morning().Add(-time.Hour),
		CredentialFingerprint: loginvault.CredentialFingerprint("salt-for-tests", testUsername, testPassword),
	}
	r.store("old", ProbeDead, 9*time.Hour)
	r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeConsecutiveFailures)
	if r.driver.loginCalls != 0 || len(r.driver.probeCalls) != 0 {
		t.Error("the site was contacted with the latch set")
	}
}

// --- one submission per attempt ---------------------------------------------

// A login that failed before the credentials were submitted may be tried
// once more -- and that is all it costs: it is not a login for the
// interval and not a strike for the latch. (On main, the request mailbox
// spends its request and its 20 minutes on exactly this.)
func TestAFailureBeforeTheSubmitCostsNothingButACooldown(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.failWith(LoginNotSubmitted, "signin_page_unreachable:network")
	r.driver.failWith(LoginNotSubmitted, "signin_page_unreachable:network")

	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeFailed, brokerapi.CodeLoginNotSubmitted)
	if resp.Detail != "signin_page_unreachable:network" {
		t.Errorf("detail = %q", resp.Detail)
	}
	if r.driver.loginCalls != 2 {
		t.Fatalf("driver logins = %d, want 2 (one retry)", r.driver.loginCalls)
	}
	st := r.back.state
	if st.UnconfirmedCount != 0 || st.Blocked || !st.RequestLoginAt.IsZero() || !st.AttemptStartedAt.IsZero() {
		t.Errorf("state after a pre-submit failure = %+v", st)
	}

	// Cooling down: not even the driver is called.
	r.clock = r.clock.Add(30 * time.Second)
	resp = r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeCooldown)
	if r.driver.loginCalls != 2 {
		t.Error("a login was attempted during the cooldown")
	}

	// After it -- minutes, not the 20-minute interval -- the login goes
	// ahead.
	r.clock = r.clock.Add(2 * time.Minute)
	r.driver.succeed("new")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
}

func TestARetryAfterAPreSubmitFailureCanSucceed(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.failWith(LoginNotSubmitted, "browser_or_network_before_submit")
	r.driver.succeed("new")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if r.driver.loginCalls != 2 || len(r.slept) != 1 || r.slept[0] != retryDelay {
		t.Errorf("logins = %d, slept = %v", r.driver.loginCalls, r.slept)
	}
}

// Whatever a driver returns that is not positively "nothing was submitted"
// is never followed by a second call: not an unknown failure, not an error
// of a type the core does not know, not a rejection.
func TestNothingThatMayHaveSubmittedIsRetried(t *testing.T) {
	for name, queue := range map[string]func(d *fakeDriver){
		"unknown":  func(d *fakeDriver) { d.failWith(LoginUnknown, "after_or_during_submit") },
		"rejected": func(d *fakeDriver) { d.failWith(LoginRejected, "credentials_rejected") },
		"untyped":  func(d *fakeDriver) { /* nothing queued: Login returns a plain error */ },
		"nil session": func(d *fakeDriver) {
			d.logins = append(d.logins, func(Credentials) (Session, error) { return nil, nil })
		},
	} {
		r := newRig(t, nil)
		r.store("old", ProbeDead, 9*time.Hour)
		queue(r.driver)
		resp := r.ask(fp("old"))
		if resp.Outcome == brokerapi.OutcomeRefreshed || resp.Session != nil {
			t.Errorf("%s: answer %s", name, resp.Outcome)
		}
		if r.driver.loginCalls != 1 {
			t.Errorf("%s: driver logins = %d, want exactly 1", name, r.driver.loginCalls)
		}
		if r.back.state.RequestLoginAt.IsZero() {
			t.Errorf("%s: a possible submission was not recorded for the interval", name)
		}
	}
}

// The record is written BEFORE the login. If that write fails, the outcome
// could not be recorded either: no login.
func TestNoLoginWhenTheRecordCannotBeWritten(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.back.failStateWrites = errors.New("permission denied")
	r.want(r.ask(fp("old")), brokerapi.OutcomeFailed, brokerapi.CodeStoreRejected)
	if r.driver.loginCalls != 0 {
		t.Fatal("logged in although the record could not be written")
	}

	r2 := newRig(t, nil)
	r2.store("old", ProbeDead, 9*time.Hour)
	r2.back.cannotWrite = true
	r2.want(r2.ask(fp("old")), brokerapi.OutcomeFailed, brokerapi.CodeStoreRejected)
	if r2.driver.loginCalls != 0 {
		t.Fatal("logged in although the session could not be stored")
	}

	r3 := newRig(t, nil)
	r3.back.failConnect = errors.New("vault k8s login: connection refused")
	r3.want(r3.ask(""), brokerapi.OutcomeFailed, brokerapi.CodeStoreUnavailable)
	if r3.driver.loginCalls != 0 || len(r3.driver.probeCalls) != 0 {
		t.Fatal("the site was contacted without a store")
	}
}

// A process that died in the middle of a login left its marker. Once the
// marker is too old to be a login in flight, the next request counts that
// login as made and failed: it is inside the interval, and it is a strike.
func TestAnAttemptThatNeverRecordedItsOutcomeCountsAsAFailedLogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	died := morning().Add(-15 * time.Minute)
	r.back.state = &loginvault.State{Salt: "salt-for-tests", AttemptStartedAt: died}

	r.want(r.ask(fp("old")), brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)
	st := r.back.state
	if st.UnconfirmedCount != 1 || !st.AttemptStartedAt.IsZero() || !st.RequestLoginAt.Equal(died) || st.LoginDayCount != 1 {
		t.Errorf("state = %+v", st)
	}
	if r.driver.loginCalls != 0 {
		t.Error("logged in straight after a login of unknown outcome")
	}

	// A second such death is the second strike.
	st.AttemptStartedAt = morning().Add(time.Hour)
	st.CredentialFingerprint = loginvault.CredentialFingerprint("salt-for-tests", testUsername, testPassword)
	r.clock = morning().Add(2 * time.Hour)
	r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeConsecutiveFailures)
}

// The marker is in the store while the driver's login runs.
func TestTheAttemptMarkerIsInTheStoreDuringTheLogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	var during loginvault.State
	r.driver.logins = append(r.driver.logins, func(Credentials) (Session, error) {
		r.back.mu.Lock()
		during = *r.back.state
		r.back.mu.Unlock()
		r.driver.set("new", ProbeAlive)
		return &fakeSession{id: "new"}, nil
	})
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if !during.AttemptStartedAt.Equal(morning()) || during.CredentialFingerprint == "" {
		t.Errorf("state during the login = %+v", during)
	}
}

// --- storing the session ----------------------------------------------------

// A new session that cannot be confirmed because the site does not answer
// is asked about again, then given up on -- and never stored.
func TestConfirmationIsRetriedThenTheLoginIsUnconfirmed(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.logins = append(r.driver.logins, func(Credentials) (Session, error) {
		return &fakeSession{id: "limbo"}, nil // the site answers "unknown" about it
	})
	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeFailed, brokerapi.CodeLoginUnconfirmed)
	if got := strings.Join(r.driver.probeCalls, ","); got != "old,limbo,limbo,limbo" {
		t.Errorf("probes = %s", got)
	}
	if r.storedID() != "old" {
		t.Error("an unconfirmed session was stored")
	}
}

// A store that is briefly unavailable after the login is written to again.
// One that stays down does not cost the caller the session: it is returned,
// marked as not stored, and offered to the store by the next request.
func TestASessionThatCannotBeStoredIsStillReturnedAndStoredLater(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("new")
	r.back.failSessionWrites = 2
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if r.storedID() != "new" {
		t.Fatal("a transient write failure was not retried")
	}

	r2 := newRig(t, nil)
	r2.store("old", ProbeDead, 9*time.Hour)
	r2.driver.succeed("new")
	r2.back.failSessionWrites = 3
	resp := r2.ask(fp("old"))
	r2.want(resp, brokerapi.OutcomeRefreshed, brokerapi.CodeSessionNotStored)
	if resp.Session["session"] != cookiePrefix+"new" || r2.storedID() != "old" {
		t.Fatalf("answer %v, stored %q", resp.Session, r2.storedID())
	}
	// Ten minutes later the connector's next request finds the store back.
	r2.clock = r2.clock.Add(10 * time.Minute)
	resp = r2.ask(fp("old"))
	r2.want(resp, brokerapi.OutcomeCurrent, "")
	if r2.storedID() != "new" || r2.driver.loginCalls != 1 {
		t.Errorf("stored %q, logins %d", r2.storedID(), r2.driver.loginCalls)
	}
}

// Something else wrote a session during the login. If the site still
// accepts this one it is the live one and replaces the other.
func TestACheckAndSetMismatchIsResolvedByAskingTheSite(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("new")
	r.back.casMismatchOnce = true
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if r.storedID() != "new" {
		t.Errorf("stored %q", r.storedID())
	}

	r2 := newRig(t, nil)
	r2.store("old", ProbeDead, 9*time.Hour)
	r2.driver.logins = append(r2.driver.logins, func(Credentials) (Session, error) {
		r2.driver.set("new", ProbeAlive)
		return &fakeSession{id: "new"}, nil
	})
	r2.back.casMismatchOnce = true
	// By the time the write is refused the other login has ended this
	// one: it is alive for its confirmation and dead at the second look.
	seen := 0
	r2.b.driver = probeHook{Driver: r2.driver, before: func(id string) {
		if id == "new" {
			seen++
			if seen == 2 {
				r2.driver.set("new", ProbeDead)
			}
		}
	}}
	resp := r2.ask(fp("old"))
	r2.want(resp, brokerapi.OutcomeFailed, brokerapi.CodeSuperseded)
	if r2.storedID() != "old" {
		t.Errorf("a superseded session was stored: %q", r2.storedID())
	}
}

type probeHook struct {
	Driver
	before func(id string)
}

func (p probeHook) Probe(ctx context.Context, s Session) ProbeResult {
	p.before(s.(*fakeSession).id)
	return p.Driver.Probe(ctx, s)
}

// --- no keepalive, and nothing in the logs -----------------------------------

// The broker sends the site nothing unless somebody asks: not at startup,
// not while idle, not after answering.
func TestNothingIsSentToTheSiteWithoutARequest(t *testing.T) {
	r := newRig(t, nil)
	r.store("s1", ProbeAlive, time.Hour)
	r.start()
	r.start()
	time.Sleep(20 * time.Millisecond)
	if r.driver.loginCalls != 0 || len(r.driver.probeCalls) != 0 || r.back.connects != 0 {
		t.Fatalf("idle: logins %d, probes %d, store connections %d", r.driver.loginCalls, len(r.driver.probeCalls), r.back.connects)
	}
	r.ask(fp("s1"))
	r.b.Wait()
	time.Sleep(20 * time.Millisecond)
	if len(r.driver.probeCalls) != 1 {
		t.Errorf("probes = %d after one request", len(r.driver.probeCalls))
	}
}

// Every log line of every scenario above that touches a credential or a
// session, read back: no username, no password, no cookie value, whatever
// the driver put into its errors and details.
func TestLogsNeverCarryCredentialsOrCookies(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxFailures = 3 })
	r.store("old", ProbeDead, 9*time.Hour)

	r.driver.failWith(LoginNotSubmitted, "page said: "+testPassword)
	r.driver.failWith(LoginNotSubmitted, cookiePrefix+"old")
	r.ask(fp("old"))
	r.clock = r.clock.Add(time.Hour)
	r.driver.failWith(LoginUnknown, "USER="+testUsername+" <html>secret page</html>")
	resp := r.ask(fp("old"))
	if resp.Detail != "user_redacted__html_secret_page__html_" {
		// Shape only: a classified token, with the credential gone.
		t.Errorf("detail = %q", resp.Detail)
	}
	r.clock = r.clock.Add(time.Hour)
	r.ask(fp("old")) // an untyped error quoting the password
	r.clock = r.clock.Add(time.Hour)
	r.driver.succeed("new")
	r.back.failSessionWrites = 3
	r.ask(fp("old"))
	r.clock = r.clock.Add(time.Hour)
	r.ask(fp("old"))
	r.driver.set("new", ProbeDead)
	r.clock = r.clock.Add(time.Hour)
	r.back.failStateWrites = errors.New("vault said " + testPassword + " for " + testUsername)
	r.ask(fp("new"))
	r.back.failStateWrites = nil
	r.driver.failWith(LoginRejected, "credentials_rejected")
	r.ask(fp("new"))
	r.ask(fp("new"))

	logs := r.logs.String()
	if !strings.Contains(logs, `"outcome":"latched"`) || !strings.Contains(logs, `"outcome":"refreshed"`) {
		t.Fatalf("the scenario did not run as intended; log:\n%s", logs)
	}
	for _, secret := range []string{testPassword, testUsername, cookiePrefix, "secret page", "<html>",
		strings.ReplaceAll(testUsername, "-", "_"), strings.ReplaceAll(testPassword, "-", "_")} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log carries %q", secret)
		}
	}
}

func TestSafeToken(t *testing.T) {
	for in, want := range map[string]string{
		"sign_in_page":                      "sign_in_page",
		"signin_page_unrecognised:http_403": "signin_page_unrecognised:http_403",
		"Bad Value <b>":                     "bad_value__b_",
		strings.Repeat("a", 200):            strings.Repeat("a", 64),
		"":                                  "",
	} {
		if got := safeToken(in); got != want {
			t.Errorf("safeToken(%q) = %q, want %q", in, got, want)
		}
	}
}
