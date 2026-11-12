package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/loginvault"
	"github.com/leftathome/schoology-go/auth"
)

// Exit codes per spec 06 section 12.4. Stable contract: cluster-side
// alerting may branch on them. They mean what they mean for
// powerschool-auth-refresher where the two share a number.
const (
	exitSuccess = 0
	// exitTransient: something outside the operator's control failed
	// before the credentials were submitted (browser, network, Vault). The
	// Job may retry it; a retry is safe, since no login was made.
	exitTransient = 1
	// exitConfigError: a required setting is missing or malformed.
	exitConfigError = 2
	// exitBadCredentials: the login was NOT CONFIRMED -- after the submit
	// the page stayed on Schoology's /login form (schoology-go's
	// ErrBadCredentials: three polls, about 1.5s). The password was
	// possibly rejected; this is not proof of it. The latch is set; later
	// runs exit exitLoginBlocked.
	exitBadCredentials = 3
	// exitSecretWriteRejected: Vault refused a read or a write (policy or
	// path).
	exitSecretWriteRejected = 4
	// exitLoginUnconfirmed: the credentials were (or may have been)
	// submitted, and no session came back: a timeout, a redirect off the
	// tenant host, a page without a session. After maxUnconfirmedLogins
	// in a row the latch is set.
	exitLoginUnconfirmed = 5
	// exitLoginBlocked: the latch is set for these credentials. No browser
	// was started and nothing was sent to Schoology. An operator must
	// change the credentials or clear the latch (docs/AUTH-RECOVERY.md).
	exitLoginBlocked = 6
)

const (
	// maxUnconfirmedLogins is how many logins in a row may end without a
	// session before the latch stops further submissions. The same limit
	// as powerschool-auth-refresher.
	maxUnconfirmedLogins = 2
	// writeAttempts bounds a write of the login state. Retrying it is safe
	// (it is not a login).
	writeAttempts = 3
	writeDelay    = 2 * time.Second
	// outcomeWriteTimeout bounds the write that records a login's outcome.
	// It runs on a context of its own: the run's may have been used up by
	// the login, and an outcome that is not recorded is a login the next
	// run repeats.
	outcomeWriteTimeout = 30 * time.Second
)

// Reasons the latch is set (loginState.BlockedReason). The same strings as
// powerschool-auth-refresher.
const (
	blockedCredentialsRejected = "credentials_rejected"
	blockedLoginUnconfirmed    = "login_unconfirmed"
)

// loginFunc is the test seam over auth.LoginWithPassword: exactly one
// browser login. It is the only code path that submits the credentials.
// Tests replace it with a stub so they never start a browser.
type loginFunc func(ctx context.Context, host, username, password string) (*auth.Credentials, error)

// sessionStore is the Vault side: the session path (write-only) and the
// login state path.
type sessionStore interface {
	// Put stores the session without check-and-set and returns the new
	// version.
	Put(ctx context.Context, data map[string]any) (version int, err error)
	// ReadState returns the login state; a zero state when none is stored.
	ReadState(ctx context.Context) (*loginState, error)
	// WriteState stores the state, check-and-set against state.Version.
	WriteState(ctx context.Context, state *loginState) error
	// Close revokes the Vault token (best effort).
	Close()
}

// deps are the seams run needs. main wires the real ones; tests wire fakes
// and therefore never start a browser, never reach Schoology and never
// need a Vault.
type deps struct {
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	connect func(ctx context.Context) (sessionStore, error)
	login   loginFunc
	// newSalt returns a fresh random salt for the credential fingerprint.
	newSalt func() (string, error)
	logger  *slog.Logger
}

// run is one execution of the job. It returns the exit code.
//
// In order:
//
//  1. Validate the configuration (exit 2).
//  2. Authenticate to Vault and read the login state. If the latch is set
//     for these credentials, exit 6: no browser, nothing sent to
//     Schoology. A latch set for credentials that have since changed is
//     cleared.
//  3. Write the state record, with the credentials' fingerprint and an
//     attempt marker. This proves the latch can be written: if it cannot,
//     a rejected login could not be recorded either, so there is no login.
//  4. Log in, once.
//  5. Record the outcome. A login left on the /login form sets the latch
//     (exit 3). A login that submitted the credentials and produced no
//     session counts as unconfirmed (exit 5); two in a row set the latch.
//     A failure before the submit counts for nothing (exit 1).
//  6. On success, clear the unconfirmed count and write the session.
func run(ctx context.Context, cfg config, d deps) int {
	log := d.logger

	if problems := cfg.problems(); len(problems) > 0 {
		log.Error("config invalid",
			"outcome", "config_error",
			"missing", problems,
		)
		return exitConfigError
	}

	store, err := d.connect(ctx)
	if err != nil {
		log.Error("vault authentication failed",
			"outcome", "transient_failure",
			"vault_addr", cfg.VaultAddr,
			"vault_role", cfg.VaultK8sRole,
			"error", cfg.redact(err.Error()),
		)
		return exitTransient
	}
	defer store.Close()

	state, err := store.ReadState(ctx)
	if err != nil {
		// Without the record there is no telling whether a latch is set:
		// no login.
		return vaultFailure(log, cfg, "read", cfg.VaultStatePath, err)
	}
	if state == nil {
		state = &loginState{}
	}
	if state.Salt == "" {
		salt, err := d.newSalt()
		if err != nil || salt == "" {
			log.Error("could not generate a salt for the credential fingerprint", "outcome", "transient_failure")
			return exitTransient
		}
		state.Salt = salt
	}
	fingerprint := loginvault.CredentialFingerprint(state.Salt, cfg.SchoologyUsername, cfg.SchoologyPassword)

	// A fresh attempt marker: another run (a Job created by hand beside a
	// scheduled one) is logging in right now. Do not log in beside it.
	if now := d.now(); state.AttemptInProgress(now) {
		log.Warn("another login for this account is in flight (another run of this job); not logging in beside it",
			"outcome", "login_in_progress",
			"attempt_started_at", state.AttemptStartedAt.UTC().Format(time.RFC3339))
		return exitSuccess
	}
	if !state.AttemptStartedAt.IsZero() {
		// A marker too old to be a login in flight: the process that wrote
		// it ended during a login whose outcome nobody recorded (the pod
		// was killed at its deadline, say). It counts as a login made then
		// without a session.
		log.Warn("an earlier login attempt never recorded its outcome (the process ended during it); counting it as an unconfirmed login",
			"attempt_started_at", state.AttemptStartedAt.UTC().Format(time.RFC3339))
		state.AttemptStartedAt = time.Time{}
		state.UnconfirmedCount++
		if state.UnconfirmedCount >= maxUnconfirmedLogins && !state.Blocked {
			state.Blocked, state.BlockedReason, state.BlockedAt = true, blockedLoginUnconfirmed, d.now().UTC()
			state.CredentialFingerprint = fingerprint
		}
		if err := writeState(ctx, d, store, state); err != nil {
			return vaultFailure(log, cfg, "write", cfg.VaultStatePath, err)
		}
	}

	if state.Blocked {
		if state.CredentialFingerprint == fingerprint {
			log.Error("login blocked: an earlier login with these credentials was not confirmed (the password was possibly rejected), "+
				"and submitting them again risks locking the account. Change the credentials, or clear the latch "+
				"(docs/AUTH-RECOVERY.md); not logging in, no browser started",
				"outcome", "login_blocked",
				"blocked_reason", state.BlockedReason,
				"blocked_at", state.BlockedAt.UTC().Format(time.RFC3339),
				"vault_path", cfg.VaultStatePath)
			return exitLoginBlocked
		}
		log.Warn("the login was blocked, but the credentials have changed since; the block no longer applies",
			"blocked_reason", state.BlockedReason)
		state.Blocked, state.BlockedReason, state.BlockedAt, state.UnconfirmedCount = false, "", time.Time{}, 0
	}

	// Written BEFORE the login, not only after a failure: if this write is
	// refused, a rejected login could not be recorded either, and the next
	// run would submit the same credentials again. The attempt marker is
	// what lets the next run count a login whose outcome this run never
	// got to record.
	state.CredentialFingerprint = fingerprint
	state.AttemptStartedAt = d.now().UTC()
	if err := writeState(ctx, d, store, state); err != nil {
		if isCASMismatch(err) {
			log.Error("the login state changed while this run was deciding (another run wrote it); NO LOGIN WAS MADE by this run",
				"outcome", "login_state_conflict", "vault_path", cfg.VaultStatePath)
			return exitTransient
		}
		log.Error("could not write the login state; not logging in, since a rejected login could not be recorded",
			"vault_path", cfg.VaultStatePath)
		return vaultFailure(log, cfg, "write", cfg.VaultStatePath, err)
	}

	log.Info("logging in to schoology", "host", cfg.SchoologyHost)
	creds, loginErr := d.login(ctx, cfg.SchoologyHost, cfg.SchoologyUsername, cfg.SchoologyPassword)

	// The outcome is recorded on a context of its own: the login may have
	// used up the run's.
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	state.AttemptStartedAt = time.Time{}

	if loginErr == nil && creds == nil {
		loginErr = errors.New("the login returned no session and no error")
	}
	if loginErr != nil {
		switch classifyLoginError(loginErr) {
		case loginNotConfirmedOnForm:
			// Never retried. Not proof of a wrong password: schoology-go
			// reports this when the URL still contains /login three polls
			// (about 1.5s) after the submit.
			log.Error("schoology login not confirmed: the page was still the /login form after the credentials were submitted "+
				"(password possibly rejected; a slow page looks the same). Latching: no further login with these credentials "+
				"until they change or the latch is cleared",
				"outcome", "bad_credentials",
				"host", cfg.SchoologyHost,
				// Not the library's error text ("invalid username or
				// password"), which claims more than it knows.
				"detail", "still on /login after the submit (schoology-go ErrBadCredentials)",
			)
			setLatch(octx, cfg, d, store, state, blockedCredentialsRejected)
			return exitBadCredentials
		case loginFailedBeforeSubmit:
			log.Error("schoology login failed before the credentials were submitted",
				"outcome", "transient_failure",
				"host", cfg.SchoologyHost,
				"error", cfg.redact(loginErr.Error()),
			)
			if err := writeState(octx, d, store, state); err != nil {
				log.Error("could not clear the attempt marker; the next run will count this run as an unconfirmed login",
					"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
			}
			return exitTransient
		default:
			return unconfirmed(octx, cfg, d, store, state, loginErr)
		}
	}

	// Only a confirmed login clears the count. This write also clears the
	// attempt marker.
	state.UnconfirmedCount = 0
	if err := writeState(octx, d, store, state); err != nil {
		log.Warn("could not record the confirmed login (the unconfirmed-login count, the attempt marker)",
			"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
	}

	// Shape the 5-field credentials map. Hand-built to keep the wire
	// format obvious.
	version, err := store.Put(octx, credentialsToMap(creds))
	if err != nil {
		if isClientError(err) {
			log.Error("vault rejected write",
				"outcome", "secret_write_failed",
				"vault_path", cfg.VaultSessionPath,
				"error", cfg.redact(err.Error()),
			)
			return exitSecretWriteRejected
		}
		log.Error("vault write failed",
			"outcome", "transient_failure",
			"vault_path", cfg.VaultSessionPath,
			"error", cfg.redact(err.Error()),
		)
		return exitTransient
	}

	log.Info("refresh complete",
		"outcome", "success",
		"vault_path", cfg.VaultSessionPath,
		"version", version,
	)
	return exitSuccess
}

type loginFailure int

const (
	// loginSubmittedNoSession: the credentials were, or may have been,
	// submitted and no session came back. The default: anything not known
	// to have happened before the submit is counted.
	loginSubmittedNoSession loginFailure = iota
	// loginNotConfirmedOnForm: schoology-go's ErrBadCredentials.
	loginNotConfirmedOnForm
	// loginFailedBeforeSubmit: the browser, the login page or its form
	// failed before the submit button was clicked.
	loginFailedBeforeSubmit
)

// beforeSubmitPrefixes are the error messages schoology-go v0.3.0's
// LoginWithPassword returns before it clicks the submit button
// (auth/login.go): the browser connection, the login page, and finding or
// filling the form. Everything after, and anything not listed here --
// "click submit" (the click may have landed), ErrLoginTimeout,
// ErrSSORequired, a page without a session cookie or settings, an error
// this list does not know -- counts as a submitted login. Fail closed:
// a library upgrade that rewords these only makes the latch stricter.
var beforeSubmitPrefixes = []string{
	"auth: username and password are required",
	"auth: connect remote browser",
	"auth: launch browser",
	"auth: connect browser",
	"auth: open page",
	"auth: open login page",
	"auth: wait load",
	"auth: find mail field",
	"auth: fill mail",
	"auth: find pass field",
	"auth: fill pass",
	"auth: find submit",
}

// classifyLoginError places a login error before or after the submit.
func classifyLoginError(err error) loginFailure {
	if errors.Is(err, auth.ErrBadCredentials) {
		return loginNotConfirmedOnForm
	}
	if errors.Is(err, auth.ErrLoginTimeout) || errors.Is(err, auth.ErrSSORequired) {
		return loginSubmittedNoSession
	}
	msg := err.Error()
	for _, p := range beforeSubmitPrefixes {
		if strings.HasPrefix(msg, p) {
			return loginFailedBeforeSubmit
		}
	}
	return loginSubmittedNoSession
}

// unconfirmed records a login that submitted the credentials and ended
// without a session, and returns the exit code. After maxUnconfirmedLogins
// in a row the latch is set: a login page that now wants a CAPTCHA, a
// second factor or a new password looks like this, and each further login
// would submit the same credentials again.
func unconfirmed(ctx context.Context, cfg config, d deps, store sessionStore, state *loginState, loginErr error) int {
	state.UnconfirmedCount++
	attrs := []any{
		"outcome", "login_unconfirmed",
		"host", cfg.SchoologyHost,
		"consecutive_unconfirmed", state.UnconfirmedCount,
		"error", cfg.redact(loginErr.Error()),
	}
	if state.UnconfirmedCount >= maxUnconfirmedLogins {
		d.logger.Error("schoology login not confirmed: the credentials were submitted and no session came back, again; "+
			"no further login will be attempted until the credentials change or the latch is cleared", attrs...)
		setLatch(ctx, cfg, d, store, state, blockedLoginUnconfirmed)
		return exitLoginUnconfirmed
	}
	d.logger.Error("schoology login not confirmed: the credentials were submitted and no session came back", attrs...)
	if err := writeState(ctx, d, store, state); err != nil {
		d.logger.Error("could not record the unconfirmed login",
			"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
	}
	return exitLoginUnconfirmed
}

// setLatch blocks further logins with the current credentials.
func setLatch(ctx context.Context, cfg config, d deps, store sessionStore, state *loginState, reason string) {
	state.Blocked = true
	state.BlockedReason = reason
	state.BlockedAt = d.now().UTC()
	if err := writeState(ctx, d, store, state); err != nil {
		// The pre-login write succeeded moments ago, so this is rare. Say
		// so loudly: without the record the next run logs in again (it
		// will at least find the stale attempt marker and count it).
		d.logger.Error("COULD NOT RECORD THE LOGIN BLOCK: the next run may submit the same credentials again. Suspend the CronJob",
			"blocked_reason", reason,
			"vault_path", cfg.VaultStatePath,
			"error", cfg.redact(err.Error()))
		return
	}
	d.logger.Warn("login blocked until the credentials change or the latch is cleared",
		"blocked_reason", reason, "vault_path", cfg.VaultStatePath)
}

// writeState stores the login state exactly once, retrying a transient
// failure (see loginvault.WriteStateOnce).
func writeState(ctx context.Context, d deps, store sessionStore, state *loginState) error {
	return loginvault.WriteStateOnce(ctx, store, state, writeAttempts, func(ctx context.Context) error {
		return d.sleep(ctx, writeDelay)
	})
}

// vaultFailure logs a failed Vault read or write and picks the exit code.
func vaultFailure(log *slog.Logger, cfg config, op, path string, err error) int {
	if isClientError(err) {
		log.Error("vault rejected the "+op,
			"outcome", "secret_"+op+"_failed",
			"vault_path", path,
			"error", cfg.redact(err.Error()))
		return exitSecretWriteRejected
	}
	log.Error("vault "+op+" failed",
		"outcome", "transient_failure",
		"vault_path", path,
		"error", cfg.redact(err.Error()))
	return exitTransient
}

// redact removes the account's username and password and the Browserless
// token from text bound for a log line, and bounds its length. Error text
// from the browser driver is assembled elsewhere (schoology-go quotes the
// Browserless URL, token included, when it cannot connect); this is the
// last check before it leaves the process.
func (c config) redact(msg string) string {
	for _, secret := range []string{c.SchoologyPassword, c.SchoologyUsername, c.BrowserlessToken} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	const max = 400
	if len(msg) > max {
		msg = msg[:max] + fmt.Sprintf("... (%d bytes)", len(msg))
	}
	return msg
}
