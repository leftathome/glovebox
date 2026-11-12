package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/leftathome/glovebox/internal/loginvault"
	"github.com/leftathome/schoology-go/auth"
)

// Values that must never reach a log line. None is a real credential.
const (
	testUsername = "parent-account-4410@example.com"
	testPassword = "correct-horse-battery-staple-3"
	testToken    = "browserless-token-8c1f"
	testSalt     = "00112233445566778899aabbccddeeff"
)

// validCreds returns a fully-populated *auth.Credentials with the
// 5 required fields set.
func validCreds() *auth.Credentials {
	return &auth.Credentials{
		Host:      "school.schoology.com",
		SessID:    "SESS-abc",
		CSRFToken: "csrf-xyz",
		CSRFKey:   "csrf-key-1",
		UID:       "12345",
	}
}

// validConfig returns a config that passes problems().
func validConfig() config {
	return config{
		SchoologyHost:     "school.schoology.com",
		SchoologyUsername: testUsername,
		SchoologyPassword: testPassword,
		VaultAddr:         "http://vault:8200",
		VaultK8sRole:      "glovebox-schoology-refresher",
		VaultKVMount:      "secret",
		VaultSessionPath:  "glovebox/schoology/test/session",
		VaultStatePath:    "glovebox/schoology/test/login-state",
		BrowserlessToken:  testToken,
	}
}

// fakeStore is an in-memory sessionStore. It persists across runs of one
// harness, as Vault does across runs of the job.
type fakeStore struct {
	putErr  error
	puts    int
	putData map[string]any
	putVer  int

	state          *loginState // nil = nothing stored
	stateVersion   int
	readStateErr   error
	writeStateErrs []error // consumed one per WriteState; nil entry or exhausted = success
	// failStateWritesFrom makes every WriteState from this 1-based call on
	// fail with a 403 (0 = never).
	failStateWritesFrom int
	stateWrites, closes int
}

func (f *fakeStore) Put(ctx context.Context, data map[string]any) (int, error) {
	f.puts++
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if f.putErr != nil {
		return 0, f.putErr
	}
	f.putVer++
	f.putData = data
	return f.putVer, nil
}

func (f *fakeStore) ReadState(context.Context) (*loginState, error) {
	if f.readStateErr != nil {
		return nil, f.readStateErr
	}
	if f.state == nil {
		return &loginState{Version: f.stateVersion}, nil
	}
	copied := *f.state
	copied.Version = f.stateVersion
	return &copied, nil
}

func (f *fakeStore) WriteState(ctx context.Context, state *loginState) error {
	f.stateWrites++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failStateWritesFrom > 0 && f.stateWrites >= f.failStateWritesFrom {
		return &vaultapi.ResponseError{HTTPMethod: "PUT", StatusCode: 403, Errors: []string{"permission denied"}}
	}
	if n := f.stateWrites - 1; n < len(f.writeStateErrs) && f.writeStateErrs[n] != nil {
		return f.writeStateErrs[n]
	}
	if state.Version != f.stateVersion {
		return &vaultapi.ResponseError{HTTPMethod: "PUT", StatusCode: 400,
			Errors: []string{"check-and-set parameter did not match the current version"}}
	}
	f.stateVersion++
	copied := *state
	copied.Version = f.stateVersion
	f.state = &copied
	state.Version = f.stateVersion
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

	// loginResults is consumed one per login; when exhausted a login
	// succeeds. A nil entry is a success.
	loginResults []error
	// loginNil makes a successful login return no credentials.
	loginNil bool
	logins   int
	// stateAtLogin is the stored state as the login starts.
	stateAtLogin *loginState
	// cancelOnLogin ends the run's context during the login, as a login
	// that uses up the run's time does.
	cancelOnLogin bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		t:     t,
		cfg:   validConfig(),
		clock: time.Date(2026, 10, 12, 13, 30, 0, 0, time.UTC),
		store: &fakeStore{},
	}
}

func (h *harness) run() int {
	h.t.Helper()
	logger := slog.New(slog.NewJSONHandler(&h.logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return run(ctx, h.cfg, deps{
		now:   func() time.Time { return h.clock },
		sleep: func(context.Context, time.Duration) error { return nil },
		connect: func(context.Context) (sessionStore, error) {
			h.connects++
			if h.connectErr != nil {
				return nil, h.connectErr
			}
			return h.store, nil
		},
		login: func(_ context.Context, host, username, password string) (*auth.Credentials, error) {
			h.logins++
			if h.cancelOnLogin {
				cancel()
			}
			if h.store.state != nil {
				copied := *h.store.state
				h.stateAtLogin = &copied
			}
			if username != testUsername || password != h.cfg.SchoologyPassword || host != h.cfg.SchoologyHost {
				h.t.Errorf("login called with unexpected arguments")
			}
			var err error
			if n := h.logins - 1; n < len(h.loginResults) {
				err = h.loginResults[n]
			}
			if err != nil {
				return nil, err
			}
			if h.loginNil {
				return nil, nil
			}
			return validCreds(), nil
		},
		newSalt: func() (string, error) { return testSalt, nil },
		logger:  logger,
	})
}

// fingerprint is the fingerprint of the harness's current credentials.
func (h *harness) fingerprint() string {
	return loginvault.CredentialFingerprint(testSalt, h.cfg.SchoologyUsername, h.cfg.SchoologyPassword)
}

// latched sets a latch for the harness's current credentials.
func (h *harness) latched(reason string) {
	h.store.state = &loginState{
		Salt: testSalt, Blocked: true, BlockedReason: reason,
		BlockedAt: h.clock.Add(-time.Hour), CredentialFingerprint: h.fingerprint(),
	}
	h.store.stateVersion = 3
}

// noSecretsLogged fails the test if a credential or the token appears in
// the logs.
func (h *harness) noSecretsLogged() {
	h.t.Helper()
	for _, s := range []string{testUsername, testPassword, testToken} {
		if strings.Contains(h.logs.String(), s) {
			h.t.Errorf("a secret reached the log: %q", s)
		}
	}
}

func (h *harness) logHas(fragment string) bool { return strings.Contains(h.logs.String(), fragment) }

func TestRun_Success(t *testing.T) {
	h := newHarness(t)
	if got := h.run(); got != exitSuccess {
		t.Fatalf("exit code: got %d, want %d\n%s", got, exitSuccess, h.logs.String())
	}
	if h.store.puts != 1 {
		t.Fatalf("session writes: got %d, want 1", h.store.puts)
	}
	want := map[string]any{
		"host":       "school.schoology.com",
		"sess_id":    "SESS-abc",
		"csrf_token": "csrf-xyz",
		"csrf_key":   "csrf-key-1",
		"uid":        "12345",
	}
	if !reflect.DeepEqual(h.store.putData, want) {
		t.Errorf("vault data: got %#v, want %#v", h.store.putData, want)
	}
	st := h.store.state
	if st == nil || st.Blocked || st.UnconfirmedCount != 0 || !st.AttemptStartedAt.IsZero() ||
		st.CredentialFingerprint != h.fingerprint() || st.Salt != testSalt {
		t.Errorf("state after a success: %+v", st)
	}
	if h.store.closes != 1 {
		t.Errorf("store closes: got %d, want 1", h.store.closes)
	}
}

// The state is written before the login, with the fingerprint and the
// attempt marker: a run that cannot write it cannot record a rejection.
func TestRun_StateWrittenBeforeLogin(t *testing.T) {
	h := newHarness(t)
	h.run()
	if h.stateAtLogin == nil {
		t.Fatal("no state record existed when the login started")
	}
	if h.stateAtLogin.CredentialFingerprint != h.fingerprint() || !h.stateAtLogin.AttemptStartedAt.Equal(h.clock) {
		t.Errorf("state at login: %+v", h.stateAtLogin)
	}
}

func TestRun_StateUnwritable_NoLogin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"403", &vaultapi.ResponseError{StatusCode: 403, Errors: []string{"permission denied"}}, exitSecretWriteRejected},
		{"500", &vaultapi.ResponseError{StatusCode: 500, Errors: []string{"internal error"}}, exitTransient},
		{"cas", &vaultapi.ResponseError{StatusCode: 400, Errors: []string{"check-and-set parameter did not match the current version"}}, exitTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.writeStateErrs = []error{tc.err, tc.err, tc.err}
			if got := h.run(); got != tc.want {
				t.Fatalf("exit code: got %d, want %d", got, tc.want)
			}
			if h.logins != 0 || h.store.puts != 0 {
				t.Errorf("logins %d, session writes %d; want none", h.logins, h.store.puts)
			}
		})
	}
}

func TestRun_StateUnreadable_NoLogin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		// A policy that does not grant read on the state path: the
		// upgrade note's failure mode. Fail safe.
		{"403", &vaultapi.ResponseError{StatusCode: 403, Errors: []string{"permission denied"}}, exitSecretWriteRejected},
		{"503", &vaultapi.ResponseError{StatusCode: 503, Errors: []string{"sealed"}}, exitTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.readStateErr = tc.err
			if got := h.run(); got != tc.want {
				t.Fatalf("exit code: got %d, want %d", got, tc.want)
			}
			if h.logins != 0 || h.store.stateWrites != 0 {
				t.Errorf("logins %d, state writes %d; want none", h.logins, h.store.stateWrites)
			}
		})
	}
}

func TestRun_VaultConnectFails_NoLogin(t *testing.T) {
	h := newHarness(t)
	h.connectErr = errors.New("vault k8s login: connection refused")
	if got := h.run(); got != exitTransient {
		t.Fatalf("exit code: got %d, want %d", got, exitTransient)
	}
	if h.logins != 0 {
		t.Errorf("logins: got %d, want 0", h.logins)
	}
}

func TestRun_ConfigError_Missing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config)
	}{
		{"missing host", func(c *config) { c.SchoologyHost = "" }},
		{"missing username", func(c *config) { c.SchoologyUsername = "" }},
		{"missing password", func(c *config) { c.SchoologyPassword = "" }},
		{"missing vault addr", func(c *config) { c.VaultAddr = "" }},
		{"missing vault role", func(c *config) { c.VaultK8sRole = "" }},
		{"missing session path", func(c *config) { c.VaultSessionPath = "" }},
		{"missing state path", func(c *config) { c.VaultStatePath = "" }},
		{"state path is the session path", func(c *config) { c.VaultStatePath = "/" + c.VaultSessionPath + "/" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.mutate(&h.cfg)
			if got := h.run(); got != exitConfigError {
				t.Fatalf("exit code: got %d, want %d", got, exitConfigError)
			}
			if h.connects != 0 || h.logins != 0 || h.store.puts != 0 {
				t.Errorf("connects %d, logins %d, writes %d; want none", h.connects, h.logins, h.store.puts)
			}
		})
	}
}

// The bug: a login left on /login exited 3, and the next scheduled run
// logged in again. Now the first sets the latch and the second makes no
// login at all.
func TestRun_NotConfirmedOnForm_LatchesAndNextRunIsBlocked(t *testing.T) {
	for _, loginErr := range []error{
		auth.ErrBadCredentials,
		errors.Join(errors.New("login pipeline"), auth.ErrBadCredentials),
	} {
		t.Run(loginErr.Error(), func(t *testing.T) {
			h := newHarness(t)
			h.loginResults = []error{loginErr}
			if got := h.run(); got != exitBadCredentials {
				t.Fatalf("first run: exit %d, want %d\n%s", got, exitBadCredentials, h.logs.String())
			}
			st := h.store.state
			if st == nil || !st.Blocked || st.BlockedReason != blockedCredentialsRejected ||
				st.CredentialFingerprint != h.fingerprint() || !st.AttemptStartedAt.IsZero() || !st.BlockedAt.Equal(h.clock) {
				t.Fatalf("state after the first run: %+v", st)
			}
			if h.store.puts != 0 {
				t.Errorf("a session was written after an unconfirmed login")
			}
			// Says what is known, not more.
			if !h.logHas("not confirmed") || !h.logHas("possibly rejected") {
				t.Errorf("log does not say 'not confirmed / possibly rejected':\n%s", h.logs.String())
			}
			if h.logHas("invalid username or password") {
				t.Errorf("log asserts a wrong password:\n%s", h.logs.String())
			}

			h.clock = h.clock.Add(12 * time.Hour)
			if got := h.run(); got != exitLoginBlocked {
				t.Fatalf("second run: exit %d, want %d", got, exitLoginBlocked)
			}
			if h.logins != 1 {
				t.Errorf("logins over both runs: got %d, want 1", h.logins)
			}
			if !h.logHas(`"outcome":"login_blocked"`) {
				t.Errorf("no outcome=login_blocked line:\n%s", h.logs.String())
			}
			h.noSecretsLogged()
		})
	}
}

// A latch set before this run: no browser, no state write.
func TestRun_Latched_NoLoginNoWrite(t *testing.T) {
	for _, reason := range []string{blockedCredentialsRejected, blockedLoginUnconfirmed} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			h.latched(reason)
			if got := h.run(); got != exitLoginBlocked {
				t.Fatalf("exit code: got %d, want %d", got, exitLoginBlocked)
			}
			if h.logins != 0 || h.store.stateWrites != 0 || h.store.puts != 0 {
				t.Errorf("logins %d, state writes %d, session writes %d; want none",
					h.logins, h.store.stateWrites, h.store.puts)
			}
		})
	}
}

// The latch clears itself when the credentials change.
func TestRun_LatchClearsWhenCredentialsChange(t *testing.T) {
	h := newHarness(t)
	h.latched(blockedCredentialsRejected)
	h.store.state.UnconfirmedCount = 1
	h.cfg.SchoologyPassword = "a-new-password-11"
	if got := h.run(); got != exitSuccess {
		t.Fatalf("exit code: got %d, want %d\n%s", got, exitSuccess, h.logs.String())
	}
	if h.logins != 1 || h.store.puts != 1 {
		t.Errorf("logins %d, session writes %d; want 1 and 1", h.logins, h.store.puts)
	}
	st := h.store.state
	if st.Blocked || st.BlockedReason != "" || st.UnconfirmedCount != 0 || st.CredentialFingerprint != h.fingerprint() {
		t.Errorf("state after the credentials changed: %+v", st)
	}
}

// Clearing the latch by hand (deleting the record) lets the next run log
// in.
func TestRun_LatchClearedByHand(t *testing.T) {
	h := newHarness(t)
	h.latched(blockedCredentialsRejected)
	if got := h.run(); got != exitLoginBlocked {
		t.Fatalf("exit code: got %d, want %d", got, exitLoginBlocked)
	}
	h.store.state, h.store.stateVersion = nil, 0 // vault kv metadata delete
	if got := h.run(); got != exitSuccess {
		t.Fatalf("after clearing: exit %d, want %d", got, exitSuccess)
	}
	if h.logins != 1 {
		t.Errorf("logins: got %d, want 1", h.logins)
	}
}

// Failures after the submit (or that cannot be placed before it) count as
// unconfirmed; two in a row set the latch; the third run makes no login.
func TestRun_UnconfirmedTwiceLatches(t *testing.T) {
	ambiguous := []error{
		auth.ErrLoginTimeout,
		auth.ErrSSORequired,
		errors.New("auth: click submit: context canceled"),
		errors.New("auth: no SESS* cookie on authenticated page - login did not complete?"),
		errors.New("auth: extracted credentials are incomplete - login may not have finished"),
		errors.New("something nobody anticipated"),
		fmt.Errorf("wrapped: %w", errors.New("auth: connect remote browser: refused")), // not a known prefix: fail closed
	}
	for _, loginErr := range ambiguous {
		t.Run(loginErr.Error(), func(t *testing.T) {
			h := newHarness(t)
			h.loginResults = []error{loginErr, loginErr}
			if got := h.run(); got != exitLoginUnconfirmed {
				t.Fatalf("first run: exit %d, want %d\n%s", got, exitLoginUnconfirmed, h.logs.String())
			}
			if st := h.store.state; st.Blocked || st.UnconfirmedCount != 1 || !st.AttemptStartedAt.IsZero() {
				t.Fatalf("state after one unconfirmed login: %+v", st)
			}
			h.clock = h.clock.Add(12 * time.Hour)
			if got := h.run(); got != exitLoginUnconfirmed {
				t.Fatalf("second run: exit %d, want %d", got, exitLoginUnconfirmed)
			}
			if st := h.store.state; !st.Blocked || st.BlockedReason != blockedLoginUnconfirmed || st.UnconfirmedCount != 2 {
				t.Fatalf("state after two unconfirmed logins: %+v", st)
			}
			h.clock = h.clock.Add(12 * time.Hour)
			if got := h.run(); got != exitLoginBlocked {
				t.Fatalf("third run: exit %d, want %d", got, exitLoginBlocked)
			}
			if h.logins != 2 || h.store.puts != 0 {
				t.Errorf("logins %d, session writes %d; want 2 and 0", h.logins, h.store.puts)
			}
			h.noSecretsLogged()
		})
	}
}

// A login that returns neither credentials nor an error is unconfirmed.
func TestRun_NilCredentialsUnconfirmed(t *testing.T) {
	h := newHarness(t)
	h.loginNil = true
	if got := h.run(); got != exitLoginUnconfirmed {
		t.Fatalf("exit code: got %d, want %d", got, exitLoginUnconfirmed)
	}
	if h.store.puts != 0 || h.store.state.UnconfirmedCount != 1 {
		t.Errorf("session writes %d, state %+v", h.store.puts, h.store.state)
	}
}

// A success between two unconfirmed logins resets the count.
func TestRun_SuccessClearsUnconfirmedCount(t *testing.T) {
	h := newHarness(t)
	h.loginResults = []error{auth.ErrLoginTimeout, nil, auth.ErrLoginTimeout}
	for i, want := range []int{exitLoginUnconfirmed, exitSuccess, exitLoginUnconfirmed} {
		if got := h.run(); got != want {
			t.Fatalf("run %d: exit %d, want %d", i+1, got, want)
		}
		h.clock = h.clock.Add(12 * time.Hour)
	}
	if st := h.store.state; st.Blocked || st.UnconfirmedCount != 1 {
		t.Errorf("state: %+v; want unblocked with one unconfirmed login", st)
	}
}

// A Browserless outage, or a login page without the form, happens before
// the submit: it never counts towards the latch, however often.
func TestRun_FailureBeforeSubmitDoesNotCount(t *testing.T) {
	before := []error{
		fmt.Errorf("auth: connect remote browser %q: %w", "ws://browserless:3000?token="+testToken, errors.New("dial tcp: connection refused")),
		errors.New("auth: launch browser: exec: not found"),
		errors.New("auth: open login page: net::ERR_NAME_NOT_RESOLVED"),
		errors.New("auth: wait load: context deadline exceeded"),
		errors.New("auth: find mail field: element not found"),
		errors.New("auth: fill pass: node detached"),
		errors.New("auth: find submit: element not found"),
	}
	h := newHarness(t)
	h.loginResults = before
	for i := range before {
		if got := h.run(); got != exitTransient {
			t.Fatalf("run %d: exit %d, want %d\n%s", i+1, got, exitTransient, h.logs.String())
		}
		h.clock = h.clock.Add(time.Hour)
	}
	if st := h.store.state; st.Blocked || st.UnconfirmedCount != 0 || !st.AttemptStartedAt.IsZero() {
		t.Errorf("state after failures before the submit: %+v", st)
	}
	if h.logins != len(before) {
		t.Errorf("logins: got %d, want %d", h.logins, len(before))
	}
	h.noSecretsLogged()
}

// A run that died during its login left the attempt marker. Once stale it
// counts as an unconfirmed login; with an earlier one, it latches.
func TestRun_StaleAttemptMarker(t *testing.T) {
	t.Run("counts as unconfirmed", func(t *testing.T) {
		h := newHarness(t)
		h.store.state = &loginState{Salt: testSalt, CredentialFingerprint: h.fingerprint(),
			AttemptStartedAt: h.clock.Add(-12 * time.Hour)}
		if got := h.run(); got != exitSuccess {
			t.Fatalf("exit code: got %d, want %d", got, exitSuccess)
		}
		if h.logins != 1 || h.store.state.UnconfirmedCount != 0 {
			t.Errorf("logins %d, state %+v", h.logins, h.store.state)
		}
		if !h.logHas("never recorded its outcome") {
			t.Errorf("the stale marker was not reported:\n%s", h.logs.String())
		}
	})
	t.Run("second in a row latches", func(t *testing.T) {
		h := newHarness(t)
		h.store.state = &loginState{Salt: testSalt, CredentialFingerprint: h.fingerprint(), UnconfirmedCount: 1,
			AttemptStartedAt: h.clock.Add(-12 * time.Hour)}
		if got := h.run(); got != exitLoginBlocked {
			t.Fatalf("exit code: got %d, want %d", got, exitLoginBlocked)
		}
		if h.logins != 0 || !h.store.state.Blocked || h.store.state.BlockedReason != blockedLoginUnconfirmed {
			t.Errorf("logins %d, state %+v", h.logins, h.store.state)
		}
	})
	t.Run("fresh marker: another login in flight", func(t *testing.T) {
		h := newHarness(t)
		h.store.state = &loginState{Salt: testSalt, AttemptStartedAt: h.clock.Add(-time.Minute)}
		if got := h.run(); got != exitSuccess {
			t.Fatalf("exit code: got %d, want %d", got, exitSuccess)
		}
		if h.logins != 0 || h.store.stateWrites != 0 {
			t.Errorf("logins %d, state writes %d; want none", h.logins, h.store.stateWrites)
		}
	})
}

// A login that uses up the run's context still has its outcome recorded:
// the latch, the unconfirmed count, a success's session.
func TestRun_OutcomeRecordedAfterRunContextEnds(t *testing.T) {
	t.Run("latch", func(t *testing.T) {
		h := newHarness(t)
		h.cancelOnLogin = true
		h.loginResults = []error{auth.ErrBadCredentials}
		if got := h.run(); got != exitBadCredentials {
			t.Fatalf("exit code: got %d, want %d", got, exitBadCredentials)
		}
		if st := h.store.state; !st.Blocked || st.BlockedReason != blockedCredentialsRejected || !st.AttemptStartedAt.IsZero() {
			t.Errorf("state: %+v", st)
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		h := newHarness(t)
		h.cancelOnLogin = true
		h.loginResults = []error{auth.ErrLoginTimeout}
		if got := h.run(); got != exitLoginUnconfirmed {
			t.Fatalf("exit code: got %d, want %d", got, exitLoginUnconfirmed)
		}
		if st := h.store.state; st.UnconfirmedCount != 1 || !st.AttemptStartedAt.IsZero() {
			t.Errorf("state: %+v", st)
		}
	})
	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		h.cancelOnLogin = true
		if got := h.run(); got != exitSuccess {
			t.Fatalf("exit code: got %d, want %d\n%s", got, exitSuccess, h.logs.String())
		}
		if h.store.putVer != 1 || !h.store.state.AttemptStartedAt.IsZero() {
			t.Errorf("session version %d, state %+v", h.store.putVer, h.store.state)
		}
	})
}

// If the latch cannot be written after a login left on /login, say so
// loudly; the exit is still 3 (a FailJob code).
func TestRun_LatchWriteFails(t *testing.T) {
	h := newHarness(t)
	h.loginResults = []error{auth.ErrBadCredentials}
	h.store.failStateWritesFrom = 2
	if got := h.run(); got != exitBadCredentials {
		t.Fatalf("exit code: got %d, want %d", got, exitBadCredentials)
	}
	if !h.logHas("COULD NOT RECORD THE LOGIN BLOCK") {
		t.Errorf("no loud line:\n%s", h.logs.String())
	}
	// The pre-login marker is still there, so the next run counts it.
	if h.store.state == nil || h.store.state.AttemptStartedAt.IsZero() {
		t.Errorf("state: %+v", h.store.state)
	}
}

func TestRun_VaultWrite_Forbidden(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"permission denied", errors.New("error writing secret to secret/data/glovebox/schoology/test/session: Error making API request.\n\nCode: 403. Errors:\n\n* permission denied")},
		{"403 status only", errors.New("403 Forbidden response from vault")},
		{"400 bad request", errors.New("400 Bad Request: invalid path")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.putErr = tc.err
			if got := h.run(); got != exitSecretWriteRejected {
				t.Fatalf("exit code: got %d, want %d", got, exitSecretWriteRejected)
			}
		})
	}
}

func TestRun_VaultWrite_5xx(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"500 internal", errors.New("error writing secret: Code: 500. Internal Server Error")},
		{"network reset", errors.New("connection reset by peer")},
		{"timeout", errors.New("context deadline exceeded while contacting vault")},
		// Regression: an embedded "400" / "403" / "404" substring inside
		// an otherwise-5xx message must not route to exitSecretWriteRejected.
		{"502 with 400 in request id", errors.New("Code: 502. request id a-400-b")},
		{"503 with 403 in timestamp", errors.New("Code: 503. timestamp 2024-04-03T12:00:00Z")},
		{"504 with 404 in trace id", errors.New("Code: 504. trace id 404aaa-bbb")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.putErr = tc.err
			if got := h.run(); got != exitTransient {
				t.Fatalf("exit code: got %d, want %d", got, exitTransient)
			}
		})
	}
}

// TestRun_VaultWrite_ResponseError_Structural exercises the primary
// isClientError path: a *vaultapi.ResponseError with a 4xx status code.
func TestRun_VaultWrite_ResponseError_Structural(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantExit   int
	}{
		{"403 forbidden", 403, exitSecretWriteRejected},
		{"400 bad request", 400, exitSecretWriteRejected},
		{"404 not found", 404, exitSecretWriteRejected},
		{"429 too many requests", 429, exitSecretWriteRejected},
		{"500 internal server", 500, exitTransient},
		{"503 service unavailable", 503, exitTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.putErr = &vaultapi.ResponseError{
				HTTPMethod: "PUT",
				URL:        "https://vault.example/v1/secret/data/glovebox/schoology/test/session",
				StatusCode: tc.statusCode,
				Errors:     []string{"vault returned an error"},
			}
			if got := h.run(); got != tc.wantExit {
				t.Fatalf("exit code: got %d, want %d (status %d)", got, tc.wantExit, tc.statusCode)
			}
		})
	}
}

func TestLoadConfig_StatePath(t *testing.T) {
	env := map[string]string{
		"SCHOOLOGY_HOST":     "https://school.schoology.com/",
		"SCHOOLOGY_USERNAME": testUsername,
		"SCHOOLOGY_PASSWORD": testPassword,
		"VAULT_ADDR":         "https://vault:8200",
		"VAULT_K8S_ROLE":     "r",
		"VAULT_SESSION_PATH": "/glovebox/schoology/default/session",
	}
	getenv := func(k string) string { return env[k] }
	cfg := loadConfig(getenv)
	if cfg.VaultStatePath != "glovebox/schoology/default/login-state" || cfg.VaultKVMount != "secret" {
		t.Errorf("defaults: state %q mount %q", cfg.VaultStatePath, cfg.VaultKVMount)
	}
	if p := cfg.problems(); len(p) != 0 {
		t.Errorf("problems: %v", p)
	}
	env["VAULT_STATE_PATH"] = "glovebox/schoology/default/session/"
	if p := loadConfig(getenv).problems(); len(p) != 1 || !strings.Contains(p[0], "VAULT_STATE_PATH") {
		t.Errorf("state path equal to the session path: problems %v", p)
	}
	env["VAULT_STATE_PATH"] = "glovebox/schoology/default/elsewhere"
	if got := loadConfig(getenv).VaultStatePath; got != "glovebox/schoology/default/elsewhere" {
		t.Errorf("explicit state path: %q", got)
	}
}

func TestRedact(t *testing.T) {
	cfg := validConfig()
	got := cfg.redact("user " + testUsername + " pass " + testPassword + " url ws://b?token=" + testToken)
	for _, s := range []string{testUsername, testPassword, testToken} {
		if strings.Contains(got, s) {
			t.Errorf("redact left %q in %q", s, got)
		}
	}
	if long := cfg.redact(strings.Repeat("x", 1000)); len(long) > 450 {
		t.Errorf("redact did not bound the length: %d", len(long))
	}
}

func TestBrowserlessURL(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"unset", "", "", ""},
		{"url only", "ws://browserless:3000", "", "ws://browserless:3000"},
		{"url and token", "ws://browserless:3000", "secret", "ws://browserless:3000?token=secret"},
		{"url with query and token", "ws://browserless:3000?launch=1", "secret", "ws://browserless:3000?launch=1&token=secret"},
		// A token with no URL is meaningless; BROWSERLESS_URL gates everything.
		{"token only", "", "secret", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BROWSERLESS_URL", tc.url)
			t.Setenv("BROWSERLESS_TOKEN", tc.token)
			if got := browserlessURL(); got != tc.want {
				t.Errorf("browserlessURL(): got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCredentialsToMap(t *testing.T) {
	t.Parallel()
	got := credentialsToMap(validCreds())
	want := map[string]any{
		"host":       "school.schoology.com",
		"sess_id":    "SESS-abc",
		"csrf_token": "csrf-xyz",
		"csrf_key":   "csrf-key-1",
		"uid":        "12345",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("credentialsToMap: got %#v, want %#v", got, want)
	}
}
