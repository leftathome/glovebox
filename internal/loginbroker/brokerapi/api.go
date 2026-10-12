// Package brokerapi is the wire contract of the login broker
// (internal/loginbroker) and the client a connector calls it with.
//
// It is deliberately small and imports nothing of the broker: a connector
// links this package and gets one request it can make, to one address, and
// nothing that can log in, read Vault or reach the Kubernetes API.
package brokerapi

import (
	"errors"
	"time"
)

// RefreshPath is the one endpoint a caller uses.
const RefreshPath = "/v1/session/refresh"

// Reasons a caller may give. A closed list: the value is logged and
// counted, so free text is refused.
const (
	// ReasonSessionDead: the site answered the caller's session with its
	// sign-in page (or its gateway refused it) before a poll.
	ReasonSessionDead = "session_dead"
	// ReasonNoSession: the caller has no session at all.
	ReasonNoSession = "no_session"
	// ReasonSessionExpired: the caller's session document says it has
	// expired; the site was not asked.
	ReasonSessionExpired = "session_expired"
	// ReasonAuthFailure: the site stopped accepting the session in the
	// middle of the caller's work.
	ReasonAuthFailure = "auth_failure"
)

// ValidReason reports whether r is one of the reasons above.
func ValidReason(r string) bool {
	switch r {
	case ReasonSessionDead, ReasonNoSession, ReasonSessionExpired, ReasonAuthFailure:
		return true
	}
	return false
}

// NoFingerprint stands in for the fingerprint when the caller has no
// session.
const NoFingerprint = "none"

// FingerprintLen is how many lower-case hex digits of a session
// fingerprint travel in a request and an answer. A prefix is enough to tell
// two sessions apart and says nothing about either.
const FingerprintLen = 16

// ShortFingerprint reduces a full session fingerprint to the wire form. An
// empty fingerprint is NoFingerprint.
func ShortFingerprint(full string) string {
	if full == "" || full == NoFingerprint {
		return NoFingerprint
	}
	if len(full) > FingerprintLen {
		full = full[:FingerprintLen]
	}
	return full
}

// ValidFingerprint reports whether fp is NoFingerprint or exactly
// FingerprintLen lower-case hex digits.
func ValidFingerprint(fp string) bool {
	if fp == NoFingerprint {
		return true
	}
	if len(fp) != FingerprintLen {
		return false
	}
	for _, r := range fp {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Request is the body of POST /v1/session/refresh.
type Request struct {
	// Reason is one of the Reason constants.
	Reason string `json:"reason"`
	// Fingerprint is the caller's view of the session it found dead
	// (ShortFingerprint), or NoFingerprint. The broker uses it to tell a
	// request about a session it has already replaced from a request about
	// the one it has stored; it never decides on it alone whether to log
	// in.
	Fingerprint string `json:"fingerprint"`
}

// Outcomes of a refresh. Exactly two of them carry a session.
const (
	// OutcomeRefreshed: the broker logged in. Session is the new one.
	OutcomeRefreshed = "refreshed"
	// OutcomeCurrent: the stored session is alive. Session is that one and
	// no login was made.
	OutcomeCurrent = "current"
	// OutcomeDeferred: the stored session is dead but policy forbids a
	// login now. RetryAfter says when one may be made. Code is CodeBlackout,
	// CodeMinInterval, CodeCooldown, CodeLoginInProgress or
	// CodeStateConflict.
	OutcomeDeferred = "deferred"
	// OutcomeBudgetExhausted: the stored session is dead and the day's
	// login budget for the site is spent. RetryAfter is the next local
	// midnight. Nothing a caller does before then causes a login.
	OutcomeBudgetExhausted = "budget_exhausted"
	// OutcomeLatched: logins are stopped until a person clears the latch.
	// Code is CodeCredentialsRejected or CodeConsecutiveFailures.
	OutcomeLatched = "latched"
	// OutcomeInconclusive: the broker could not tell whether the stored
	// session is alive, and does not log in on a guess.
	OutcomeInconclusive = "inconclusive"
	// OutcomeFailed: a login was attempted and produced no session, or
	// could not be attempted. Code says which.
	OutcomeFailed = "failed"
)

// Codes that qualify an outcome. A closed list.
const (
	CodeBlackout    = "blackout"
	CodeMinInterval = "min_interval"
	CodeCooldown    = "cooldown"
	// CodeLoginInProgress: another workload sharing the account's record
	// (a second broker pod during a restart, the scheduled login job) has a
	// login in flight. Its outcome is not yet known and is not guessed.
	CodeLoginInProgress = "login_in_progress"
	// CodeStateConflict: the record changed between this request reading
	// it and writing it. Nothing was decided on the stale read.
	CodeStateConflict       = "state_conflict"
	CodeCredentialsRejected = "credentials_rejected"
	CodeConsecutiveFailures = "consecutive_failures"
	CodeLoginUnconfirmed    = "login_unconfirmed"
	CodeLoginFailed         = "login_failed"
	CodeLoginNotSubmitted   = "login_not_submitted"
	CodeStoreUnavailable    = "store_unavailable"
	CodeStoreRejected       = "store_rejected"
	CodeProbeInconclusive   = "probe_inconclusive"
	CodeInternal            = "internal"
	// CodeSuperseded: another writer's login replaced this one while it
	// was being stored. The store holds the other session.
	CodeSuperseded = "superseded"
	// CodeSessionNotStored qualifies OutcomeRefreshed when the login worked
	// but the session could not be written to the store: the caller has it,
	// other readers do not yet.
	CodeSessionNotStored = "session_not_stored"
)

// Response is the body of every answer the broker gives to an
// authenticated, well-formed request, with HTTP status 200. Anything else
// (401, 403, 400, 429, 503) carries an Error body instead.
type Response struct {
	Site    string `json:"site"`
	Outcome string `json:"outcome"`
	// Code qualifies the outcome; empty when there is nothing to add.
	Code string `json:"code,omitempty"`
	// Detail is a further classified token from the site driver (for
	// example which check of the sign-in page failed). Never free text.
	Detail string `json:"detail,omitempty"`
	// RetryAfter is set with OutcomeDeferred: the earliest time a login may
	// be made.
	RetryAfter *time.Time `json:"retry_after,omitempty"`
	// Session is set with OutcomeRefreshed and OutcomeCurrent: the fields of
	// the session exactly as the broker writes them to the store. What they
	// are is the site driver's business (for PowerSchool: "session", the
	// session JSON the connector parses).
	Session map[string]string `json:"session,omitempty"`
	// Fingerprint is ShortFingerprint of that session.
	Fingerprint string `json:"fingerprint,omitempty"`
	// LoggedInAt is when that session was created, when known.
	LoggedInAt *time.Time `json:"logged_in_at,omitempty"`
}

// HasSession reports whether the answer carries a session.
func (r *Response) HasSession() bool {
	return r != nil && (r.Outcome == OutcomeRefreshed || r.Outcome == OutcomeCurrent) && len(r.Session) > 0
}

// Error is the body of a non-200 answer.
type Error struct {
	// Error is a classified code: unauthenticated, forbidden, bad_request,
	// rate_limited, auth_unavailable, method_not_allowed.
	Error string `json:"error"`
}

// ErrUnexpectedAnswer is returned by Client.Refresh for an answer that is
// not a Response the caller can act on.
var ErrUnexpectedAnswer = errors.New("login broker: unexpected answer")
