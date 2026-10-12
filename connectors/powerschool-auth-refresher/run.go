package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/loginvault"
	"github.com/leftathome/glovebox/internal/pslogin"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// Exit codes. Stable: cluster-side alerting may branch on them. 0 through 4
// mean what they mean for schoology-auth-refresher (spec 06 section 12.4).
const (
	// exitSuccess covers every run that did what it should, including the
	// two that deliberately do nothing: a run inside the blackout window
	// (so the CronJob is not marked failed for obeying its rule) and a run
	// that found the stored session still good.
	exitSuccess = 0
	// exitTransient: something outside the operator's control failed
	// (browser, network, Vault unavailable). The next scheduled run is the
	// retry; nothing re-runs this one.
	exitTransient = 1
	// exitConfigError: a required setting is missing or malformed.
	exitConfigError = 2
	// exitBadCredentials: the portal refused the username/password. The
	// latch is set; later runs exit exitLoginBlocked.
	exitBadCredentials = 3
	// exitVaultRejected: Vault refused a read or write (policy or path).
	exitVaultRejected = 4
	// exitLoginUnconfirmed: the browser login returned, but the session it
	// produced did not open the signed-in home page. Nothing was written.
	exitLoginUnconfirmed = 5
	// exitLoginBlocked: the latch is set. No browser was started. An
	// operator must change the credentials or clear the latch.
	exitLoginBlocked = 6
	// exitSignInUnrecognised: the portal's sign-in page is not one this
	// binary will type a password into. No browser was started.
	exitSignInUnrecognised = 7
	// exitLoginStateConflict: the login state record was changed by
	// another workload between this run reading it and writing its attempt
	// marker. NO LOGIN WAS MADE: for a scheduled run, this window has no
	// new session unless whoever changed the record made one.
	//
	// Non-zero on purpose, so that the Job shows as failed. That is safe
	// only because a failed login Job is never run again by Kubernetes:
	// the chart renders backoffLimit 0 and restartPolicy Never for every
	// login CronJob (scripts/test-chart-powerschool-login.sh pins both),
	// and a Job made by hand from one inherits them. If that ever changes,
	// this must go back to 0: a re-run would be a second login.
	//
	// "Another login is in flight" (outcome login_in_progress) stays 0:
	// there a session is being made, by the other workload.
	exitLoginStateConflict = 8
)

const (
	// maxLoginAttempts is the hard ceiling on browser logins per run: one
	// attempt, plus one more only when the first failed before the
	// credentials were submitted. See runLogin.
	maxLoginAttempts = 2
	// loginRetryDelay separates the two attempts.
	loginRetryDelay = 10 * time.Second
	// verifyAttempts bounds the read-only home-page checks after a login.
	verifyAttempts = 3
	verifyDelay    = 5 * time.Second
	// writeAttempts bounds a Vault write after a login. Retrying it is safe
	// (it is not a login) and worth it: a session that is not stored was
	// bought with the previous session and is then thrown away.
	writeAttempts = 3
	writeDelay    = 5 * time.Second
	// maxUnconfirmedLogins is how many logins in a row may return without a
	// working session before the latch stops further submissions.
	maxUnconfirmedLogins = 2
)

// Reasons the latch is set (loginState.BlockedReason).
const (
	blockedCredentialsRejected = "credentials_rejected"
	blockedLoginUnconfirmed    = "login_unconfirmed"
)

// loginFunc performs exactly one browser login and returns the session it
// produced. It is the only code path that submits the credentials.
type loginFunc func(ctx context.Context) (*pslib.SessionExport, error)

// probeFunc reads the signed-in home page with a session, through a
// transport that can only GET, and returns the id of the student the page
// marks as selected ("" when it marks none). An error for which
// pslib.IsSessionExpired is true means the portal does not accept the
// session; any other error means the question was not answered.
type probeFunc func(ctx context.Context, session *pslib.SessionExport) (selectedStudentID string, err error)

// sessionStore is the Vault side: the session path and the state path.
type sessionStore interface {
	// Read returns the stored session (nil when there is none) and the
	// current KV version of the session path (0 when it does not exist).
	Read(ctx context.Context) (stored *storedSession, version int, err error)
	// CanWrite reports whether this token may write the session path.
	CanWrite(ctx context.Context) (bool, error)
	// Write stores data only if the path is still at version cas
	// (check-and-set), and returns the new version.
	Write(ctx context.Context, data map[string]any, cas int) (version int, err error)
	// ReadState returns the login state; a zero state when none is stored.
	ReadState(ctx context.Context) (*loginState, error)
	WriteState(ctx context.Context, state *loginState) error
	// Close revokes the Vault token.
	Close()
}

// deps are the seams run needs. main wires the real ones; tests wire fakes
// and therefore never start a browser, never reach a portal and never need
// a Vault.
type deps struct {
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	connect func(ctx context.Context) (sessionStore, error)
	// checkSignIn fetches the sign-in page (GET only) and returns an error
	// wrapping errSignInUnrecognised when it is not the expected form.
	checkSignIn func(ctx context.Context) error
	login       loginFunc
	probe       probeFunc
	// readRequest returns the connector's login request field ("" when it
	// is empty). Only called in request mode. It talks to the Kubernetes
	// API, never to the portal.
	readRequest func(ctx context.Context) (string, error)
	// newSalt returns a fresh random salt for the credential fingerprint.
	newSalt func() (string, error)
	// terminate records the one-line reason the container ended
	// (/dev/termination-log), shown by kubectl describe.
	terminate func(message string)
	logger    *slog.Logger
}

// result is how a run ended.
type result struct {
	code    int
	outcome string
	// message is the termination message: one line, no secrets.
	message string
}

// run is one execution of the job; it returns the exit code, having
// written the termination message.
//
// In order:
//
//  1. Refuse, exit 0, when a login started now could still be running
//     inside the blackout. Nothing is contacted.
//  2. Authenticate to Vault and read the login state. If the latch is set
//     for these credentials, refuse (exit 6). No browser, no portal.
//  3. If the stored session is younger than MinSessionAge and still opens
//     the signed-in home page, exit 0 without logging in.
//  4. Check the Vault token can write the session path, and fetch the
//     sign-in page (GET) to see that it is the portal's own form.
//  5. Write the state record (this proves the latch can be written), then
//     log in: once, or twice when the first attempt failed before the
//     credentials were submitted. The blackout is checked before each.
//  6. Confirm the new session by reading the home page (GET only). A login
//     the portal rejected sets the latch; so do two unconfirmed logins in
//     a row.
//  7. Write the session to Vault, check-and-set against the version read
//     in step 3.
func run(ctx context.Context, cfg config, d deps) int {
	r := execute(ctx, cfg, d)
	if d.terminate != nil {
		d.terminate(fmt.Sprintf("powerschool-auth-refresher: outcome=%s exit=%d: %s", r.outcome, r.code, r.message))
	}
	return r.code
}

func execute(ctx context.Context, cfg config, d deps) result {
	log := d.logger

	if now := d.now(); cfg.Blackout.blocksLoginAt(now) {
		logBlackoutRefusal(log, cfg, now)
		return blackoutResult(cfg, exitSuccess)
	}

	// Request mode: this run exists only to answer the connector. Its
	// first step -- before Vault, and before anything is sent to the
	// portal -- is to see whether the connector has asked. If it has not,
	// the run ends here. A run without a pending request makes no request
	// to the portal at all: not a login, not a check of the stored
	// session. Checking a session nobody is waiting for would be the
	// keepalive the account's owner has refused.
	var request *pendingRequest
	if cfg.requestMode() {
		pending, res := readPendingRequest(ctx, cfg, d)
		if pending == nil {
			return res
		}
		request = pending
	}

	store, err := d.connect(ctx)
	if err != nil {
		log.Error("vault authentication failed",
			"outcome", "transient_failure",
			"vault_role", cfg.VaultK8sRole,
			"error", cfg.redact(err.Error()))
		return result{exitTransient, "transient_failure", "Vault authentication failed"}
	}
	defer store.Close()

	state, err := store.ReadState(ctx)
	if err != nil {
		return vaultFailure(log, cfg, "read", cfg.VaultStatePath, err)
	}
	if state == nil {
		state = &loginState{}
	}
	if state.Salt == "" {
		salt, err := d.newSalt()
		if err != nil || salt == "" {
			log.Error("could not generate a salt for the credential fingerprint", "outcome", "transient_failure")
			return result{exitTransient, "transient_failure", "no random salt available"}
		}
		state.Salt = salt
	}
	fingerprint := credentialFingerprint(state.Salt, cfg.Username, cfg.Password)

	// The record is shared with the login broker, when one is deployed for
	// this account, and with any other run of this job. A fresh attempt
	// marker means one of them is logging in right now: this run must not
	// log in beside it, and must not judge it either.
	if now := d.now(); state.AttemptInProgress(now) {
		log.Warn("another login for this account is in flight (the login broker, or another run of this job); not logging in beside it",
			"outcome", "login_in_progress",
			"attempt_started_at", state.AttemptStartedAt.UTC().Format(time.RFC3339))
		return result{exitSuccess, "login_in_progress",
			"another login for this account started at " + state.AttemptStartedAt.UTC().Format(time.RFC3339) + " and has not recorded its outcome yet"}
	}
	if !state.AttemptStartedAt.IsZero() {
		// A marker too old to be a login in flight: the process that wrote
		// it died in the middle of a login whose outcome nobody knows. It
		// counts as a login made then, without a working session.
		at := state.AttemptStartedAt
		log.Warn("an earlier login attempt never recorded its outcome (the process ended during it); counting it as an unconfirmed login",
			"attempt_started_at", at.UTC().Format(time.RFC3339))
		state.AttemptStartedAt = time.Time{}
		state.RequestLoginAt = at
		state.CountLogin(loginvault.DayOf(at, cfg.Blackout.loc))
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
			log.Error("login blocked: an earlier login with these credentials did not produce a session, and submitting them again risks locking the account. "+
				"Change the credentials in Vault, or clear the latch (see the runbook); not logging in",
				"outcome", "login_blocked",
				"blocked_reason", state.BlockedReason,
				"blocked_at", state.BlockedAt.UTC().Format(time.RFC3339),
				"vault_path", cfg.VaultStatePath)
			return result{exitLoginBlocked, "login_blocked",
				"login blocked (" + state.BlockedReason + ") since " + state.BlockedAt.UTC().Format(time.RFC3339) +
					"; change the credentials or clear the latch at " + cfg.VaultStatePath}
		}
		log.Warn("the login was blocked, but the credentials have changed since; the block no longer applies",
			"blocked_reason", state.BlockedReason)
		state.Blocked, state.BlockedReason, state.BlockedAt, state.UnconfirmedCount = false, "", time.Time{}, 0
	}

	if request != nil {
		// Each request is acted on once, and logins made for requests keep
		// a minimum distance from each other, whatever the connector
		// writes. Both are decided here, from this workload's own record
		// in Vault, which the connector cannot touch.
		if state.ServedRequest == request.raw {
			return noLoginRequested(log, "the pending request has already been acted on")
		}
		if !state.RequestLoginAt.IsZero() {
			since := d.now().Sub(state.RequestLoginAt)
			if since < cfg.RequestMinInterval {
				log.Warn("a login was made for a request too recently; this request waits",
					"outcome", "login_request_too_soon",
					"last_request_login_seconds_ago", int64(since/time.Second),
					"min_interval_seconds", int64(cfg.RequestMinInterval/time.Second))
				return result{exitSuccess, "login_request_too_soon",
					fmt.Sprintf("a login was made for a request %s ago; the minimum interval is %s",
						since.Round(time.Second), cfg.RequestMinInterval)}
			}
		}
	}

	stored, version, err := store.Read(ctx)
	if err != nil {
		return vaultFailure(log, cfg, "read", cfg.VaultSessionPath, err)
	}

	if request != nil && stored != nil && !stored.LoggedInAt.IsZero() && !stored.LoggedInAt.Before(request.At) {
		// A login newer than the request is already in Vault (a manual
		// run, or the answer to this very request on its way to the
		// connector). Nothing to do, and nothing to ask the portal.
		return noLoginRequested(log, "a login newer than the pending request has already been made")
	}

	switch keepStoredFor(ctx, cfg, d, stored, request) {
	case keepYes:
		if request != nil {
			// Answered without a login. Record it, so the next run does
			// not ask the portal about this same request again.
			state.ServedRequest = request.raw
			if err := writeState(ctx, d, store, state); err != nil {
				log.Warn("could not record that the request was answered; the next run will check the session again",
					"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
			}
		}
		return result{exitSuccess, "session_still_good", "the stored session is young and still accepted; no login"}
	case keepUndetermined:
		return result{exitTransient, "transient_failure", "could not tell whether the stored session is still valid; no login"}
	}

	if ok, err := store.CanWrite(ctx); err != nil {
		// Not being able to ask is not evidence of a missing grant (a role
		// without the default policy cannot ask). Say so and go on.
		log.Warn("could not check write access to the session path before logging in",
			"vault_path", cfg.VaultSessionPath,
			"error", cfg.redact(err.Error()))
	} else if !ok {
		log.Error("the Vault token cannot write the session path; not logging in",
			"outcome", "secret_write_failed",
			"vault_path", cfg.VaultSessionPath,
			"vault_role", cfg.VaultK8sRole)
		return result{exitVaultRejected, "secret_write_failed", "the Vault token cannot write " + cfg.VaultSessionPath}
	}

	// Is the page the browser is about to type the password into the
	// portal's own sign-in form? Not retried within the run.
	if err := d.checkSignIn(ctx); err != nil {
		if errors.Is(err, errSignInUnrecognised) {
			log.Error("the portal's sign-in page is not the expected form on the configured host; not starting the browser",
				append([]any{"outcome", "signin_page_unrecognised", "reason", signInReason(err)}, powerschool.ErrorAttrs(err)...)...)
			return result{exitSignInUnrecognised, "signin_page_unrecognised", "sign-in page not recognised: " + signInReason(err)}
		}
		log.Error("could not fetch the portal's sign-in page; not starting the browser",
			append([]any{"outcome", "transient_failure"}, powerschool.ErrorAttrs(err)...)...)
		return result{exitTransient, "transient_failure", "the sign-in page could not be fetched"}
	}

	// Written BEFORE the login, not only after a rejection: if this write
	// is refused, a rejection could not be recorded either, and the next
	// run would submit the same credentials again.
	state.CredentialFingerprint = fingerprint
	if request != nil {
		// Also before the login: whatever comes of it, this request has
		// had its one login, and the interval to the next starts now.
		state.ServedRequest = request.raw
		state.RequestLoginAt = d.now().UTC()
	}
	// The attempt marker: from this write until the outcome is recorded,
	// nobody else sharing the record logs in (they see a login in flight).
	// The write is check-and-set against the version read above, so if
	// anybody changed the record since -- the broker starting a login of
	// its own, most of all -- it is refused and this run does not log in.
	attemptAt := d.now().UTC()
	state.AttemptStartedAt = attemptAt
	if err := writeState(ctx, d, store, state); err != nil {
		if isCASMismatch(err) {
			log.Error("the login state changed while this run was deciding (another login workload wrote it); NO LOGIN WAS MADE by this run",
				"outcome", "login_state_conflict", "vault_path", cfg.VaultStatePath)
			return result{exitLoginStateConflict, "login_state_conflict",
				"the login state at " + cfg.VaultStatePath + " changed during this run; no login was made"}
		}
		log.Error("could not write the login state; not logging in, since a rejected login could not be recorded")
		return vaultFailure(log, cfg, "write", cfg.VaultStatePath, err)
	}

	session, r, notSubmitted := runLogin(ctx, cfg, d)
	// Every write of the record from here on clears the marker, and every
	// way out of this function below makes one.
	state.AttemptStartedAt = time.Time{}
	if !notSubmitted {
		// The credentials were submitted, or may have been: a login, for
		// the daily count the login broker budgets against.
		state.CountLogin(loginvault.DayOf(attemptAt, cfg.Blackout.loc))
	}
	if session == nil {
		switch r.code {
		case exitBadCredentials:
			setLatch(ctx, cfg, d, store, state, blockedCredentialsRejected)
		case exitLoginUnconfirmed:
			return unconfirmed(ctx, cfg, d, store, state, r.message)
		default:
			if err := writeState(ctx, d, store, state); err != nil {
				log.Error("could not clear the attempt marker; other login workloads will hold off until it is stale, then count this run as an unconfirmed login",
					"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
			}
		}
		return r
	}

	loggedInAt := d.now().UTC()
	session = normalizeSession(session, loggedInAt.Add(cfg.SessionLifetime))
	if session == nil {
		log.Error("the login returned no usable cookies; nothing written", "outcome", "login_unconfirmed")
		return unconfirmed(ctx, cfg, d, store, state, "the login returned no usable cookies")
	}

	selected, ok := confirmSession(ctx, d, session)
	if !ok {
		return unconfirmed(ctx, cfg, d, store, state, "the session the login produced does not open the home page")
	}

	// Only a confirmed login clears the count. This write also clears the
	// attempt marker and records the login in the daily count.
	state.UnconfirmedCount = 0
	if err := writeState(ctx, d, store, state); err != nil {
		log.Warn("could not record the confirmed login (the unconfirmed-login count, the attempt marker)",
			"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
	}

	data, err := sessionData(session, loggedInAt, selected)
	if err != nil {
		log.Error("could not encode the session; nothing written", "outcome", "transient_failure")
		return result{exitTransient, "transient_failure", "could not encode the session"}
	}

	newVersion, r := writeSession(ctx, cfg, d, store, session, data, version)
	if r.code != exitSuccess || r.outcome != "" {
		return r
	}

	log.Info("refresh complete",
		"outcome", "success",
		"vault_path", cfg.VaultSessionPath,
		"version", newVersion,
		"cookies", len(session.Cookies),
		"expires_at", session.ExpiresAt.UTC().Format(time.RFC3339))
	return result{exitSuccess, "success", fmt.Sprintf("logged in and stored session version %d", newVersion)}
}

// credentialFingerprint identifies a username/password pair without
// revealing either.
func credentialFingerprint(salt, username, password string) string {
	return pslogin.CredentialFingerprint(salt, username, password)
}

// blackoutResult is the result of every blackout refusal.
func blackoutResult(cfg config, code int) result {
	return result{code, "blackout_refused",
		"refused to log in: inside (or within " + loginHeadroom.String() + " of) the blackout " +
			cfg.Blackout.String() + " " + blackoutZone}
}

// logBlackoutRefusal writes the one line an operator looks for when the job
// "did nothing" in the afternoon.
func logBlackoutRefusal(log *slog.Logger, cfg config, now time.Time) {
	log.Warn("refusing to log in: a login started now could still be in progress inside the blackout window "+
		"(a login then would sign the account's owner out of the portal)",
		"outcome", "blackout_refused",
		"blackout_window", cfg.Blackout.String(),
		"blackout_timezone", cfg.Blackout.loc.String(),
		"login_headroom_seconds", int64(loginHeadroom/time.Second),
		"local_time", now.In(cfg.Blackout.loc).Format("2006-01-02 15:04:05 MST"))
}

type keepDecision int

const (
	// keepNo: log in.
	keepNo keepDecision = iota
	// keepYes: the stored session is young and the portal still accepts it.
	keepYes
	// keepUndetermined: the stored session is young and the portal could
	// not be asked. Logging in on a guess could end a good session, so the
	// run stops.
	keepUndetermined
)

// pendingRequest is a login request that is present, well-formed and
// fresh.
type pendingRequest struct {
	psportal.LoginRequest
	// raw is the field exactly as read; it is what "served" is recorded
	// and compared as.
	raw string
}

// requestClockSkew is how far in the future a request's time may be and
// still count. Beyond it the request is not believed.
const requestClockSkew = 2 * time.Minute

// noLoginRequested ends a request-mode run that has nothing to do.
func noLoginRequested(log *slog.Logger, why string) result {
	log.Info("no login requested: "+why+"; nothing sent to the portal", "outcome", "no_login_requested")
	return result{exitSuccess, "no_login_requested", why}
}

// readPendingRequest reads the connector's request. It returns nil and the
// result to end the run with when there is nothing to act on: no request,
// one that is not exactly a request, one from the future, or one older
// than RequestMaxAge (the poll that made it has long stopped waiting; a
// login now would be for nobody).
//
// The field is written by the connector, which parses untrusted pages. It
// is treated as data of that kind: validated for size and shape
// (psportal.ParseLoginRequest), never echoed into a log line, and able to
// cause exactly one thing -- this workload going on to its ordinary flow.
func readPendingRequest(ctx context.Context, cfg config, d deps) (*pendingRequest, result) {
	log := d.logger
	raw, err := d.readRequest(ctx)
	if err != nil {
		log.Error("could not read the login request; not logging in",
			"outcome", "transient_failure",
			"configmap", cfg.RequestConfigMap,
			"error", cfg.redact(err.Error()))
		return nil, result{exitTransient, "transient_failure", "the login request could not be read"}
	}
	parsed, err := psportal.ParseLoginRequest(raw)
	if errors.Is(err, psportal.ErrNoLoginRequest) {
		return nil, noLoginRequested(log, "the request field is empty")
	}
	if err != nil {
		log.Warn("the login request field does not hold a request; ignoring it", "problem", err.Error())
		return nil, noLoginRequested(log, "the request field is malformed")
	}
	now := d.now()
	switch age := now.Sub(parsed.At); {
	case age < -requestClockSkew:
		log.Warn("the login request is dated in the future; ignoring it",
			"seconds_ahead", int64(-age/time.Second))
		return nil, noLoginRequested(log, "the request is dated in the future")
	case age > cfg.RequestMaxAge:
		return nil, noLoginRequested(log, fmt.Sprintf("the last request is %s old; nobody is waiting for it", age.Round(time.Second)))
	}
	return &pendingRequest{LoginRequest: parsed, raw: raw}, result{}
}

// keepStoredFor is keepStored, with one shortcut in request mode: when the
// stored session is the very one the connector reported dead, there is
// nothing to ask the portal -- the connector has just asked.
func keepStoredFor(ctx context.Context, cfg config, d deps, stored *storedSession, request *pendingRequest) keepDecision {
	if request != nil && stored != nil && request.Fingerprint != psportal.NoSessionFingerprint {
		if session, err := powerschool.ParseSession([]byte(stored.Document), d.now()); err == nil &&
			psportal.RequestFingerprint(powerschool.SessionFingerprint(session)) == request.Fingerprint {
			d.logger.Info("the stored session is the one the connector found dead; logging in")
			return keepNo
		}
	}
	return keepStored(ctx, cfg, d, stored)
}

// keepStored decides whether the session already in Vault makes a login
// unnecessary. Only a session that is BOTH younger than MinSessionAge and
// accepted by the portal is kept.
func keepStored(ctx context.Context, cfg config, d deps, stored *storedSession) keepDecision {
	log := d.logger
	now := d.now()

	if stored == nil {
		log.Info("no session is stored yet; logging in")
		return keepNo
	}
	session, err := powerschool.ParseSession([]byte(stored.Document), now)
	if err != nil {
		// ParseSession never quotes the document.
		log.Info("the stored session is unusable; logging in", "reason", err.Error())
		return keepNo
	}
	if stored.LoggedInAt.IsZero() {
		log.Info("the stored session's age is unknown; logging in")
		return keepNo
	}
	age := now.Sub(stored.LoggedInAt)
	if age < 0 {
		age = 0
	}
	ageAttrs := []any{
		"session_age_seconds", int64(age / time.Second),
		"min_session_age_seconds", int64(cfg.MinSessionAge / time.Second),
	}
	if age >= cfg.MinSessionAge {
		log.Info("the stored session is old enough to replace; logging in", ageAttrs...)
		return keepNo
	}

	_, err = d.probe(ctx, session)
	switch {
	case err == nil:
		log.Info("session still good: it is younger than the minimum age and the portal still accepts it; not logging in",
			append([]any{"outcome", "session_still_good"}, ageAttrs...)...)
		return keepYes
	case pslib.IsSessionExpired(err):
		log.Info("the stored session is young but the portal no longer accepts it; logging in", ageAttrs...)
		return keepNo
	default:
		log.Error("could not tell whether the stored session is still valid; not logging in, since a login would end it if it is",
			append(append([]any{"outcome", "transient_failure"}, ageAttrs...), powerschool.ErrorAttrs(err)...)...)
		return keepUndetermined
	}
}

// runLogin performs the browser login. It returns the session, or nil and
// how the run ends.
//
// The portal locks an account after repeated bad submissions, so the rule
// is fixed here and not left to a retry policy: at most maxLoginAttempts
// logins, and the second happens only when the first is known to have
// failed before any credentials were submitted. A rejected login, and any
// failure that cannot be placed before the submit, ends the run.
func runLogin(ctx context.Context, cfg config, d deps) (session *pslib.SessionExport, r result, notSubmitted bool) {
	log := d.logger
	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		// Checked again here, with the same headroom: Vault and the reads
		// above take time, and so does the pause before a second attempt.
		if now := d.now(); cfg.Blackout.blocksLoginAt(now) {
			logBlackoutRefusal(log, cfg, now)
			if attempt == 1 {
				return nil, blackoutResult(cfg, exitSuccess), true
			}
			return nil, blackoutResult(cfg, exitTransient), true
		}

		log.Info("logging in to the portal", "attempt", attempt)
		session, err := d.login(ctx)
		if err == nil {
			if session == nil {
				log.Error("the login returned no session", "outcome", "login_unconfirmed")
				return nil, result{exitLoginUnconfirmed, "login_unconfirmed", "the login returned no session"}, false
			}
			return session, result{}, false
		}

		switch failureKind(err) {
		case loginRejected:
			// Never retried: repeated bad submissions lock the account.
			log.Error("the portal rejected the credentials; no further login will be attempted until they change or the latch is cleared",
				"outcome", "bad_credentials",
				"attempt", attempt)
			return nil, result{exitBadCredentials, "bad_credentials", "the portal rejected the credentials; the login is now blocked"}, false
		case loginFailedBeforeSubmit:
			if attempt < maxLoginAttempts {
				log.Warn("the login failed before the credentials were submitted; trying once more",
					"attempt", attempt,
					"error", cfg.redact(err.Error()))
				if serr := d.sleep(ctx, loginRetryDelay); serr != nil {
					log.Error("interrupted before the second login attempt", "outcome", "transient_failure")
					return nil, result{exitTransient, "transient_failure", "interrupted before the second login attempt"}, true
				}
				continue
			}
			log.Error("the login failed before the credentials were submitted, twice",
				"outcome", "transient_failure",
				"attempt", attempt,
				"error", cfg.redact(err.Error()))
			return nil, result{exitTransient, "transient_failure", "the login failed before the credentials were submitted, twice"}, true
		default:
			log.Error("the login failed and the credentials may have been submitted; not retrying",
				"outcome", "transient_failure",
				"attempt", attempt,
				"error", cfg.redact(err.Error()))
			return nil, result{exitTransient, "transient_failure", "the login failed; the credentials may have been submitted"}, false
		}
	}
	// Unreachable: every path through the loop body returns or continues
	// with attempt < maxLoginAttempts.
	return nil, result{exitTransient, "transient_failure", "no login attempt completed"}, false
}

// unconfirmed records a login that returned without a working session and
// ends the run. The session is never written. After maxUnconfirmedLogins in
// a row the latch is set: a sign-in page that now wants a CAPTCHA, a second
// factor or a new password looks like this, and each further login would
// end the connector's session again without ever replacing it.
func unconfirmed(ctx context.Context, cfg config, d deps, store sessionStore, state *loginState, message string) result {
	state.UnconfirmedCount++
	if state.UnconfirmedCount >= maxUnconfirmedLogins {
		d.logger.Error("logins keep returning without a working session; no further login will be attempted until the credentials change or the latch is cleared",
			"outcome", "login_unconfirmed",
			"consecutive_unconfirmed", state.UnconfirmedCount)
		setLatch(ctx, cfg, d, store, state, blockedLoginUnconfirmed)
		return result{exitLoginUnconfirmed, "login_unconfirmed",
			fmt.Sprintf("%s (%d in a row); the login is now blocked", message, state.UnconfirmedCount)}
	}
	if err := writeState(ctx, d, store, state); err != nil {
		d.logger.Error("could not record the unconfirmed login",
			"vault_path", cfg.VaultStatePath, "error", cfg.redact(err.Error()))
	}
	return result{exitLoginUnconfirmed, "login_unconfirmed",
		fmt.Sprintf("%s (%d in a row)", message, state.UnconfirmedCount)}
}

// setLatch blocks further logins with the current credentials.
func setLatch(ctx context.Context, cfg config, d deps, store sessionStore, state *loginState, reason string) {
	state.Blocked = true
	state.BlockedReason = reason
	state.BlockedAt = d.now().UTC()
	if err := writeState(ctx, d, store, state); err != nil {
		// The pre-login write succeeded moments ago, so this is rare. Say
		// so loudly: without the record the next run logs in again.
		d.logger.Error("COULD NOT RECORD THE LOGIN BLOCK: the next run will submit the same credentials again. Suspend the CronJobs",
			"blocked_reason", reason,
			"vault_path", cfg.VaultStatePath,
			"error", cfg.redact(err.Error()))
		return
	}
	d.logger.Warn("login blocked until the credentials change or the latch is cleared",
		"blocked_reason", reason, "vault_path", cfg.VaultStatePath)
}

// writeState stores the login state, retrying a transient failure.
func writeState(ctx context.Context, d deps, store sessionStore, state *loginState) error {
	// Exactly once: see loginvault.WriteStateOnce. A write Vault applied
	// and then answered with an error is recognised as this run's own.
	return loginvault.WriteStateOnce(ctx, store, state, writeAttempts, func(ctx context.Context) error {
		return d.sleep(ctx, writeDelay)
	})
}

// normalizeSession drops cookies the connector would drop and sets the
// session's expiry (login time plus SessionLifetime; see
// pslogin.NormalizeSession for why not the library's). It returns nil when
// no cookie is left.
func normalizeSession(s *pslib.SessionExport, expiresAt time.Time) *pslib.SessionExport {
	return pslogin.NormalizeSession(s, expiresAt)
}

// confirmSession reads the home page with the new session. Reads only: the
// probe's transport refuses every method but GET and HEAD.
func confirmSession(ctx context.Context, d deps, session *pslib.SessionExport) (selected string, ok bool) {
	log := d.logger
	var err error
	for attempt := 1; attempt <= verifyAttempts; attempt++ {
		selected, err = d.probe(ctx, session)
		if err == nil {
			return selected, true
		}
		if pslib.IsSessionExpired(err) {
			log.Error("the portal does not accept the session the login produced; nothing written",
				append([]any{"outcome", "login_unconfirmed"}, powerschool.ErrorAttrs(err)...)...)
			return "", false
		}
		if attempt < verifyAttempts {
			if serr := d.sleep(ctx, verifyDelay); serr != nil {
				break
			}
		}
	}
	log.Error("could not confirm the new session by reading the home page; nothing written",
		append([]any{"outcome", "login_unconfirmed"}, powerschool.ErrorAttrs(err)...)...)
	return "", false
}

// sessionData shapes the Vault secret: the session JSON as one string
// (what ESO copies into the connector's session.json), logged_in_at,
// expires_at and selected_student_id. See pslogin.SessionData.
func sessionData(session *pslib.SessionExport, loggedInAt time.Time, selected string) (map[string]any, error) {
	return pslogin.SessionData(session, loggedInAt, selected)
}

// writeSession stores the session, check-and-set against the version read
// before the login. A transient failure is retried; a refusal (4xx) is not.
//
// A check-and-set mismatch means another run (a manual Job beside a
// scheduled one) wrote a session while this one was logging in. Only one of
// the two sessions is alive -- the portal keeps the later login -- so this
// run asks the portal about its own once more: if it is still accepted it
// is the live one and replaces the other; if not, the other run's stays.
func writeSession(ctx context.Context, cfg config, d deps, store sessionStore,
	session *pslib.SessionExport, data map[string]any, cas int) (int, result) {
	log := d.logger
	var err error
	rechecked := false
	for attempt := 1; attempt <= writeAttempts; attempt++ {
		var version int
		version, err = store.Write(ctx, data, cas)
		if err == nil {
			return version, result{}
		}
		if isCASMismatch(err) && !rechecked {
			rechecked = true
			log.Warn("the stored session changed while this run was logging in (another run?); checking which session is alive")
			_, current, rerr := store.Read(ctx)
			if rerr != nil {
				return 0, vaultFailure(log, cfg, "read", cfg.VaultSessionPath, rerr)
			}
			if _, perr := d.probe(ctx, session); perr != nil {
				log.Warn("another run stored a session while this one was logging in, and the portal no longer accepts this run's; not stored",
					append([]any{"outcome", "superseded"}, powerschool.ErrorAttrs(perr)...)...)
				return 0, result{exitTransient, "superseded", "another run's newer login replaced this one; nothing stored"}
			}
			cas = current
			attempt--
			continue
		}
		if isClientError(err) {
			break
		}
		if attempt < writeAttempts {
			if serr := d.sleep(ctx, writeDelay); serr != nil {
				break
			}
		}
	}
	return 0, vaultFailure(log, cfg, "write", cfg.VaultSessionPath, err)
}

// vaultFailure logs a failed Vault read or write and picks the exit code.
func vaultFailure(log *slog.Logger, cfg config, op, path string, err error) result {
	if isClientError(err) {
		log.Error("vault rejected the "+op,
			"outcome", "secret_"+op+"_failed",
			"vault_path", path,
			"error", cfg.redact(err.Error()))
		return result{exitVaultRejected, "secret_" + op + "_failed", "Vault rejected the " + op + " of " + path}
	}
	log.Error("vault "+op+" failed",
		"outcome", "transient_failure",
		"vault_path", path,
		"error", cfg.redact(err.Error()))
	return result{exitTransient, "transient_failure", "Vault " + op + " of " + path + " failed"}
}

// redact removes the account's username and password from text bound for a
// log line, and bounds its length. Error text from a browser driver or an
// HTTP client is assembled elsewhere; this is the last check before it
// leaves the process.
func (c config) redact(msg string) string {
	for _, secret := range []string{c.Password, c.Username} {
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
