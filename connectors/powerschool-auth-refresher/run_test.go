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
	pslib "github.com/leftathome/powerschool-go"
)

// Values that must never reach a log line. None is a real credential.
const (
	testUsername    = "parent-account-7731"
	testPassword    = "correct-horse-battery-staple-9"
	testCookieValue = "cookie-value-5b1e7d9c4a"
	testStudentID   = "424242"
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

// fakeStore is an in-memory sessionStore.
type fakeStore struct {
	stored      *storedSession
	readErr     error
	canWrite    bool
	canWriteErr error
	writeErrs   []error // consumed one per Write; nil entry or exhausted = success

	reads, capChecks, writes int
	written                  []map[string]any
}

func (f *fakeStore) Read(context.Context) (*storedSession, error) {
	f.reads++
	return f.stored, f.readErr
}

func (f *fakeStore) CanWrite(context.Context) (bool, error) {
	f.capChecks++
	return f.canWrite, f.canWriteErr
}

func (f *fakeStore) Write(_ context.Context, data map[string]any) (int, error) {
	f.writes++
	if n := f.writes - 1; n < len(f.writeErrs) && f.writeErrs[n] != nil {
		return 0, f.writeErrs[n]
	}
	f.written = append(f.written, data)
	return 7, nil
}

// harness wires run() to fakes and counts what happened.
type harness struct {
	t     *testing.T
	cfg   config
	clock time.Time
	store *fakeStore
	logs  bytes.Buffer

	connectErr error
	connects   int

	// loginResults is consumed one per login; when exhausted a login
	// succeeds. A nil entry is a success.
	loginResults []error
	logins       int

	// probeResults is consumed one per probe; when exhausted a probe
	// succeeds.
	probeResults []error
	probes       int

	slept []time.Duration
	// onSleep, when set, runs on every sleep (to move the clock).
	onSleep func(time.Duration)
	// onConnect, when set, runs when Vault is contacted.
	onConnect func()
}

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
		login: func(context.Context) (*pslib.SessionExport, error) {
			h.logins++
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
		logger: slog.New(slog.NewJSONHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (h *harness) run() int {
	h.t.Helper()
	code := run(context.Background(), h.cfg, h.deps())
	h.assertNoSecretsLogged()
	return code
}

// assertNoSecretsLogged is applied to every run in this file.
func (h *harness) assertNoSecretsLogged() {
	h.t.Helper()
	out := h.logs.String()
	for name, secret := range map[string]string{
		"password": testPassword, "username": testUsername,
		"cookie value": testCookieValue, "student id": testStudentID,
	} {
		if strings.Contains(out, secret) {
			h.t.Errorf("the %s appears in the log output:\n%s", name, out)
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
		h.t.Errorf("vault writes = %d, want %d", h.store.writes, wantWrites)
	}
}

func (h *harness) logged(substr string) bool { return strings.Contains(h.logs.String(), substr) }

var (
	errBeforeSubmit = &loginError{kind: loginFailedBeforeSubmit, err: errors.New("page load error net::ERR_NAME_NOT_RESOLVED")}
	errRejected     = &loginError{kind: loginRejected, err: errors.New("login failed: invalid credentials")}
	errUnknown      = &loginError{kind: loginFailedUnknown, err: errors.New("context deadline exceeded")}
	errSessionGone  = pslib.NewError(pslib.CodeSessionExpired, "the portal returned its login page", pslib.ErrSessionExpired)
	errNetwork      = pslib.WrapNetworkError(errors.New("dial tcp: connection refused"))
)

// --- blackout -------------------------------------------------------------

// The owner's rule, end to end through run(): a run at 16:00:00, 18:30 or
// 20:59:59 Pacific contacts nothing and exits 0; one at 15:59:59 or 21:00:00
// logs in. On every kind of day, both daylight-saving changes included, and
// with the process clock in UTC as it is in the pod.
func TestRunRefusesInsideTheBlackoutAndProceedsOutsideIt(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	for _, day := range blackoutDays {
		for _, c := range blackoutClock {
			at := time.Date(2026, time.Month(day.month), day.day, c.h, c.m, c.s, 0, la).UTC()
			name := fmt.Sprintf("%s %02d:%02d:%02d", day.name, c.h, c.m, c.s)
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, at)
				code := h.run()
				if c.inside {
					h.expect(code, exitSuccess, 0, 0)
					if h.connects != 0 || h.probes != 0 {
						t.Errorf("a refused run contacted Vault (%d) or the portal (%d)", h.connects, h.probes)
					}
					if !h.logged(`"outcome":"blackout_refused"`) || !h.logged("refusing to log in") {
						t.Errorf("no clear refusal line:\n%s", h.logs.String())
					}
					if !h.logged(`"blackout_window":"16:00-21:00"`) || !h.logged(`"blackout_timezone":"America/Los_Angeles"`) {
						t.Errorf("the refusal does not name the window:\n%s", h.logs.String())
					}
					return
				}
				h.expect(code, exitSuccess, 1, 1)
			})
		}
	}
}

// The clock is read again immediately before the login: a run that started
// at 15:59:50 and spent 20 seconds on Vault must not log in at 16:00:10.
func TestRunRechecksTheBlackoutImmediatelyBeforeLoggingIn(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	h := newHarness(t, time.Date(2026, 10, 12, 15, 59, 50, 0, la))
	h.onConnect = func() { h.clock = h.clock.Add(20 * time.Second) }
	code := h.run()
	h.expect(code, exitSuccess, 0, 0)
	if !h.logged(`"outcome":"blackout_refused"`) {
		t.Errorf("no refusal line:\n%s", h.logs.String())
	}
}

// The pause before the one permitted retry can carry the run into the
// window. The second attempt is then not made.
func TestRunDoesNotRetryIntoTheBlackout(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	h := newHarness(t, time.Date(2026, 10, 12, 15, 59, 55, 0, la))
	h.loginResults = []error{errBeforeSubmit}
	code := h.run()
	h.expect(code, exitTransient, 1, 0)
	if !h.logged(`"outcome":"blackout_refused"`) {
		t.Errorf("no refusal line:\n%s", h.logs.String())
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
}

func TestRunLogsInWhenYoungSessionIsNoLongerAccepted(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.stored = storedAt(t, h.clock.Add(-3*time.Hour))
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

func TestRunRejectedCredentialsAreNamedAndNotRetried(t *testing.T) {
	h := newHarness(t, morning(t))
	h.loginResults = []error{errRejected}
	h.expect(h.run(), exitBadCredentials, 1, 0)
	if !h.logged(`"outcome":"bad_credentials"`) {
		t.Errorf("no bad_credentials line:\n%s", h.logs.String())
	}
}

// --- confirmation ---------------------------------------------------------

func TestRunDoesNotStoreASessionThePortalDoesNotAccept(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{errSessionGone}
	h.expect(h.run(), exitLoginUnconfirmed, 1, 0)
	if h.probes != 1 {
		t.Errorf("probes = %d, want 1: a refused session is not asked about again", h.probes)
	}
}

func TestRunRetriesTheConfirmingReadButNeverTheLogin(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{errNetwork, errNetwork}
	h.expect(h.run(), exitSuccess, 1, 1)
	if h.probes != 3 {
		t.Errorf("probes = %d, want 3", h.probes)
	}

	h = newHarness(t, morning(t))
	h.probeResults = []error{errNetwork, errNetwork, errNetwork, errNetwork}
	h.expect(h.run(), exitLoginUnconfirmed, 1, 0)
	if h.probes != verifyAttempts {
		t.Errorf("probes = %d, want %d", h.probes, verifyAttempts)
	}
}

func TestRunRefusesALoginThatReturnedNoCookies(t *testing.T) {
	h := newHarness(t, morning(t))
	d := h.deps()
	d.login = func(context.Context) (*pslib.SessionExport, error) {
		h.logins++
		return &pslib.SessionExport{Cookies: []*http.Cookie{{Name: "x"}}, ExpiresAt: h.clock.Add(time.Hour)}, nil
	}
	code := run(context.Background(), h.cfg, d)
	h.expect(code, exitLoginUnconfirmed, 1, 0)
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
