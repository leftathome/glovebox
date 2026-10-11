package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
)

// Exit codes. Stable: cluster-side alerting may branch on them. 0 through 4
// mean what they mean for schoology-auth-refresher (spec 06 section 12.4).
const (
	// exitSuccess covers every run that did what it should, including the
	// two that deliberately do nothing: a run inside the blackout window
	// and a run that found the stored session still good.
	exitSuccess = 0
	// exitTransient: something outside the operator's control failed
	// (browser, network, Vault unavailable). The next scheduled run is the
	// retry; nothing re-runs this one.
	exitTransient = 1
	// exitConfigError: a required setting is missing or malformed.
	exitConfigError = 2
	// exitBadCredentials: the portal refused the username/password.
	exitBadCredentials = 3
	// exitVaultRejected: Vault refused a read or write (policy or path).
	exitVaultRejected = 4
	// exitLoginUnconfirmed: the browser login returned, but the session it
	// produced did not open the signed-in home page. Nothing was written.
	exitLoginUnconfirmed = 5
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
	// writeAttempts bounds the Vault write after a login. Retrying it is
	// safe (it is not a login) and worth it: a session that is not stored
	// was bought with the previous session and is then thrown away.
	writeAttempts = 3
	writeDelay    = 5 * time.Second
)

// loginFailure says what a failed login means for a second attempt.
type loginFailure int

const (
	// loginFailedUnknown: anything not positively identified as one of the
	// other two. The credentials may have been submitted, so no retry.
	loginFailedUnknown loginFailure = iota
	// loginFailedBeforeSubmit: the browser did not start, or the sign-in
	// page could not be reached. Nothing was typed into a form. This is the
	// only kind that allows the single retry.
	loginFailedBeforeSubmit
	// loginRejected: the portal answered the submitted form with its
	// invalid-credentials notice.
	loginRejected
)

// loginError is what a loginFunc returns on failure.
type loginError struct {
	kind loginFailure
	err  error
}

func (e *loginError) Error() string { return e.err.Error() }
func (e *loginError) Unwrap() error { return e.err }

// failureKind extracts the loginFailure from err. An error that is not a
// *loginError is loginFailedUnknown, the kind that never retries.
func failureKind(err error) loginFailure {
	var le *loginError
	if errors.As(err, &le) {
		return le.kind
	}
	return loginFailedUnknown
}

// loginFunc performs exactly one browser login and returns the session it
// produced. It is the only code path that submits the credentials.
type loginFunc func(ctx context.Context) (*pslib.SessionExport, error)

// probeFunc reads the signed-in home page with a session, through a
// transport that can only GET, and returns the id of the student the page
// marks as selected ("" when it marks none). An error for which
// pslib.IsSessionExpired is true means the portal does not accept the
// session; any other error means the question was not answered.
type probeFunc func(ctx context.Context, session *pslib.SessionExport) (selectedStudentID string, err error)

// storedSession is the session currently in Vault.
type storedSession struct {
	// Document is the session JSON, exactly what the connector is given.
	Document string
	// LoggedInAt is when that session was created: the logged_in_at field
	// this binary writes, else the creation time of the KV version. Zero
	// when neither is known.
	LoggedInAt time.Time
}

// sessionStore is the Vault side: one KV v2 path.
type sessionStore interface {
	// Read returns the stored session, or nil when there is none.
	Read(ctx context.Context) (*storedSession, error)
	// CanWrite reports whether this token may write the session path.
	CanWrite(ctx context.Context) (bool, error)
	// Write stores data and returns the new KV version.
	Write(ctx context.Context, data map[string]any) (version int, err error)
}

// deps are the seams run needs. main wires the real ones; tests wire fakes
// and therefore never start a browser, never reach a portal and never need
// a Vault.
type deps struct {
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	connect func(ctx context.Context) (sessionStore, error)
	login   loginFunc
	probe   probeFunc
	logger  *slog.Logger
}

// run is one execution of the job. In order:
//
//  1. Refuse, exit 0, when the clock is inside the blackout window. Nothing
//     is contacted.
//  2. Authenticate to Vault and read the session stored there.
//  3. If that session is younger than MinSessionAge and still opens the
//     signed-in home page, exit 0 without logging in: a login would end a
//     session the connector is using, for nothing.
//  4. Check the Vault token can write the session path, so a login is never
//     spent on a session that cannot be stored.
//  5. Log in: once, or twice when the first attempt failed before the
//     credentials were submitted. The blackout is checked again before each.
//  6. Confirm the new session by reading the home page (GET only).
//  7. Write it to Vault.
func run(ctx context.Context, cfg config, d deps) int {
	log := d.logger

	if cfg.Blackout.contains(d.now()) {
		logBlackoutRefusal(log, cfg, d.now(), "the run started inside the blackout window")
		return exitSuccess
	}

	store, err := d.connect(ctx)
	if err != nil {
		log.Error("vault authentication failed",
			"outcome", "transient_failure",
			"vault_role", cfg.VaultK8sRole,
			"error", cfg.redact(err.Error()))
		return exitTransient
	}

	stored, err := store.Read(ctx)
	if err != nil {
		return vaultFailure(log, cfg, "read", err)
	}

	switch keepStored(ctx, cfg, d, stored) {
	case keepYes:
		return exitSuccess
	case keepUndetermined:
		return exitTransient
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
		return exitVaultRejected
	}

	session, code := runLogin(ctx, cfg, d)
	if session == nil {
		return code
	}

	loggedInAt := d.now().UTC()
	session = normalizeSession(session, loggedInAt.Add(cfg.SessionLifetime))
	if session == nil {
		log.Error("the login returned no usable cookies; nothing written",
			"outcome", "login_unconfirmed")
		return exitLoginUnconfirmed
	}

	selected, ok := confirmSession(ctx, d, session)
	if !ok {
		return exitLoginUnconfirmed
	}

	data, err := sessionData(session, loggedInAt, selected)
	if err != nil {
		log.Error("could not encode the session; nothing written", "outcome", "transient_failure")
		return exitTransient
	}

	version, code := writeSession(ctx, cfg, d, store, data)
	if code != exitSuccess {
		return code
	}

	log.Info("refresh complete",
		"outcome", "success",
		"vault_path", cfg.VaultSessionPath,
		"version", version,
		"cookies", len(session.Cookies),
		"expires_at", session.ExpiresAt.UTC().Format(time.RFC3339))
	return exitSuccess
}

// logBlackoutRefusal writes the one line an operator looks for when the job
// "did nothing" in the afternoon.
func logBlackoutRefusal(log *slog.Logger, cfg config, now time.Time, why string) {
	log.Warn("refusing to log in: "+why+
		" (a login now would sign the account's owner out of the portal)",
		"outcome", "blackout_refused",
		"blackout_window", cfg.Blackout.String(),
		"blackout_timezone", cfg.Blackout.loc.String(),
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
// the exit code.
//
// The portal locks an account after repeated bad submissions, so the rule
// is fixed here and not left to a retry policy: at most maxLoginAttempts
// logins, and the second happens only when the first is known to have
// failed before any credentials were submitted. A rejected login, and any
// failure that cannot be placed before the submit, ends the run.
func runLogin(ctx context.Context, cfg config, d deps) (*pslib.SessionExport, int) {
	log := d.logger
	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		// Checked again here: Vault and the home-page read above take time,
		// and so does the pause before a second attempt.
		if now := d.now(); cfg.Blackout.contains(now) {
			logBlackoutRefusal(log, cfg, now, "the blackout window began before the login could start")
			if attempt == 1 {
				return nil, exitSuccess
			}
			return nil, exitTransient
		}

		log.Info("logging in to the portal", "attempt", attempt)
		session, err := d.login(ctx)
		if err == nil {
			if session == nil {
				log.Error("the login returned no session", "outcome", "login_unconfirmed")
				return nil, exitLoginUnconfirmed
			}
			return session, exitSuccess
		}

		switch failureKind(err) {
		case loginRejected:
			// Never retried: repeated bad submissions lock the account.
			log.Error("the portal rejected the credentials; correct them in Vault before the next run",
				"outcome", "bad_credentials",
				"attempt", attempt)
			return nil, exitBadCredentials
		case loginFailedBeforeSubmit:
			if attempt < maxLoginAttempts {
				log.Warn("the login failed before the credentials were submitted; trying once more",
					"attempt", attempt,
					"error", cfg.redact(err.Error()))
				if serr := d.sleep(ctx, loginRetryDelay); serr != nil {
					log.Error("interrupted before the second login attempt", "outcome", "transient_failure")
					return nil, exitTransient
				}
				continue
			}
			log.Error("the login failed before the credentials were submitted, twice",
				"outcome", "transient_failure",
				"attempt", attempt,
				"error", cfg.redact(err.Error()))
			return nil, exitTransient
		default:
			log.Error("the login failed and the credentials may have been submitted; not retrying",
				"outcome", "transient_failure",
				"attempt", attempt,
				"error", cfg.redact(err.Error()))
			return nil, exitTransient
		}
	}
	// Unreachable: every path through the loop body returns or continues
	// with attempt < maxLoginAttempts.
	return nil, exitTransient
}

// normalizeSession drops cookies the connector would drop and sets the
// session's expiry. It returns nil when no cookie is left.
//
// The expiry the library reports is not evidence of the portal's session
// lifetime: it is the latest expiry among the browser's cookies (which can
// be a year out for a preference cookie) or, when every cookie is a
// session cookie, "four hours from now". The connector refuses a session
// past its expires_at, so a four-hour guess would stop the afternoon and
// evening polls on a session the portal still honours. This binary writes
// login time plus SessionLifetime instead: an upper bound on how long the
// connector will present the session. The portal says when it really ends,
// by refusing it, and the connector handles that.
func normalizeSession(s *pslib.SessionExport, expiresAt time.Time) *pslib.SessionExport {
	out := &pslib.SessionExport{ExpiresAt: expiresAt.UTC()}
	for _, c := range s.Cookies {
		if c != nil && c.Name != "" && c.Value != "" {
			out.Cookies = append(out.Cookies, c)
		}
	}
	if len(out.Cookies) == 0 {
		return nil
	}
	return out
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

// sessionData shapes the Vault secret.
//
//	session              the session JSON, as one string. This is the
//	                     property ESO copies into the connector's
//	                     session.json; the connector parses it with
//	                     powerschool.ParseSession.
//	logged_in_at         when this binary logged in (RFC 3339, UTC). Read
//	                     back by the next run to compute the session's age.
//	expires_at           the same instant as in the session JSON.
//	selected_student_id  the student the portal had selected after login,
//	                     "" when the page marked none. Kept in Vault only:
//	                     never logged, and not part of what ESO syncs.
func sessionData(session *pslib.SessionExport, loggedInAt time.Time, selected string) (map[string]any, error) {
	doc, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"session":             string(doc),
		"logged_in_at":        loggedInAt.UTC().Format(time.RFC3339),
		"expires_at":          session.ExpiresAt.UTC().Format(time.RFC3339),
		"selected_student_id": selected,
	}, nil
}

// writeSession stores the session, retrying a transient failure. A refusal
// (4xx) is not retried: the policy or the path is wrong.
func writeSession(ctx context.Context, cfg config, d deps, store sessionStore, data map[string]any) (int, int) {
	log := d.logger
	var err error
	for attempt := 1; attempt <= writeAttempts; attempt++ {
		var version int
		version, err = store.Write(ctx, data)
		if err == nil {
			return version, exitSuccess
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
	return 0, vaultFailure(log, cfg, "write", err)
}

// vaultFailure logs a failed Vault read or write and picks the exit code.
func vaultFailure(log *slog.Logger, cfg config, op string, err error) int {
	if isClientError(err) {
		log.Error("vault rejected the "+op,
			"outcome", "secret_"+op+"_failed",
			"vault_path", cfg.VaultSessionPath,
			"error", cfg.redact(err.Error()))
		return exitVaultRejected
	}
	log.Error("vault "+op+" failed",
		"outcome", "transient_failure",
		"vault_path", cfg.VaultSessionPath,
		"error", cfg.redact(err.Error()))
	return exitTransient
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
