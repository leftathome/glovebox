// Package pslogin is how a workload logs in to the PowerSchool parent
// portal and looks at the result: the one browser login, the plain-GET
// check of the sign-in page that precedes it, the read-only look at the
// home page that follows it, and the shape of the session written to Vault.
//
// Two binaries use it and must not drift apart: the scheduled login job
// (connectors/powerschool-auth-refresher) and the login broker
// (connectors/login-broker), for which Driver in this package implements
// loginbroker.Driver.
//
// What the package guarantees towards the portal:
//
//   - Login is the only code path that submits the credentials, and one
//     call submits them at most once.
//   - Every other request goes through the connector's own transport,
//     pinned to the portal host over https and refusing every method but
//     GET and HEAD.
//   - Nothing here logs. Errors carry classified kinds and reasons from
//     closed lists; text assembled by the browser driver is kept inside the
//     error for the caller to redact, never interpreted beyond a fixed set
//     of markers.
package pslogin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/loginvault"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// LoginTimeout bounds one browser login, browser start included. The
// library's own default is 60s; a cold container needs some room. It is
// also part of the blackout headroom (psportal.LoginHeadroom).
const LoginTimeout = psportal.LoginTimeout

// ProbeTimeout bounds one home-page read and one sign-in page check.
const ProbeTimeout = 30 * time.Second

// Config is where the portal is and who logs in.
type Config struct {
	// BaseURL ("https://host") and Host ("host[:port]"), both from
	// powerschool.NormalizeBaseURL so that every binary agrees about where
	// the portal is.
	BaseURL string
	Host    string
	// Username and Password are the account's. Only Login reads them.
	Username string
	Password string
	// ChromeNoSandbox starts Chromium with --no-sandbox (the images set it;
	// see their Dockerfiles).
	ChromeNoSandbox bool
}

// Failure says what a failed login means for a second attempt.
type Failure int

const (
	// FailedUnknown: anything not positively identified as one of the
	// other two. The credentials may have been submitted, so no retry.
	FailedUnknown Failure = iota
	// FailedBeforeSubmit: the browser did not start, or the sign-in page
	// could not be reached. Nothing was typed into a form. This is the
	// only kind that allows a retry.
	FailedBeforeSubmit
	// Rejected: the portal answered the submitted form with its
	// invalid-credentials notice.
	Rejected
)

// LoginError is what Login returns on failure.
type LoginError struct {
	Kind Failure
	Err  error
}

func (e *LoginError) Error() string { return e.Err.Error() }
func (e *LoginError) Unwrap() error { return e.Err }

// KindOf extracts the Failure from err. An error that is not a *LoginError
// is FailedUnknown, the kind that never retries.
func KindOf(err error) Failure {
	var le *LoginError
	if errors.As(err, &le) {
		return le.Kind
	}
	return FailedUnknown
}

// Login returns the production login function: one powerschool-go browser
// login (a local headless Chromium driven through chromedp; the library has
// no remote-browser option, which is why the images that run it bundle a
// browser where schoology-auth-refresher uses Browserless).
//
// The library's debug logging is never enabled: it prints the sign-in
// form's HTML.
func Login(cfg Config) func(ctx context.Context) (*pslib.SessionExport, error) {
	return func(ctx context.Context) (*pslib.SessionExport, error) {
		client, err := pslib.NewClient(cfg.BaseURL,
			pslib.WithCredentials(cfg.Username, cfg.Password),
			pslib.WithLogLevel(pslib.LogLevelWarn),
		)
		if err != nil {
			return nil, &LoginError{Kind: FailedUnknown, Err: errors.New("could not construct the portal client")}
		}

		// The browser profile holds the session cookies once the login
		// succeeds. It gets a directory of its own (under TMPDIR, which the
		// chart backs with memory) that is removed on every way out of this
		// function, instead of being left to the browser to clean up.
		profile, err := os.MkdirTemp("", "ps-login-profile-")
		if err != nil {
			return nil, &LoginError{Kind: FailedUnknown, Err: errors.New("could not create a browser profile directory")}
		}
		defer os.RemoveAll(profile)

		opts := &pslib.AuthOptions{Timeout: LoginTimeout, NoSandbox: cfg.ChromeNoSandbox, UserDataDir: profile}
		if err := client.AuthenticateWithOptions(ctx, opts); err != nil {
			return nil, &LoginError{Kind: ClassifyLoginFailure(err.Error()), Err: err}
		}
		session := client.ExportSession()
		if session == nil {
			return nil, &LoginError{Kind: FailedUnknown, Err: errors.New("the library reported success but exported no session")}
		}
		return session, nil
	}
}

// browserStartMarkers identify a browser that never came up. They are
// chromedp's and os/exec's own messages; no page had been requested.
var browserStartMarkers = []string{
	"chrome failed to start",
	"executable file not found",
	"fork/exec",
	"websocket url timeout reached",
}

// preSubmitNetErrors are the Chromium network errors that mean the sign-in
// page itself could not be fetched: name resolution and connection
// establishment. Certificate errors, HTTP errors and aborts are left out on
// purpose -- none is "try again in ten seconds".
var preSubmitNetErrors = []string{
	"ERR_NAME_NOT_RESOLVED",
	"ERR_NAME_RESOLUTION_FAILED",
	"ERR_DNS_TIMED_OUT",
	"ERR_CONNECTION_REFUSED",
	"ERR_CONNECTION_TIMED_OUT",
	"ERR_CONNECTION_RESET",
	"ERR_CONNECTION_CLOSED",
	"ERR_ADDRESS_UNREACHABLE",
	"ERR_INTERNET_DISCONNECTED",
	"ERR_NETWORK_CHANGED",
	"ERR_TIMED_OUT",
}

// pageLoadErrorPrefix is how chromedp reports a failed Navigate action. The
// library's login issues exactly one, for the sign-in page, as its first
// step; the navigation that follows the submit is not a Navigate action and
// cannot produce this text. So an error carrying it happened before any
// field was filled in.
const pageLoadErrorPrefix = "page load error net::"

// rejectedMarker is the library's message when the portal answers the
// submitted form with its invalid-credentials notice.
const rejectedMarker = "login failed: invalid credentials"

// ClassifyLoginFailure places a library login error. The library returns
// one undifferentiated error for every failure, so its text is all there is
// to go on; the match is therefore an allowlist. Whatever is not positively
// recognised as "before the submit" is FailedUnknown and is never retried
// -- a timeout, a missing form field, an error this code has not seen.
// Getting that wrong in the other direction is what locks an account.
func ClassifyLoginFailure(msg string) Failure {
	if strings.Contains(msg, rejectedMarker) {
		return Rejected
	}
	for _, m := range browserStartMarkers {
		if strings.Contains(msg, m) {
			return FailedBeforeSubmit
		}
	}
	if i := strings.Index(msg, pageLoadErrorPrefix); i >= 0 {
		code := msg[i+len(pageLoadErrorPrefix):]
		for _, n := range preSubmitNetErrors {
			if code == n || strings.HasPrefix(code, n+" ") || strings.HasPrefix(code, n+"\n") {
				return FailedBeforeSubmit
			}
		}
	}
	return FailedUnknown
}

// ProbeHTTPClient builds the client for ONE home-page read with a session.
// base replaces the innermost transport in tests; nil is the default one.
//
// It is the connector's own client -- pinned to the portal host, GET and
// HEAD only, no cookie jar -- with one addition underneath the pin: a
// gatewayCookies overlay, so that the access gateway renewing its cookie
// during the read does not make a good session look unreadable.
func ProbeHTTPClient(cfg Config, base http.RoundTripper) *http.Client {
	client := powerschool.NewHTTPClient(cfg.Host, false, nil, ProbeTimeout)
	if t, ok := client.Transport.(*powerschool.Transport); ok {
		t.Base = psportal.NewGatewayCookies(base)
	}
	return client
}

// Probe returns the library's reading of the signed-in home page with a
// session: the id of the student the page marks as selected ("" when it
// marks none). An error for which pslib.IsSessionExpired is true means the
// portal does not accept the session; any other error means the question
// was not answered.
//
// It builds a client that holds no credentials (so the library cannot log
// in again) on the connector's own transport, pinned to the portal host
// with no POST allowed at all: neither of the connector's two exceptions is
// enabled here.
func Probe(cfg Config, base http.RoundTripper) func(ctx context.Context, session *pslib.SessionExport) (string, error) {
	return func(ctx context.Context, session *pslib.SessionExport) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()

		lib, err := pslib.NewClient(cfg.BaseURL,
			pslib.WithHTTPClient(ProbeHTTPClient(cfg, base)),
			pslib.WithHTTPPageFetch(),
			pslib.WithSession(session.Cookies, session.ExpiresAt),
			pslib.WithLogLevel(pslib.LogLevelWarn),
		)
		if err != nil {
			return "", err
		}
		// GetHomePage fails unless the page carries the student switcher,
		// which only a signed-in guardian page does, and reports the
		// sign-in page (redirect or 200) as a session error.
		page, err := lib.GetHomePage(ctx)
		if err != nil {
			return "", err
		}
		if selected := page.SelectedStudent(); selected != nil {
			return selected.ID, nil
		}
		return "", nil
	}
}

// Verdict asks the portal about a session with the connector's own probe
// (powerschool.NewSessionProbe): one GET of the home page, classified live,
// dead or unknown by where the redirects ended and what kind of page came
// back. It is the same rule the connector applies at the start of every
// poll, so a login workload and the connector cannot disagree about whether
// a session is dead.
func Verdict(ctx context.Context, cfg Config, base http.RoundTripper, session *pslib.SessionExport) powerschool.SessionVerdict {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	return powerschool.NewSessionProbe(ProbeHTTPClient(cfg, base), cfg.BaseURL, session)(ctx)
}

// CredentialFingerprint identifies a username/password pair without
// revealing either: SHA-256 over the salt and both values, NUL-separated so
// ("ab","c") and ("a","bc") differ.
func CredentialFingerprint(salt, username, password string) string {
	return loginvault.CredentialFingerprint(salt, username, password)
}

// NormalizeSession drops cookies the connector would drop and sets the
// session's expiry. It returns nil when no cookie is left.
//
// The expiry the library reports is not evidence of the portal's session
// lifetime: it is the latest expiry among the browser's cookies (which can
// be a year out for a preference cookie) or, when every cookie is a
// session cookie, "four hours from now". The connector refuses a session
// past its expires_at, so a four-hour guess would stop the afternoon and
// evening polls on a session the portal still honours. The login workloads
// write login time plus a configured lifetime instead: an upper bound on
// how long the connector will present the session. The portal says when it
// really ends, by refusing it, and the connector handles that.
func NormalizeSession(s *pslib.SessionExport, expiresAt time.Time) *pslib.SessionExport {
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

// Field names of the session secret in Vault.
const (
	// FieldSession is the session JSON, as one string. This is the
	// property ESO copies into the connector's session.json; the connector
	// parses it with powerschool.ParseSession.
	// It is also the field the connector reads out of a login broker's
	// answer, hence the connector's own constant.
	FieldSession = powerschool.SessionField
	// FieldLoggedInAt is when the login was made (RFC 3339, UTC).
	FieldLoggedInAt = "logged_in_at"
	// FieldExpiresAt is the same instant as in the session JSON.
	FieldExpiresAt = "expires_at"
	// FieldSelectedStudent is the student the portal had selected after
	// login, "" when unknown. Kept in Vault only: never logged, and not
	// part of what ESO syncs.
	FieldSelectedStudent = "selected_student_id"
)

// SessionData shapes the Vault secret (see the Field constants).
func SessionData(session *pslib.SessionExport, loggedInAt time.Time, selected string) (map[string]any, error) {
	doc, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		FieldSession:         string(doc),
		FieldLoggedInAt:      loggedInAt.UTC().Format(time.RFC3339),
		FieldExpiresAt:       session.ExpiresAt.UTC().Format(time.RFC3339),
		FieldSelectedStudent: selected,
	}, nil
}
