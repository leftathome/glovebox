package main

import (
	"errors"
	"net/http"

	"github.com/leftathome/glovebox/internal/pslogin"
)

// The login itself, its failure classification and the home-page read live
// in internal/pslogin, shared with the login broker so the two cannot
// drift. These are this binary's names for them.

// loginTimeout bounds one browser login, browser start included. It is
// also part of the blackout headroom (loginHeadroom).
const loginTimeout = pslogin.LoginTimeout

// probeTimeout bounds one home-page read.
const probeTimeout = pslogin.ProbeTimeout

// loginFailure says what a failed login means for a second attempt.
type loginFailure = pslogin.Failure

const (
	// loginFailedUnknown: anything not positively identified as one of the
	// other two. The credentials may have been submitted, so no retry.
	loginFailedUnknown = pslogin.FailedUnknown
	// loginFailedBeforeSubmit: the browser did not start, or the sign-in
	// page could not be reached. Nothing was typed into a form. This is the
	// only kind that allows the single retry.
	loginFailedBeforeSubmit = pslogin.FailedBeforeSubmit
	// loginRejected: the portal answered the submitted form with its
	// invalid-credentials notice.
	loginRejected = pslogin.Rejected
)

// loginError is what a loginFunc returns on failure.
type loginError struct {
	kind loginFailure
	err  error
}

func (e *loginError) Error() string { return e.err.Error() }
func (e *loginError) Unwrap() error { return e.err }

// failureKind extracts the loginFailure from err: this binary's own
// loginError (the fakes in its tests), else the shared package's. An error
// that is neither is loginFailedUnknown, the kind that never retries.
func failureKind(err error) loginFailure {
	var le *loginError
	if errors.As(err, &le) {
		return le.kind
	}
	return pslogin.KindOf(err)
}

// classifyLoginFailure places a library login error.
func classifyLoginFailure(msg string) loginFailure { return pslogin.ClassifyLoginFailure(msg) }

// portal is the part of the configuration the shared login code needs.
func (c config) portal() pslogin.Config {
	return pslogin.Config{
		BaseURL:         c.BaseURL,
		Host:            c.Host,
		Username:        c.Username,
		Password:        c.Password,
		ChromeNoSandbox: c.ChromeNoSandbox,
	}
}

// realLogin returns the production loginFunc: one powerschool-go browser
// login.
func realLogin(cfg config) loginFunc { return pslogin.Login(cfg.portal()) }

// probeHTTPClient builds the client for ONE home-page read with a session.
func probeHTTPClient(cfg config, base http.RoundTripper) *http.Client {
	return pslogin.ProbeHTTPClient(cfg.portal(), base)
}

// realProbe returns the production probeFunc.
func realProbe(cfg config, base http.RoundTripper) probeFunc {
	return pslogin.Probe(cfg.portal(), base)
}
