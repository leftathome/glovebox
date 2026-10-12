// Package loginbroker is a small always-on service that logs in to a site
// when, and only when, a connector that found its session dead asks for a
// new one. The connector never holds the account's credentials; the broker
// never does the connector's reading.
//
// The package is the generic core: the HTTP endpoint, who may call it, and
// the policy every login is subject to (single-flight, probe before login,
// a minimum interval, blackout windows, a failure latch). Everything that
// is about one particular site -- how to log in, how to tell a live session
// from a dead one, what a session looks like -- is behind Driver. One
// broker process serves one site with one account: see DESIGN.md beside
// this file for the contract, the rules and the threat model.
package loginbroker

import (
	"context"
	"time"

	"github.com/leftathome/glovebox/internal/dailywindow"
)

// Credentials are the account's. The core hands them to Driver.Login and
// does nothing else with them: it does not log them, store them or put
// them in an error.
type Credentials struct {
	Username string
	Password string
}

// Session is whatever a driver's login produces. It is opaque to the core,
// which only passes it back to the same driver.
type Session any

// ProbeState is what one look at the site says about a session.
type ProbeState int

const (
	// ProbeUnknown: the question was not answered (network failure, server
	// error, a page that is neither the signed-in page nor the sign-in
	// page). Nothing may be concluded; in particular the core does not log
	// in, because a login would end the session if it is alive.
	ProbeUnknown ProbeState = iota
	// ProbeAlive: the site served a signed-in page.
	ProbeAlive
	// ProbeDead: the site positively refused the session.
	ProbeDead
)

// String is the log and metric form.
func (s ProbeState) String() string {
	switch s {
	case ProbeAlive:
		return "alive"
	case ProbeDead:
		return "dead"
	}
	return "unknown"
}

// ProbeResult is a ProbeState with a classified reason: a short token from
// a list the driver fixes, never page content or a URL.
type ProbeResult struct {
	State  ProbeState
	Reason string
}

// LoginFailure says what a failed login means for what may happen next.
type LoginFailure int

const (
	// LoginUnknown: the login failed and the credentials may have been
	// submitted. Never retried within a request; counted toward the latch.
	// This is what a driver must return for anything it cannot positively
	// place in one of the other two kinds.
	LoginUnknown LoginFailure = iota
	// LoginNotSubmitted: the login failed before the credentials left the
	// process (the browser did not start, the sign-in page was unreachable
	// or was not the expected form). Safe to try again; not counted toward
	// the latch and not counted as a login for the minimum interval.
	LoginNotSubmitted
	// LoginRejected: the site refused the submitted credentials. Sets the
	// latch at once: repeating a wrong password is how an account gets
	// locked.
	LoginRejected
)

// LoginError is the error a Driver's Login returns. Detail is a classified
// token (lower-case letters, digits, underscore, colon); the core logs and
// returns Detail and never the wrapped error's text, which a browser driver
// or an HTTP client may have assembled from anything.
type LoginError struct {
	Kind   LoginFailure
	Detail string
	// Err is kept for errors.Is/As by the driver's own tests. The core
	// ignores it.
	Err error
}

func (e *LoginError) Error() string {
	if e.Detail != "" {
		return "login failed: " + e.Detail
	}
	return "login failed"
}

func (e *LoginError) Unwrap() error { return e.Err }

// Driver is everything the broker needs to know about one site.
//
// Rules a driver must keep, because the core relies on them:
//
//   - Login submits the credentials AT MOST ONCE per call. It does not
//     retry internally after a submission, whatever the result.
//   - Login returns a *LoginError whose Kind is LoginNotSubmitted only when
//     it is certain nothing was submitted. When in doubt: LoginUnknown.
//   - Probe is read-only: it must be unable to send anything but a GET or
//     a HEAD, by construction of its transport and not by intention.
//   - Apart from the credential submission inside Login, a driver sends the
//     site nothing that changes state.
//   - No method logs, and no error or reason quotes a credential, a cookie,
//     a token or page content.
//   - A driver makes no request the core did not ask for: no keepalive, no
//     background refresh.
type Driver interface {
	// Site is the driver's name: lower-case, used in logs, metric labels
	// and answers ("powerschool").
	Site() string

	// Login logs in and returns the new session. See the rules above.
	Login(ctx context.Context, creds Credentials) (Session, error)

	// Probe asks the site whether it still accepts the session.
	Probe(ctx context.Context, session Session) ProbeResult

	// Encode turns a session into the fields stored in Vault and returned
	// to the caller. loggedInAt is when the session was created. The field
	// "logged_in_at" is the core's and is written by it; a driver that also
	// sets it is overruled.
	Encode(session Session, loggedInAt time.Time) (map[string]string, error)

	// Decode is the inverse of Encode, for a session read back from the
	// store. An error means the stored fields are not a usable session;
	// the core then treats the store as holding none. The error must not
	// quote the fields.
	Decode(fields map[string]string) (Session, error)

	// Fingerprint identifies a session without revealing it: a hex digest
	// that the site's connector computes the same way, so that the
	// connector's "this one is dead" and the broker's "this one is stored"
	// can be compared.
	Fingerprint(session Session) string
}

// Constraints are limits a driver puts on the policy, which configuration
// can tighten but not loosen.
type Constraints struct {
	// Blackout windows that always apply, whatever is configured.
	Blackout dailywindow.Schedule
	// MinInterval is the least configurable interval between logins.
	MinInterval time.Duration
	// LoginHeadroom is how long before a blackout window a login must not
	// start: the longest ONE call of Login may take from its first request
	// to its submit -- every step of it, not only the browser -- plus a
	// margin. The core also cancels a Login call that runs longer.
	LoginHeadroom time.Duration
	// Location is the account's local time zone: the calendar the daily
	// login budget is counted on. Nil means the zone of the configured
	// blackout, else UTC.
	Location *time.Location
}

// Constrained is implemented by a driver that has Constraints.
type Constrained interface {
	Constraints() Constraints
}
