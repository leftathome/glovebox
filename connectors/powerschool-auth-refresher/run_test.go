package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// Values that must never reach a log line. None is a real credential.
const (
	testUsername    = "parent-account-7731"
	testPassword    = "correct-horse-battery-staple-9"
	testCookieValue = "cookie-value-5b1e7d9c4a"
	testStudentID   = "sid-90817263-x"
	testSalt        = "00112233445566778899aabbccddeeff"
)

// morning is a weekday instant well outside the blackout: 05:00 Pacific.
func morning(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 10, 12, 5, 0, 0, 0, mustZone(t, "America/Los_Angeles"))
}

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		BaseURL:          "https://portal.example.org",
		Host:             "portal.example.org",
		Username:         testUsername,
		Password:         testPassword,
		VaultAddr:        "https://vault.example.org:8200",
		VaultK8sRole:     defaultVaultK8sRole,
		VaultKVMount:     defaultVaultKVMount,
		VaultSessionPath: defaultVaultSessionPath,
		VaultStatePath:   "glovebox/powerschool/default/login-state",
		Blackout:         defaultBlackout(t),
		MinSessionAge:    defaultMinSessionAge,
		SessionLifetime:  defaultSessionLifetime,
	}
}

// librarySession is a session shaped the way powerschool-go's browser login
// returns one: a session cookie (the library gives those an expiry of
// time.Unix(-1, 0)) beside a dated one, and the library's own expiry guess.
func librarySession(now time.Time) *pslib.SessionExport {
	return &pslib.SessionExport{
		Cookies: []*http.Cookie{
			{Name: "JSESSIONID", Value: testCookieValue, Path: "/", Domain: "portal.example.org",
				Expires: time.Unix(-1, 0), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode},
			{Name: "psaid", Value: "second-" + testCookieValue, Path: "/", Domain: ".example.org",
				Expires: now.Add(30 * 24 * time.Hour).UTC().Truncate(time.Second), Secure: true},
			{Name: "empty", Value: ""},
			nil,
		},
		ExpiresAt: now.Add(4 * time.Hour),
	}
}

// storedAt returns a stored session logged in at the given instant.
func storedAt(t *testing.T, loggedInAt time.Time) *storedSession {
	t.Helper()
	s := normalizeSession(librarySession(loggedInAt), loggedInAt.Add(defaultSessionLifetime))
	data, err := sessionData(s, loggedInAt, "")
	if err != nil {
		t.Fatal(err)
	}
	return &storedSession{Document: data["session"].(string), LoggedInAt: loggedInAt}
}

// fakeStore is an in-memory sessionStore. It persists across runs of one
// harness, as Vault does across runs of the job.
type fakeStore struct {
	stored      *storedSession
	version     int
	readErr     error
	canWrite    bool
	canWriteErr error
	writeErrs   []error // consumed one per Write; nil entry or exhausted = success

	state          *loginState // nil = nothing stored
	readStateErr   error
	writeStateErrs []error // consumed one per WriteState

	// onRead, when set, runs after each Read (to simulate another run
	// writing the path in between).
	onRead func()

	reads, capChecks, writes, stateWrites, closes int
	written                                       []map[string]any
	writtenCAS                                    []int
}

func (f *fakeStore) Read(context.Context) (*storedSession, int, error) {
	f.reads++
	stored, version := f.stored, f.version
	if f.onRead != nil {
		f.onRead()
	}
	return stored, version, f.readErr
}

func (f *fakeStore) CanWrite(context.Context) (bool, error) {
	f.capChecks++
	return f.canWrite, f.canWriteErr
}

func (f *fakeStore) Write(_ context.Context, data map[string]any, cas int) (int, error) {
	f.writes++
	if n := f.writes - 1; n < len(f.writeErrs) && f.writeErrs[n] != nil {
		return 0, f.writeErrs[n]
	}
	if cas != f.version {
		return 0, &vaultapi.ResponseError{HTTPMethod: "PUT", StatusCode: 400,
			Errors: []string{"check-and-set parameter did not match the current version"}}
	}
	f.version++
	f.written = append(f.written, data)
	f.writtenCAS = append(f.writtenCAS, cas)
	loggedInAt, _ := time.Parse(time.RFC3339, data["logged_in_at"].(string))
	f.stored = &storedSession{Document: data["session"].(string), LoggedInAt: loggedInAt}
	return f.version, nil
}

func (f *fakeStore) ReadState(context.Context) (*loginState, error) {
	if f.readStateErr != nil {
		return nil, f.readStateErr
	}
	if f.state == nil {
		return &loginState{}, nil
	}
	copied := *f.state
	return &copied, nil
}

func (f *fakeStore) WriteState(_ context.Context, state *loginState) error {
	f.stateWrites++
	if n := f.stateWrites - 1; n < len(f.writeStateErrs) && f.writeStateErrs[n] != nil {
		return f.writeStateErrs[n]
	}
	copied := *state
	f.state = &copied
	return nil
}

func (f *fakeStore) Close() { f.closes++ }

// harness wires run() to fakes and counts what happened. Counters are
// cumulative over the harness's runs.
type harness struct {
	t     *testing.T
	cfg   config
	clock time.Time
	store *fakeStore
	logs  bytes.Buffer

	connectErr error
	connects   int

	// signInErr is what the sign-in check returns.
	signInErr    error
	signInChecks int

	// loginResults is consumed one per login; when exhausted a login
	// succeeds. A nil entry is a success.
	loginResults []error
	logins       int
	// onLogin, when set, runs at the start of every login.
	onLogin func()

	// probeResults is consumed one per probe; when exhausted a probe
	// succeeds.
	probeResults []error
	probes       int

	slept []time.Duration
	// onSleep, when set, runs on every sleep (to move the clock).
	onSleep func(time.Duration)
	// onConnect, when set, runs when Vault is contacted.
	onConnect func()

	// terminations are the termination messages, one per run.
	terminations []string

	// request is the connector's login request field; requestErr fails the
	// read. requestReads counts reads.
	request      string
	requestErr   error
	requestReads int
}

// requestMode puts the harness's config into request mode.
func (h *harness) requestMode() *harness {
	h.cfg.RequestConfigMap = "glovebox-glovebox-powerschool-login-request"
	h.cfg.RequestMaxAge = defaultRequestMaxAge
	h.cfg.RequestMinInterval = minRequestInterval
	return h
}

// ask sets a request made `ago` before the harness clock about the session
// with the given full fingerprint ("" = the connector had none).
func (h *harness) ask(ago time.Duration, fullFingerprint string) string {
	h.request = psportal.FormatLoginRequest(h.clock.Add(-ago), fullFingerprint)
	return h.request
}

// portalContacts is every way the portal can be reached.
func (h *harness) portalContacts() int { return h.probes + h.signInChecks + h.logins }

func newHarness(t *testing.T, at time.Time) *harness {
	t.Helper()
	return &harness{t: t, cfg: testConfig(t), clock: at, store: &fakeStore{canWrite: true}}
}

func (h *harness) deps() deps {
	return deps{
		now: func() time.Time { return h.clock },
		sleep: func(_ context.Context, d time.Duration) error {
			h.slept = append(h.slept, d)
			h.clock = h.clock.Add(d)
			if h.onSleep != nil {
				h.onSleep(d)
			}
			return nil
		},
		connect: func(context.Context) (sessionStore, error) {
			h.connects++
			if h.onConnect != nil {
				h.onConnect()
			}
			if h.connectErr != nil {
				return nil, h.connectErr
			}
			return h.store, nil
		},
		checkSignIn: func(context.Context) error {
			h.signInChecks++
			return h.signInErr
		},
		login: func(context.Context) (*pslib.SessionExport, error) {
			h.logins++
			if h.onLogin != nil {
				h.onLogin()
			}
			if n := h.logins - 1; n < len(h.loginResults) && h.loginResults[n] != nil {
				return nil, h.loginResults[n]
			}
			return librarySession(h.clock), nil
		},
		probe: func(context.Context, *pslib.SessionExport) (string, error) {
			h.probes++
			if n := h.probes - 1; n < len(h.probeResults) && h.probeResults[n] != nil {
				return "", h.probeResults[n]
			}
			return testStudentID, nil
		},
		readRequest: func(context.Context) (string, error) {
			h.requestReads++
			return h.request, h.requestErr
		},
		newSalt:   func() (string, error) { return testSalt, nil },
		terminate: func(m string) { h.terminations = append(h.terminations, m) },
		logger:    slog.New(slog.NewJSONHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (h *harness) run() int {
	h.t.Helper()
	before := len(h.terminations)
	code := run(context.Background(), h.cfg, h.deps())
	h.assertNoSecretsLogged()
	if len(h.terminations) != before+1 {
		h.t.Errorf("the run wrote %d termination messages, want exactly 1", len(h.terminations)-before)
	}
	return code
}

// assertNoSecretsLogged is applied to every run in this file: to the log
// and to the termination messages.
func (h *harness) assertNoSecretsLogged() {
	h.t.Helper()
	out := h.logs.String() + "\n" + strings.Join(h.terminations, "\n")
	for name, secret := range map[string]string{
		"password": testPassword, "username": testUsername,
		"cookie value": testCookieValue, "student id": testStudentID,
	} {
		if strings.Contains(out, secret) {
			h.t.Errorf("the %s appears in the log or termination message:\n%s", name, out)
		}
	}
}

func (h *harness) expect(code, wantCode, wantLogins, wantWrites int) {
	h.t.Helper()
	if code != wantCode {
		h.t.Errorf("exit code = %d, want %d\nlog:\n%s", code, wantCode, h.logs.String())
	}
	if h.logins != wantLogins {
		h.t.Errorf("logins = %d, want %d", h.logins, wantLogins)
	}
	if h.store.writes != wantWrites {
		h.t.Errorf("vault session writes = %d, want %d", h.store.writes, wantWrites)
	}
}

func (h *harness) logged(substr string) bool { return strings.Contains(h.logs.String(), substr) }

// lastTermination is the termination message of the most recent run.
func (h *harness) lastTermination() string {
	if len(h.terminations) == 0 {
		return ""
	}
	return h.terminations[len(h.terminations)-1]
}

var (
	errBeforeSubmit = &loginError{kind: loginFailedBeforeSubmit, err: errors.New("page load error net::ERR_NAME_NOT_RESOLVED")}
	errRejected     = &loginError{kind: loginRejected, err: errors.New("login failed: invalid credentials")}
	errUnknown      = &loginError{kind: loginFailedUnknown, err: errors.New("context deadline exceeded")}
	errSessionGone  = pslib.NewError(pslib.CodeSessionExpired, "the portal returned its login page", pslib.ErrSessionExpired)
	errNetwork      = pslib.WrapNetworkError(errors.New("dial tcp: connection refused"))
)

// --- blackout -------------------------------------------------------------

// runClock are the instants a whole run is exercised at. A login may take
// loginHeadroom (150s), so the last instant a run may start is 15:57:29.
var runClock = []struct {
	h, m, s int
	refused bool
}{
	{15, 57, 29, false},
	{15, 57, 30, true},
	{15, 58, 0, true},
	{15, 58, 31, true},
	{15, 59, 30, true},
	{15, 59, 59, true},
	{16, 0, 0, true},
	{18, 30, 0, true},
	{20, 59, 59, true},
	{21, 0, 0, false},
	{5, 0, 0, false},
	{21, 5, 0, false},
	{0, 0, 0, false},
}

// The owner's rule, end to end through run(): a run that could log in
// between 16:00:00 and 20:59:59 Pacific contacts nothing and exits 0; one
// at 21:00:00, or early enough to be finished by 15:59:59, logs in. On
// every kind of day, both daylight-saving changes included, and with the
// process clock in UTC as it is in the pod.
func TestRunRefusesInsideTheBlackoutAndProceedsOutsideIt(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	for _, day := range blackoutDays {
		for _, c := range runClock {
			at := time.Date(2026, time.Month(day.month), day.day, c.h, c.m, c.s, 0, la).UTC()
			name := fmt.Sprintf("%s %02d:%02d:%02d", day.name, c.h, c.m, c.s)
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, at)
				code := h.run()
				if c.refused {
					h.expect(code, exitSuccess, 0, 0)
					if h.connects != 0 || h.probes != 0 || h.signInChecks != 0 {
						t.Errorf("a refused run contacted Vault (%d) or the portal (%d probes, %d sign-in checks)",
							h.connects, h.probes, h.signInChecks)
					}
					if !h.logged(`"outcome":"blackout_refused"`) || !h.logged("refusing to log in") {
						t.Errorf("no clear refusal line:\n%s", h.logs.String())
					}
					if !h.logged(`"blackout_window":"16:00-21:00"`) || !h.logged(`"blackout_timezone":"America/Los_Angeles"`) {
						t.Errorf("the refusal does not name the window:\n%s", h.logs.String())
					}
					if msg := h.lastTermination(); !strings.Contains(msg, "outcome=blackout_refused exit=0") ||
						!strings.Contains(msg, "16:00-21:00 America/Los_Angeles") {
						t.Errorf("termination message = %q", msg)
					}
					return
				}
				h.expect(code, exitSuccess, 1, 1)
			})
		}
	}
}

// The clock is read again immediately before the login: a run that started
// in time and then spent 40 seconds on Vault must not start a login that
// could run past 16:00.
func TestRunRechecksTheBlackoutImmediatelyBeforeLoggingIn(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	h := newHarness(t, time.Date(2026, 10, 12, 15, 57, 0, 0, la))
	h.onConnect = func() { h.clock = h.clock.Add(40 * time.Second) }
	code := h.run()
	h.expect(code, exitSuccess, 0, 0)
	if !h.logged(`"outcome":"blackout_refused"`) {
		t.Errorf("no refusal line:\n%s", h.logs.String())
	}
}

// The pause before the one permitted retry can carry the run to within the
// headroom of the window. The second attempt is then not made.
func TestRunDoesNotRetryIntoTheBlackout(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	h := newHarness(t, time.Date(2026, 10, 12, 15, 57, 25, 0, la))
	h.loginResults = []error{errBeforeSubmit}
	code := h.run()
	h.expect(code, exitTransient, 1, 0)
	if !h.logged(`"outcome":"blackout_refused"`) {
		t.Errorf("no refusal line:\n%s", h.logs.String())
	}
}

// A widened blackout is enforced like the floor.
func TestRunHonoursAWidenedBlackout(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	wide, err := parseBlackout("15:00", "22:00", "06:00-07:00", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		h, m    int
		refused bool
	}{{14, 58, true}, {14, 50, false}, {15, 30, true}, {21, 30, true}, {22, 0, false}, {6, 30, true}, {5, 57, false}, {5, 58, true}} {
		h := newHarness(t, time.Date(2026, 10, 12, c.h, c.m, 0, 0, la))
		h.cfg.Blackout = wide
		code := h.run()
		if c.refused {
			h.expect(code, exitSuccess, 0, 0)
		} else {
			h.expect(code, exitSuccess, 1, 1)
		}
	}
}

// --- young-session skip ---------------------------------------------------

func TestRunSkipsLoginWhenStoredSessionIsYoungAndStillAccepted(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.stored = storedAt(t, h.clock.Add(-3*time.Hour))
	code := h.run()
	h.expect(code, exitSuccess, 0, 0)
	if h.probes != 1 {
		t.Errorf("probes = %d, want exactly one cheap read", h.probes)
	}
	if !h.logged("session still good") || !h.logged(`"outcome":"session_still_good"`) {
		t.Errorf("no 'session still good' line:\n%s", h.logs.String())
	}
	if h.signInChecks != 0 || h.store.stateWrites != 0 {
		t.Errorf("a skipped run fetched the sign-in page (%d) or wrote state (%d)", h.signInChecks, h.store.stateWrites)
	}
}

func TestRunLogsInWhenYoungSessionIsNoLongerAccepted(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.stored = storedAt(t, h.clock.Add(-3*time.Hour))
	h.store.version = 4
	h.probeResults = []error{errSessionGone}
	h.expect(h.run(), exitSuccess, 1, 1)
}

// A young session whose validity could not be established is left alone: a
// login would end it if it is in fact good.
func TestRunDoesNotLogInWhenYoungSessionCannotBeChecked(t *testing.T) {
	for name, probeErr := range map[string]error{
		"network": errNetwork,
		"parse":   pslib.WrapParseError(errors.New("no students found"), "students list"),
		"status":  pslib.WrapNetworkError(&pslib.HTTPStatusError{Op: "GET /guardian/home.html", StatusCode: 503}),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, morning(t))
			h.store.stored = storedAt(t, h.clock.Add(-3*time.Hour))
			h.probeResults = []error{probeErr}
			h.expect(h.run(), exitTransient, 0, 0)
		})
	}
}

func TestRunLogsInWhenStoredSessionHasReachedTheMinimumAge(t *testing.T) {
	for _, c := range []struct {
		name      string
		age       time.Duration
		wantLogin bool
	}{
		{"one second short of the minimum", defaultMinSessionAge - time.Second, false},
		{"exactly the minimum", defaultMinSessionAge, true},
		{"well past it", 24 * time.Hour, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, morning(t))
			h.store.stored = storedAt(t, h.clock.Add(-c.age))
			code := h.run()
			if c.wantLogin {
				h.expect(code, exitSuccess, 1, 1)
				// An old session is replaced without being asked about:
				// the only read is the one confirming the new session.
				if h.probes != 1 {
					t.Errorf("probes = %d, want 1", h.probes)
				}
				return
			}
			h.expect(code, exitSuccess, 0, 0)
		})
	}
}

func TestRunMinimumAgeIsConfigurable(t *testing.T) {
	h := newHarness(t, morning(t))
	h.cfg.MinSessionAge = 30 * time.Hour
	h.store.stored = storedAt(t, h.clock.Add(-24*time.Hour))
	h.expect(h.run(), exitSuccess, 0, 0)
}

func TestRunLogsInWhenThereIsNoUsableStoredSession(t *testing.T) {
	at := morning(t)
	expired := storedAt(t, at.Add(-48*time.Hour)) // past its 36h expires_at
	young := storedAt(t, at.Add(-time.Hour))
	for name, stored := range map[string]*storedSession{
		"nothing stored": nil,
		"not JSON":       {Document: "{not json", LoggedInAt: at.Add(-time.Hour)},
		"no cookies":     {Document: `{"cookies":[],"expires_at":"2099-01-01T00:00:00Z"}`, LoggedInAt: at.Add(-time.Hour)},
		"expired":        {Document: expired.Document, LoggedInAt: at.Add(-time.Hour)},
		"age unknown":    {Document: young.Document},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, at)
			h.store.stored = stored
			code := h.run()
			h.expect(code, exitSuccess, 1, 1)
			if h.probes != 1 {
				t.Errorf("probes = %d, want only the post-login confirmation", h.probes)
			}
		})
	}
}

// A stored session dated in the future (clock skew) counts as age zero.
func TestRunTreatsAFutureLoginTimeAsYoung(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.stored = storedAt(t, h.clock.Add(5*time.Minute))
	h.expect(h.run(), exitSuccess, 0, 0)
}

// --- one login per run ----------------------------------------------------

func TestRunLoginAttempts(t *testing.T) {
	for _, c := range []struct {
		name       string
		results    []error
		wantCode   int
		wantLogins int
		wantWrites int
	}{
		{"first attempt succeeds", nil, exitSuccess, 1, 1},
		{"fails before the submit, then succeeds", []error{errBeforeSubmit}, exitSuccess, 2, 1},
		{"fails before the submit twice", []error{errBeforeSubmit, errBeforeSubmit, nil}, exitTransient, 2, 0},
		{"rejected credentials", []error{errRejected, nil}, exitBadCredentials, 1, 0},
		{"unplaceable failure", []error{errUnknown, nil}, exitTransient, 1, 0},
		{"an error of no known kind", []error{errors.New("boom"), nil}, exitTransient, 1, 0},
		{"before the submit, then rejected", []error{errBeforeSubmit, errRejected, nil}, exitBadCredentials, 2, 0},
		{"before the submit, then unplaceable", []error{errBeforeSubmit, errUnknown, nil}, exitTransient, 2, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, morning(t))
			h.loginResults = c.results
			h.expect(h.run(), c.wantCode, c.wantLogins, c.wantWrites)
		})
	}
}

// Whatever the login returns, in whatever order, a run never logs in more
// than twice, and never a second time unless the first failure was before
// the submit.
func TestRunNeverExceedsTheLoginCeiling(t *testing.T) {
	kinds := []error{nil, errBeforeSubmit, errRejected, errUnknown, errors.New("untyped")}
	for _, first := range kinds {
		for _, second := range kinds {
			for _, third := range kinds {
				h := newHarness(t, morning(t))
				h.loginResults = []error{first, second, third, errBeforeSubmit, errBeforeSubmit}
				h.run()
				if h.logins > maxLoginAttempts {
					t.Fatalf("%d logins for results (%v, %v, %v)", h.logins, first, second, third)
				}
				if h.logins == 2 && first != errBeforeSubmit {
					t.Fatalf("a second login followed a first result of %v", first)
				}
			}
		}
	}
}

func TestRunWaitsBeforeTheSingleRetry(t *testing.T) {
	h := newHarness(t, morning(t))
	h.loginResults = []error{errBeforeSubmit}
	h.expect(h.run(), exitSuccess, 2, 1)
	if len(h.slept) != 1 || h.slept[0] != loginRetryDelay {
		t.Errorf("slept %v, want one pause of %s", h.slept, loginRetryDelay)
	}
}

// --- the latch: rejected credentials (I1) ----------------------------------

// A rejected login sets the latch. Every later run with the same
// credentials refuses, exit 6, without starting a browser or touching the
// portal. A changed password lifts it.
func TestRejectedCredentialsBlockEveryLaterRunUntilTheyChange(t *testing.T) {
	h := newHarness(t, morning(t))
	h.loginResults = []error{errRejected}
	h.expect(h.run(), exitBadCredentials, 1, 0)
	if !h.logged(`"outcome":"bad_credentials"`) {
		t.Errorf("no bad_credentials line:\n%s", h.logs.String())
	}
	st := h.store.state
	if st == nil || !st.Blocked || st.BlockedReason != blockedCredentialsRejected {
		t.Fatalf("latch not set: %+v", st)
	}
	if !st.BlockedAt.Equal(h.clock.UTC()) {
		t.Errorf("blocked_at = %s, want %s", st.BlockedAt, h.clock.UTC())
	}
	if st.CredentialFingerprint != credentialFingerprint(testSalt, testUsername, testPassword) {
		t.Errorf("the latch does not carry the fingerprint of the credentials used")
	}

	// The next ten scheduled runs: none starts a browser.
	for i := 0; i < 10; i++ {
		h.clock = h.clock.Add(24 * time.Hour)
		probesBefore, signInBefore := h.probes, h.signInChecks
		code := h.run()
		h.expect(code, exitLoginBlocked, 1, 0)
		if h.probes != probesBefore || h.signInChecks != signInBefore {
			t.Fatalf("a blocked run contacted the portal")
		}
	}
	if !h.logged(`"outcome":"login_blocked"`) || !h.logged("login blocked") {
		t.Errorf("no login_blocked line:\n%s", h.logs.String())
	}
	if msg := h.lastTermination(); !strings.Contains(msg, "outcome=login_blocked exit=6") ||
		!strings.Contains(msg, "credentials_rejected") || !strings.Contains(msg, h.cfg.VaultStatePath) {
		t.Errorf("termination message = %q", msg)
	}

	// The operator corrects the password: the run proceeds, and a
	// confirmed login leaves no block behind.
	h.cfg.Password = "a-new-password-entirely-4"
	h.clock = h.clock.Add(24 * time.Hour)
	h.expect(h.run(), exitSuccess, 2, 1)
	if h.store.state.Blocked || h.store.state.UnconfirmedCount != 0 {
		t.Errorf("state after a confirmed login: %+v", h.store.state)
	}
	if strings.Contains(h.logs.String(), "a-new-password-entirely-4") {
		t.Error("the new password appears in the log")
	}
}

// A new password that is also wrong is blocked in its turn, under its own
// fingerprint: one submission per password, not one per run.
func TestAChangedButStillWrongPasswordIsSubmittedOnce(t *testing.T) {
	h := newHarness(t, morning(t))
	h.loginResults = []error{errRejected, errRejected, errRejected}
	h.expect(h.run(), exitBadCredentials, 1, 0)
	h.expect(h.run(), exitLoginBlocked, 1, 0)

	h.cfg.Password = "second-wrong-password"
	h.expect(h.run(), exitBadCredentials, 2, 0)
	h.expect(h.run(), exitLoginBlocked, 2, 0)
	h.expect(h.run(), exitLoginBlocked, 2, 0)
}

// The operator clears the latch by deleting the state record.
func TestClearingTheStateRecordLiftsTheBlock(t *testing.T) {
	h := newHarness(t, morning(t))
	h.loginResults = []error{errRejected}
	h.expect(h.run(), exitBadCredentials, 1, 0)
	h.expect(h.run(), exitLoginBlocked, 1, 0)

	h.store.state = nil // vault kv metadata delete .../login-state
	h.expect(h.run(), exitSuccess, 2, 1)
}

// The fingerprint is salted and never contains the values.
func TestCredentialFingerprint(t *testing.T) {
	fp := credentialFingerprint(testSalt, testUsername, testPassword)
	if len(fp) != 64 || strings.Contains(fp, testPassword) || strings.Contains(fp, testUsername) {
		t.Errorf("fingerprint = %q", fp)
	}
	for name, other := range map[string]string{
		"another salt":     credentialFingerprint("ffeeddccbbaa99887766554433221100", testUsername, testPassword),
		"another password": credentialFingerprint(testSalt, testUsername, testPassword+"x"),
		"another username": credentialFingerprint(testSalt, testUsername+"x", testPassword),
		"shifted boundary": credentialFingerprint(testSalt, testUsername+"c", strings.TrimPrefix(testPassword, "c")),
	} {
		if other == fp {
			t.Errorf("%s gives the same fingerprint", name)
		}
	}
	if credentialFingerprint(testSalt, testUsername, testPassword) != fp {
		t.Error("the fingerprint is not stable")
	}
}

// The salt is generated once and kept.
func TestTheSaltIsKeptAcrossRuns(t *testing.T) {
	h := newHarness(t, morning(t))
	h.expect(h.run(), exitSuccess, 1, 1)
	if h.store.state == nil || h.store.state.Salt != testSalt {
		t.Fatalf("state = %+v", h.store.state)
	}
	h.store.state.Salt = "a-salt-from-an-earlier-run"
	h.clock = h.clock.Add(24 * time.Hour)
	h.expect(h.run(), exitSuccess, 2, 2)
	if h.store.state.Salt != "a-salt-from-an-earlier-run" {
		t.Errorf("the salt was replaced: %q", h.store.state.Salt)
	}
}

// If the state record cannot be written, a rejection could not be recorded
// either. The run stops BEFORE the login.
func TestRunDoesNotLogInWhenTheLatchCannotBeWritten(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.writeStateErrs = []error{vaultStatus(403)}
	h.expect(h.run(), exitVaultRejected, 0, 0)

	h = newHarness(t, morning(t))
	h.store.writeStateErrs = []error{vaultStatus(503), vaultStatus(503), vaultStatus(503)}
	h.expect(h.run(), exitTransient, 0, 0)

	h = newHarness(t, morning(t))
	h.store.readStateErr = vaultStatus(403)
	h.expect(h.run(), exitVaultRejected, 0, 0)
	if h.store.reads != 0 {
		t.Error("the session was read although the state could not be")
	}
}

// --- the latch: unconfirmed logins (I2) ------------------------------------

// A login that returns without a working session (a CAPTCHA, a second
// factor, an expired password: the library sees no error notice and reports
// success) is login_unconfirmed. Nothing is written. The second in a row
// sets the latch, so the job stops ending the connector's session twice a
// day without ever replacing it.
func TestTwoUnconfirmedLoginsInARowBlockFurtherLogins(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{errSessionGone, errSessionGone}

	h.expect(h.run(), exitLoginUnconfirmed, 1, 0)
	if !h.logged(`"outcome":"login_unconfirmed"`) {
		t.Errorf("no login_unconfirmed line:\n%s", h.logs.String())
	}
	if st := h.store.state; st == nil || st.Blocked || st.UnconfirmedCount != 1 {
		t.Fatalf("after one unconfirmed login: %+v", st)
	}
	if msg := h.lastTermination(); !strings.Contains(msg, "outcome=login_unconfirmed exit=5") {
		t.Errorf("termination message = %q", msg)
	}

	h.clock = h.clock.Add(16 * time.Hour)
	h.expect(h.run(), exitLoginUnconfirmed, 2, 0)
	if st := h.store.state; !st.Blocked || st.BlockedReason != blockedLoginUnconfirmed || st.UnconfirmedCount != 2 {
		t.Fatalf("after two unconfirmed logins: %+v", st)
	}

	for i := 0; i < 5; i++ {
		h.clock = h.clock.Add(12 * time.Hour)
		h.expect(h.run(), exitLoginBlocked, 2, 0)
	}
	if msg := h.lastTermination(); !strings.Contains(msg, "login_unconfirmed") {
		t.Errorf("termination message = %q", msg)
	}
	if h.store.writes != 0 {
		t.Error("an unconfirmed session was written")
	}
}

// Only a CONFIRMED login resets the count: unconfirmed, confirmed,
// unconfirmed does not block.
func TestAConfirmedLoginResetsTheUnconfirmedCount(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{errSessionGone}
	h.expect(h.run(), exitLoginUnconfirmed, 1, 0)

	h.clock = h.clock.Add(24 * time.Hour)
	h.expect(h.run(), exitSuccess, 2, 1)
	if h.store.state.UnconfirmedCount != 0 {
		t.Fatalf("count after a confirmed login = %d", h.store.state.UnconfirmedCount)
	}

	h.clock = h.clock.Add(24 * time.Hour)
	h.probeResults = []error{errSessionGone, nil, errSessionGone}
	h.expect(h.run(), exitLoginUnconfirmed, 3, 1)
	if st := h.store.state; st.Blocked || st.UnconfirmedCount != 1 {
		t.Fatalf("one unconfirmed login after a confirmed one: %+v", st)
	}
}

// Runs that do not log in at all leave the count alone: a transient
// failure, a skipped run or a blackout refusal between two unconfirmed
// logins does not hide that they were consecutive.
func TestRunsWithoutALoginDoNotResetTheUnconfirmedCount(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{errSessionGone}
	h.expect(h.run(), exitLoginUnconfirmed, 1, 0)

	h.clock = h.clock.Add(12 * time.Hour) // 17:00: blackout
	h.expect(h.run(), exitSuccess, 1, 0)

	h.clock = h.clock.Add(12 * time.Hour)
	h.loginResults = []error{nil, errBeforeSubmit, errBeforeSubmit}
	h.expect(h.run(), exitTransient, 3, 0)
	if st := h.store.state; st.Blocked || st.UnconfirmedCount != 1 {
		t.Fatalf("state = %+v", st)
	}

	h.clock = h.clock.Add(24 * time.Hour)
	h.probeResults = []error{errSessionGone, errSessionGone}
	h.expect(h.run(), exitLoginUnconfirmed, 4, 0)
	if !h.store.state.Blocked {
		t.Fatalf("the second unconfirmed login did not block: %+v", h.store.state)
	}
}

// Every way a login can come back without a usable session counts.
func TestEveryKindOfUnconfirmedLoginCounts(t *testing.T) {
	t.Run("the home page could not be read", func(t *testing.T) {
		h := newHarness(t, morning(t))
		h.probeResults = []error{errNetwork, errNetwork, errNetwork}
		h.expect(h.run(), exitLoginUnconfirmed, 1, 0)
		if h.probes != verifyAttempts || h.store.state.UnconfirmedCount != 1 {
			t.Errorf("probes = %d, count = %d", h.probes, h.store.state.UnconfirmedCount)
		}
	})
	t.Run("no cookies", func(t *testing.T) {
		h := newHarness(t, morning(t))
		d := h.deps()
		d.login = func(context.Context) (*pslib.SessionExport, error) {
			h.logins++
			return &pslib.SessionExport{Cookies: []*http.Cookie{{Name: "x"}}, ExpiresAt: h.clock.Add(time.Hour)}, nil
		}
		code := run(context.Background(), h.cfg, d)
		h.expect(code, exitLoginUnconfirmed, 1, 0)
		if h.store.state.UnconfirmedCount != 1 {
			t.Errorf("count = %d", h.store.state.UnconfirmedCount)
		}
	})
	t.Run("no session at all", func(t *testing.T) {
		h := newHarness(t, morning(t))
		d := h.deps()
		d.login = func(context.Context) (*pslib.SessionExport, error) { h.logins++; return nil, nil }
		code := run(context.Background(), h.cfg, d)
		h.expect(code, exitLoginUnconfirmed, 1, 0)
		if h.store.state.UnconfirmedCount != 1 {
			t.Errorf("count = %d", h.store.state.UnconfirmedCount)
		}
	})
}

func TestRunRetriesTheConfirmingReadButNeverTheLogin(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{errNetwork, errNetwork}
	h.expect(h.run(), exitSuccess, 1, 1)
	if h.probes != 3 {
		t.Errorf("probes = %d, want 3", h.probes)
	}

	h = newHarness(t, morning(t))
	h.probeResults = []error{errSessionGone}
	h.expect(h.run(), exitLoginUnconfirmed, 1, 0)
	if h.probes != 1 {
		t.Errorf("probes = %d, want 1: a refused session is not asked about again", h.probes)
	}
}

// --- the sign-in page check (M3) -------------------------------------------

func TestRunDoesNotStartTheBrowserOnAnUnrecognisedSignInPage(t *testing.T) {
	h := newHarness(t, morning(t))
	h.signInErr = &signInError{Reason: "form_posts_off_host"}
	h.expect(h.run(), exitSignInUnrecognised, 0, 0)
	if h.signInChecks != 1 {
		t.Errorf("sign-in checks = %d, want 1: it is not retried within a run", h.signInChecks)
	}
	if !h.logged(`"outcome":"signin_page_unrecognised"`) || !h.logged(`"reason":"form_posts_off_host"`) {
		t.Errorf("log:\n%s", h.logs.String())
	}
	if msg := h.lastTermination(); !strings.Contains(msg, "outcome=signin_page_unrecognised exit=7") {
		t.Errorf("termination message = %q", msg)
	}
	if h.store.stateWrites != 0 {
		t.Error("state was written although no login was attempted")
	}
}

func TestRunDoesNotStartTheBrowserWhenTheSignInPageCannotBeFetched(t *testing.T) {
	h := newHarness(t, morning(t))
	h.signInErr = errors.New("dial tcp: lookup portal.example.org: no such host")
	h.expect(h.run(), exitTransient, 0, 0)
	if h.signInChecks != 1 {
		t.Errorf("sign-in checks = %d, want 1", h.signInChecks)
	}
}

func TestRunChecksTheSignInPageOnceBeforeLoggingIn(t *testing.T) {
	h := newHarness(t, morning(t))
	h.loginResults = []error{errBeforeSubmit}
	h.expect(h.run(), exitSuccess, 2, 1)
	if h.signInChecks != 1 {
		t.Errorf("sign-in checks = %d, want 1", h.signInChecks)
	}
}

// --- vault ----------------------------------------------------------------

func vaultStatus(code int) error {
	return &vaultapi.ResponseError{HTTPMethod: "PUT", URL: "https://vault.example.org:8200/v1/secret/data/x", StatusCode: code}
}

func TestRunDoesNotLogInWhenVaultCannotBeUsed(t *testing.T) {
	t.Run("authentication fails", func(t *testing.T) {
		h := newHarness(t, morning(t))
		h.connectErr = errors.New("vault k8s login: permission denied")
		h.expect(h.run(), exitTransient, 0, 0)
	})
	t.Run("read refused", func(t *testing.T) {
		h := newHarness(t, morning(t))
		h.store.readErr = vaultStatus(403)
		h.expect(h.run(), exitVaultRejected, 0, 0)
	})
	t.Run("read unavailable", func(t *testing.T) {
		h := newHarness(t, morning(t))
		h.store.readErr = vaultStatus(503)
		h.expect(h.run(), exitTransient, 0, 0)
	})
	t.Run("token cannot write", func(t *testing.T) {
		h := newHarness(t, morning(t))
		h.store.canWrite = false
		h.expect(h.run(), exitVaultRejected, 0, 0)
	})
}

// Being unable to ask about capabilities is not a refusal.
func TestRunProceedsWhenTheCapabilityCheckItselfFails(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.canWrite, h.store.canWriteErr = false, vaultStatus(403)
	h.expect(h.run(), exitSuccess, 1, 1)
}

func TestRunRetriesATransientWriteWithoutLoggingInAgain(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.writeErrs = []error{vaultStatus(503), errors.New("connection reset by peer")}
	h.expect(h.run(), exitSuccess, 1, 3)

	h = newHarness(t, morning(t))
	h.store.writeErrs = []error{vaultStatus(503), vaultStatus(503), vaultStatus(503), vaultStatus(503)}
	h.expect(h.run(), exitTransient, 1, writeAttempts)
}

func TestRunDoesNotRetryARefusedWrite(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.writeErrs = []error{vaultStatus(403)}
	h.expect(h.run(), exitVaultRejected, 1, 1)
}

// The Vault token is revoked at the end of every run that obtained one.
func TestRunRevokesTheVaultTokenOnEveryPath(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"success":           func(h *harness) {},
		"session good":      func(h *harness) { h.store.stored = storedAt(h.t, h.clock.Add(-time.Hour)) },
		"rejected":          func(h *harness) { h.loginResults = []error{errRejected} },
		"unconfirmed":       func(h *harness) { h.probeResults = []error{errSessionGone} },
		"write refused":     func(h *harness) { h.store.writeErrs = []error{vaultStatus(403)} },
		"state unreadable":  func(h *harness) { h.store.readStateErr = vaultStatus(403) },
		"sign-in not known": func(h *harness) { h.signInErr = &signInError{Reason: "no_password_form"} },
		"blocked": func(h *harness) {
			h.store.state = &loginState{Salt: testSalt, Blocked: true, BlockedReason: blockedCredentialsRejected,
				CredentialFingerprint: credentialFingerprint(testSalt, testUsername, testPassword)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, morning(t))
			setup(h)
			h.run()
			if h.store.closes != 1 {
				t.Errorf("Close calls = %d, want 1", h.store.closes)
			}
		})
	}
}

// --- check-and-set on the session write ------------------------------------

// The write names the version read before the login.
func TestRunWritesTheSessionCheckAndSet(t *testing.T) {
	h := newHarness(t, morning(t))
	h.expect(h.run(), exitSuccess, 1, 1)
	if h.store.writtenCAS[0] != 0 {
		t.Errorf("first write cas = %d, want 0 (must not exist yet)", h.store.writtenCAS[0])
	}
	h.clock = h.clock.Add(24 * time.Hour)
	h.expect(h.run(), exitSuccess, 2, 2)
	if h.store.writtenCAS[1] != 1 {
		t.Errorf("second write cas = %d, want 1", h.store.writtenCAS[1])
	}
}

// Another run (a manual Job beside the scheduled one) stored a session
// while this one was logging in. The portal keeps only the later login.
func TestRunDoesNotStoreASessionAnotherRunSuperseded(t *testing.T) {
	overlap := func(h *harness) {
		first := true
		h.store.onRead = func() {
			if first { // after this run's read, the other run writes
				first = false
				h.store.version++
				h.store.stored = storedAt(h.t, h.clock)
			}
		}
	}

	t.Run("the other login was later: ours is dead and is not stored", func(t *testing.T) {
		h := newHarness(t, morning(t))
		overlap(h)
		h.probeResults = []error{nil, errSessionGone} // confirmed, then refused on the re-check
		h.expect(h.run(), exitTransient, 1, 1)
		if len(h.store.written) != 0 || h.store.version != 1 {
			t.Errorf("the superseded session replaced the other run's")
		}
		if !h.logged(`"outcome":"superseded"`) {
			t.Errorf("log:\n%s", h.logs.String())
		}
	})

	t.Run("the other login was earlier: ours is alive and replaces it", func(t *testing.T) {
		h := newHarness(t, morning(t))
		overlap(h)
		h.expect(h.run(), exitSuccess, 1, 2)
		if len(h.store.written) != 1 || h.store.writtenCAS[0] != 1 || h.store.version != 2 {
			t.Errorf("written = %d, cas = %v, version = %d", len(h.store.written), h.store.writtenCAS, h.store.version)
		}
		if h.probes != 2 {
			t.Errorf("probes = %d, want the confirmation and one re-check", h.probes)
		}
	})
}

// --- what is written ------------------------------------------------------

func TestRunWritesTheSessionWithItsLoginTimeAndExpiry(t *testing.T) {
	h := newHarness(t, morning(t))
	h.expect(h.run(), exitSuccess, 1, 1)

	data := h.store.written[0]
	for _, key := range []string{"session", "logged_in_at", "expires_at", "selected_student_id"} {
		if _, ok := data[key].(string); !ok {
			t.Fatalf("field %q missing or not a string: %#v", key, data[key])
		}
	}
	if len(data) != 4 {
		t.Errorf("unexpected fields: %v", data)
	}
	if got, want := data["logged_in_at"], h.clock.UTC().Format(time.RFC3339); got != want {
		t.Errorf("logged_in_at = %v, want %v", got, want)
	}
	wantExpiry := h.clock.Add(defaultSessionLifetime).UTC().Format(time.RFC3339)
	if data["expires_at"] != wantExpiry {
		t.Errorf("expires_at = %v, want login time + %s = %v", data["expires_at"], defaultSessionLifetime, wantExpiry)
	}
	if data["selected_student_id"] != testStudentID {
		t.Errorf("selected_student_id = %v", data["selected_student_id"])
	}

	var doc pslib.SessionExport
	if err := json.Unmarshal([]byte(data["session"].(string)), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.ExpiresAt.UTC().Format(time.RFC3339); got != wantExpiry {
		t.Errorf("session expires_at = %v, want %v (not the library's four-hour guess)", got, wantExpiry)
	}
	if len(doc.Cookies) != 2 {
		t.Errorf("cookies = %d, want the 2 with a name and a value", len(doc.Cookies))
	}
	if msg := h.lastTermination(); !strings.Contains(msg, "outcome=success exit=0") {
		t.Errorf("termination message = %q", msg)
	}
}

// What this binary writes is what the connector reads. The document is
// delivered the way the chart delivers it -- the Vault "session" property
// as the contents of a file -- and loaded with the connector's own
// LoadSession, so the two cannot drift apart.
func TestWrittenSessionLoadsWithTheConnectorsOwnLoader(t *testing.T) {
	h := newHarness(t, morning(t))
	h.expect(h.run(), exitSuccess, 1, 1)
	doc := h.store.written[0]["session"].(string)

	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "POWERSCHOOL_SESSION_FILE" {
			return path
		}
		return ""
	}

	// Still loadable late on the same day, for the evening poll...
	loaded, err := powerschool.LoadSession(getenv, h.clock.Add(17*time.Hour))
	if err != nil {
		t.Fatalf("connector LoadSession: %v", err)
	}
	// ...and refused once past its expiry.
	if _, err := powerschool.LoadSession(getenv, h.clock.Add(defaultSessionLifetime+time.Second)); err == nil {
		t.Error("the connector accepted the session after its expires_at")
	}

	want := normalizeSession(librarySession(h.clock), h.clock.Add(defaultSessionLifetime))
	if len(loaded.Cookies) != len(want.Cookies) {
		t.Fatalf("cookies = %d, want %d", len(loaded.Cookies), len(want.Cookies))
	}
	for i, c := range want.Cookies {
		got := loaded.Cookies[i]
		if got.Name != c.Name || got.Value != c.Value || got.Path != c.Path || got.Domain != c.Domain ||
			got.Secure != c.Secure || got.HttpOnly != c.HttpOnly || got.SameSite != c.SameSite ||
			!got.Expires.Equal(c.Expires) {
			t.Errorf("cookie %d differs after the round trip:\n got %+v\nwant %+v", i, got, c)
		}
	}
	if !loaded.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("expires_at = %s, want %s", loaded.ExpiresAt, want.ExpiresAt)
	}
	// The connector recognises a session by this fingerprint; the same
	// cookies must give the same one on both sides.
	if powerschool.SessionFingerprint(loaded) != powerschool.SessionFingerprint(want) {
		t.Error("the session fingerprint changed across the round trip")
	}

	// And the next run of this binary reads its own document back.
	if _, err := powerschool.ParseSession([]byte(doc), h.clock); err != nil {
		t.Errorf("ParseSession on our own document: %v", err)
	}
}

// --- nothing secret in the logs ------------------------------------------

func TestRunRedactsCredentialsFromErrorText(t *testing.T) {
	leaky := fmt.Errorf("could not type %q for user %q into the form", testPassword, testUsername)

	h := newHarness(t, morning(t))
	h.loginResults = []error{&loginError{kind: loginFailedBeforeSubmit, err: leaky}, &loginError{kind: loginFailedUnknown, err: leaky}}
	h.expect(h.run(), exitTransient, 2, 0) // run() asserts no secret was logged
	if !h.logged("[redacted]") {
		t.Errorf("the error text was not passed through redact:\n%s", h.logs.String())
	}

	h = newHarness(t, morning(t))
	h.connectErr = leaky
	h.expect(h.run(), exitTransient, 0, 0)

	h = newHarness(t, morning(t))
	h.store.writeErrs = []error{fmt.Errorf("permission denied for %s", testPassword)}
	h.expect(h.run(), exitVaultRejected, 1, 1)

	h = newHarness(t, morning(t))
	h.signInErr = leaky
	h.expect(h.run(), exitTransient, 0, 0)

	h = newHarness(t, morning(t))
	h.loginResults = []error{errRejected}
	h.store.writeStateErrs = []error{nil, fmt.Errorf("permission denied for %s", testPassword)}
	h.expect(h.run(), exitBadCredentials, 1, 0)
	if !h.logged("COULD NOT RECORD THE LOGIN BLOCK") {
		t.Errorf("a latch that could not be written was not reported:\n%s", h.logs.String())
	}
}

// A portal-side error is never logged as text: its message can carry a
// request URL or page content. Only its class reaches the log.
func TestRunLogsPortalErrorsByClassOnly(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.stored = storedAt(t, h.clock.Add(-time.Hour))
	h.probeResults = []error{pslib.WrapNetworkError(fmt.Errorf(
		"GET https://portal.example.org/guardian/home.html?frn=%s: cookie %s", testStudentID, testCookieValue))}
	h.expect(h.run(), exitTransient, 0, 0)
	if !h.logged(`"error_class":"network"`) {
		t.Errorf("no error_class in:\n%s", h.logs.String())
	}
	if h.logged("frn=") {
		t.Errorf("the error text reached the log:\n%s", h.logs.String())
	}
}

func TestRedactBoundsLength(t *testing.T) {
	cfg := config{Username: "u-name", Password: "p-word"}
	got := cfg.redact(strings.Repeat("x", 5000) + "p-word")
	if len(got) > 450 || strings.Contains(got, "p-word") {
		t.Errorf("redact returned %d bytes", len(got))
	}
	if got := (config{}).redact("nothing to hide"); got != "nothing to hide" {
		t.Errorf("redact with empty secrets = %q", got)
	}
}

// --- request mode -----------------------------------------------------------

// storedFP is the full fingerprint of a stored session, as the connector
// would compute it from the same document.
func storedFP(t *testing.T, s *storedSession, now time.Time) string {
	t.Helper()
	parsed, err := powerschool.ParseSession([]byte(s.Document), now)
	if err != nil {
		t.Fatal(err)
	}
	return powerschool.SessionFingerprint(parsed)
}

// The rule the owner set: no keepalive. A run with no pending request does
// nothing at the portal -- no login, no sign-in check, no look at the
// stored session -- whatever state that session is in.
func TestRequestModeWithoutARequestContactsNothing(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"empty field":            func(h *harness) {},
		"blank field":            func(h *harness) { h.request = "  \n" },
		"not a request":          func(h *harness) { h.request = "log in now please" },
		"oversized":              func(h *harness) { h.request = strings.Repeat("2026-10-12T13:05:09Z ", 500) },
		"a json object":          func(h *harness) { h.request = `{"request":"now"}` },
		"upper-case fingerprint": func(h *harness) { h.request = "2026-10-12T12:00:00Z 5F2A9C0E11D24B7A" },
		"stale (16 minutes)":     func(h *harness) { h.ask(16*time.Minute, "") },
		"stale (yesterday)":      func(h *harness) { h.ask(26*time.Hour, "") },
		"dated in the future":    func(h *harness) { h.ask(-3*time.Minute, "") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, morning(t)).requestMode()
			// An old, dead-looking session in Vault: without a request
			// that is nobody's business.
			h.store.stored = storedAt(t, h.clock.Add(-30*time.Hour))
			setup(h)
			code := h.run()
			h.expect(code, exitSuccess, 0, 0)
			if h.portalContacts() != 0 {
				t.Errorf("the portal was contacted: probes %d, sign-in checks %d, logins %d", h.probes, h.signInChecks, h.logins)
			}
			if h.connects != 0 {
				t.Errorf("Vault was contacted %d time(s) although there was nothing to compare a request with", h.connects)
			}
			if h.requestReads != 1 {
				t.Errorf("request reads = %d, want 1", h.requestReads)
			}
			if !h.logged(`"outcome":"no_login_requested"`) {
				t.Errorf("no no_login_requested line:\n%s", h.logs.String())
			}
			if msg := h.lastTermination(); !strings.Contains(msg, "outcome=no_login_requested exit=0") {
				t.Errorf("termination message = %q", msg)
			}
			// What the field held is never echoed.
			if h.request != "" && len(h.request) > 10 && strings.Contains(h.logs.String(), strings.TrimSpace(h.request)) {
				t.Errorf("the request field was echoed into the log")
			}
		})
	}
}

// The same, with the REAL portal clients pointed at a server that fails the
// test on any request at all: fifty runs without a pending request, with a
// stored session of every age, and not one byte goes to the portal.
func TestRequestModeWithoutARequestMakesZeroPortalRequests(t *testing.T) {
	p := newPortal(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the portal received %s %s from a run with no pending login request", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusTeapot)
	})
	portalCfg := p.cfg()

	h := newHarness(t, morning(t)).requestMode()
	h.cfg.BaseURL, h.cfg.Host = portalCfg.BaseURL, portalCfg.Host
	d := h.deps()
	d.probe = realProbe(h.cfg, p.srv.Client().Transport)
	d.checkSignIn = realSignInCheck(h.cfg, p.srv.Client().Transport)
	d.login = func(context.Context) (*pslib.SessionExport, error) {
		t.Error("a login was started without a pending request")
		return nil, errors.New("no")
	}

	ages := []time.Duration{time.Minute, 20 * time.Minute, 2 * time.Hour, 11 * time.Hour, 40 * time.Hour}
	requests := []string{"", "garbage", psportal.FormatLoginRequest(h.clock.Add(-time.Hour), "")}
	for i := 0; i < 50; i++ {
		h.store.stored = storedAt(t, h.clock.Add(-ages[i%len(ages)]))
		h.request = requests[i%len(requests)]
		if code := run(context.Background(), h.cfg, d); code != exitSuccess {
			t.Fatalf("run %d: exit %d\n%s", i, code, h.logs.String())
		}
		h.clock = h.clock.Add(3 * time.Minute)
		if h.cfg.Blackout.blocksLoginAt(h.clock) {
			h.clock = morning(t).Add(24 * time.Hour)
		}
	}
	if got := len(p.paths()); got != 0 {
		t.Fatalf("%d request(s) reached the portal: %v", got, p.paths())
	}

	// And a served request, and a request older than the last login --
	// both need Vault, neither needs the portal.
	served := psportal.FormatLoginRequest(h.clock.Add(-time.Minute), "")
	h.store.state = &loginState{Salt: testSalt, ServedRequest: served}
	h.request = served
	if code := run(context.Background(), h.cfg, d); code != exitSuccess {
		t.Fatalf("served request: exit %d", code)
	}
	h.store.state = nil
	h.store.stored = storedAt(t, h.clock.Add(-30*time.Second))
	h.request = psportal.FormatLoginRequest(h.clock.Add(-time.Minute), "")
	if code := run(context.Background(), h.cfg, d); code != exitSuccess {
		t.Fatalf("request older than the last login: exit %d", code)
	}
	if got := len(p.paths()); got != 0 {
		t.Fatalf("%d request(s) reached the portal: %v", got, p.paths())
	}
}

// A pending request about the very session Vault holds: the connector has
// just asked the portal about it, so this run does not ask again. It logs
// in, once, and records the request as served.
func TestRequestModeLogsInForAPendingRequest(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.store.stored = storedAt(t, h.clock.Add(-40*time.Minute))
	h.store.version = 3
	raw := h.ask(90*time.Second, storedFP(t, h.store.stored, h.clock))

	code := h.run()
	h.expect(code, exitSuccess, 1, 1)
	if h.probes != 1 {
		t.Errorf("probes = %d, want only the confirmation of the new session", h.probes)
	}
	if h.signInChecks != 1 {
		t.Errorf("sign-in checks = %d", h.signInChecks)
	}
	st := h.store.state
	if st.ServedRequest != raw || !st.RequestLoginAt.Equal(h.clock.UTC()) {
		t.Errorf("state after serving: served %q at %s, want %q at %s", st.ServedRequest, st.RequestLoginAt, raw, h.clock.UTC())
	}
	if !h.logged("the stored session is the one the connector found dead") {
		t.Errorf("log:\n%s", h.logs.String())
	}
}

// Served once. The same request is still in the field at the next runs
// (the connector does not clear it); nothing more happens for it.
func TestRequestModeServesARequestOnce(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.ask(time.Minute, "")
	h.expect(h.run(), exitSuccess, 1, 1)

	for i := 0; i < 5; i++ {
		h.clock = h.clock.Add(3 * time.Minute)
		contacts := h.portalContacts()
		h.expect(h.run(), exitSuccess, 1, 1)
		if h.portalContacts() != contacts {
			t.Fatalf("run %d contacted the portal for a request already served", i+2)
		}
	}
	if !h.logged("already been acted on") && !h.logged("newer than the pending request") {
		t.Errorf("log:\n%s", h.logs.String())
	}
}

// A request gets ONE login, whatever comes of it. A login that failed after
// the credentials may have been submitted is not repeated three minutes
// later for the same request.
func TestRequestModeDoesNotRepeatAFailedLoginForTheSameRequest(t *testing.T) {
	for name, failure := range map[string]error{"unplaceable": errUnknown, "before submit, twice": errBeforeSubmit} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, morning(t)).requestMode()
			h.ask(time.Minute, "")
			h.loginResults = []error{failure, failure}
			h.run()
			logins := h.logins
			for i := 0; i < 4; i++ {
				h.clock = h.clock.Add(3 * time.Minute)
				h.run()
			}
			if h.logins != logins {
				t.Errorf("logins went from %d to %d for one request", logins, h.logins)
			}
		})
	}
}

// A failure BEFORE any login was started (Vault down, the sign-in page
// unreachable) leaves the request pending: the next run tries again.
func TestRequestModeRetriesWhenNoLoginWasStarted(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.ask(time.Minute, "")
	h.signInErr = errors.New("dial tcp: no route to host")
	h.expect(h.run(), exitTransient, 0, 0)

	h.signInErr = nil
	h.clock = h.clock.Add(3 * time.Minute)
	h.expect(h.run(), exitSuccess, 1, 1)
}

// The bound on a connector that has been taken over: however many fresh
// requests it writes, logins made for requests are at least 20 minutes
// apart. Enforced here, from this workload's own record.
func TestRequestModeMinimumIntervalBetweenRequestLogins(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.cfg.MinSessionAge = 5 * time.Minute // what the chart sets on the on-request CronJobs
	h.ask(time.Minute, "")
	h.expect(h.run(), exitSuccess, 1, 1)

	// A new request every three minutes, each about a "different" session.
	for minute := 3; minute < 20; minute += 3 {
		h.clock = morning(t).Add(time.Duration(minute) * time.Minute)
		h.ask(10*time.Second, fmt.Sprintf("%064d", minute))
		contacts := h.portalContacts()
		code := h.run()
		h.expect(code, exitSuccess, 1, 1)
		if h.portalContacts() != contacts {
			t.Fatalf("minute %d: the portal was contacted inside the minimum interval", minute)
		}
		if !h.logged(`"outcome":"login_request_too_soon"`) {
			t.Fatalf("minute %d: no login_request_too_soon line", minute)
		}
	}
	if msg := h.lastTermination(); !strings.Contains(msg, "outcome=login_request_too_soon exit=0") {
		t.Errorf("termination message = %q", msg)
	}

	h.clock = morning(t).Add(20 * time.Minute)
	h.ask(10*time.Second, fmt.Sprintf("%064d", 99))
	h.expect(h.run(), exitSuccess, 2, 2)

	// Over a whole day of a request every three minutes: at most three
	// logins an hour, and none in the blackout.
	h = newHarness(t, time.Date(2026, 10, 12, 0, 0, 0, 0, mustZone(t, "America/Los_Angeles"))).requestMode()
	h.cfg.MinSessionAge = 5 * time.Minute
	start := h.clock
	for i := 0; i < 480; i++ {
		h.clock = start.Add(time.Duration(i) * 3 * time.Minute)
		h.ask(5*time.Second, fmt.Sprintf("%064d", i))
		before := h.logins
		h.run()
		if h.logins > before && h.cfg.Blackout.blocksLoginAt(h.clock) {
			t.Fatalf("a login was made at %s, inside the blackout", h.clock.Format("15:04"))
		}
	}
	// 24h minus the blackout (5h + 2.5 min) at one per 21 minutes (the
	// first 3-minute tick at or after 20 minutes): at most 55.
	if h.logins > 55 || h.logins < 40 {
		t.Errorf("logins over a day of constant requests = %d, want roughly 54 and never more than 55", h.logins)
	}
}

func TestRequestModeMinimumIntervalHasAFloor(t *testing.T) {
	env := minimalEnv()
	env["POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP"] = "cm"
	cfg, problems := loadConfig(envOf(env))
	if len(problems) != 0 || !cfg.requestMode() || cfg.RequestMinInterval != 20*time.Minute || cfg.RequestMaxAge != 15*time.Minute {
		t.Fatalf("defaults: %+v, problems %v", cfg, problems)
	}
	env["POWERSCHOOL_LOGIN_REQUEST_MIN_INTERVAL"] = "1m"
	if _, problems := loadConfig(envOf(env)); len(problems) == 0 {
		t.Error("a 1m interval between request logins was accepted")
	}
	env["POWERSCHOOL_LOGIN_REQUEST_MIN_INTERVAL"] = "45m"
	env["POWERSCHOOL_LOGIN_REQUEST_MAX_AGE"] = "30m"
	env["POWERSCHOOL_LOGIN_REQUEST_NAMESPACE"] = "openclaw"
	cfg, problems = loadConfig(envOf(env))
	if len(problems) != 0 || cfg.RequestMinInterval != 45*time.Minute || cfg.RequestMaxAge != 30*time.Minute || cfg.RequestNamespace != "openclaw" {
		t.Errorf("overrides: %+v, problems %v", cfg, problems)
	}
	env["POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP"] = "a/b"
	if _, problems := loadConfig(envOf(env)); len(problems) == 0 {
		t.Error("a path was accepted as a ConfigMap name")
	}
	// Not configured: not request mode.
	if cfg, _ := loadConfig(envOf(minimalEnv())); cfg.requestMode() {
		t.Error("request mode is on without a ConfigMap")
	}
}

// Vault holds a different session from the one the connector saw die (the
// connector's copy was stale). If the portal still accepts it there is
// nothing to log in for; the request is recorded as answered so that the
// next runs do not keep asking the portal about it.
func TestRequestModeAnswersWithAGoodStoredSessionAndDoesNotAskAgain(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.cfg.MinSessionAge = 10 * time.Hour
	h.store.stored = storedAt(t, h.clock.Add(-5*time.Minute))
	raw := h.ask(time.Minute, strings.Repeat("ab", 32)) // some other session
	// The stored session predates the request, so it is not "a newer
	// login"; it is young, so it is checked.
	h.expect(h.run(), exitSuccess, 0, 0)
	if h.probes != 1 || !h.logged("session still good") {
		t.Fatalf("probes = %d\n%s", h.probes, h.logs.String())
	}
	if h.store.state.ServedRequest != raw {
		t.Errorf("the answered request was not recorded: %+v", h.store.state)
	}
	for i := 0; i < 4; i++ {
		h.clock = h.clock.Add(3 * time.Minute)
		h.expect(h.run(), exitSuccess, 0, 0)
	}
	if h.probes != 1 {
		t.Errorf("probes = %d: the stored session was checked again for the same request (a keepalive by another name)", h.probes)
	}
}

// A login newer than the request is already in Vault: done, without Vault
// state and without the portal.
func TestRequestModeIgnoresARequestOlderThanTheLastLogin(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.store.stored = storedAt(t, h.clock.Add(-30*time.Second))
	h.ask(2*time.Minute, "")
	h.expect(h.run(), exitSuccess, 0, 0)
	if h.portalContacts() != 0 || h.store.stateWrites != 0 {
		t.Errorf("portal contacts %d, state writes %d", h.portalContacts(), h.store.stateWrites)
	}
}

func TestRequestModeCannotReadTheRequest(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.requestErr = errors.New("kubernetes get configmap: HTTP 403 Forbidden")
	h.expect(h.run(), exitTransient, 0, 0)
	if h.portalContacts() != 0 || h.connects != 0 {
		t.Errorf("portal contacts %d, vault connects %d", h.portalContacts(), h.connects)
	}
}

// The blackout comes first, as always: the request is not even read.
func TestRequestModeBlackoutComesFirst(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	h := newHarness(t, time.Date(2026, 10, 12, 17, 0, 0, 0, la)).requestMode()
	h.ask(time.Minute, "")
	h.expect(h.run(), exitSuccess, 0, 0)
	if h.requestReads != 0 || h.portalContacts() != 0 || !h.logged(`"outcome":"blackout_refused"`) {
		t.Errorf("request reads %d, portal contacts %d", h.requestReads, h.portalContacts())
	}
}

// The latch outranks a request.
func TestRequestModeLatchStillBlocks(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.store.state = &loginState{Salt: testSalt, Blocked: true, BlockedReason: blockedCredentialsRejected,
		CredentialFingerprint: credentialFingerprint(testSalt, testUsername, testPassword)}
	h.ask(time.Minute, "")
	h.expect(h.run(), exitLoginBlocked, 0, 0)
	if h.portalContacts() != 0 {
		t.Error("the portal was contacted with the latch set")
	}
}

// The operator's manual run: the bypass makes the binary behave as a
// scheduled run, ignoring the request field and the request interval. The
// connector cannot set it: it is an environment variable on a Job.
func TestRequestBypassIsAnOrdinaryRun(t *testing.T) {
	h := newHarness(t, morning(t)).requestMode()
	h.cfg.RequestBypass = true
	h.store.state = &loginState{Salt: testSalt, RequestLoginAt: h.clock.Add(-time.Minute)}
	h.expect(h.run(), exitSuccess, 1, 1)
	if h.requestReads != 0 {
		t.Errorf("the request was read %d time(s) in a bypass run", h.requestReads)
	}
	// It does not disturb the request bookkeeping.
	if !h.store.state.RequestLoginAt.Equal(h.clock.Add(-time.Minute)) || h.store.state.ServedRequest != "" {
		t.Errorf("state = %+v", h.store.state)
	}

	// Every other rule still holds for it: the blackout...
	h = newHarness(t, time.Date(2026, 10, 12, 17, 0, 0, 0, mustZone(t, "America/Los_Angeles"))).requestMode()
	h.cfg.RequestBypass = true
	h.expect(h.run(), exitSuccess, 0, 0)
	// ...and the young-session check.
	h = newHarness(t, morning(t)).requestMode()
	h.cfg.RequestBypass = true
	h.store.stored = storedAt(t, h.clock.Add(-time.Hour))
	h.expect(h.run(), exitSuccess, 0, 0)
}

// Without request mode nothing about a scheduled run changes, and the
// request is never read.
func TestScheduledRunsDoNotReadTheRequest(t *testing.T) {
	h := newHarness(t, morning(t))
	h.request = "2026-10-12T12:00:00Z none"
	h.expect(h.run(), exitSuccess, 1, 1)
	if h.requestReads != 0 || h.store.state.ServedRequest != "" || !h.store.state.RequestLoginAt.IsZero() {
		t.Errorf("request reads %d, state %+v", h.requestReads, h.store.state)
	}
}
