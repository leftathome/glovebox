package psportal

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// A login request is how the connector says "my session is dead; a poll is
// waiting" to the login workload. It is one short string in one field of
// one ConfigMap, and it is all the connector can do about a login: it
// cannot create a Job, start a pod, or reach a credential. The login
// workload reads the field on its own schedule and decides, by its own
// rules (blackout, latch, minimum interval), whether to act on it.
//
// The format is deliberately poor in content:
//
//	<RFC 3339 UTC time> <16 hex digits | "none">
//
// the time the request was made, and a prefix of the fingerprint (a SHA-256
// over the cookies; see the connector's SessionFingerprint) of the session
// the connector found dead, or "none" when it had no session at all. The
// login workload uses the fingerprint only to tell whether the session it
// has stored is that same dead one.

// LoginRequestKey is the ConfigMap data key that holds the request.
const LoginRequestKey = "request"

// LoginRequestFingerprintLen is how many hex digits of the session
// fingerprint a request carries.
const LoginRequestFingerprintLen = 16

// NoSessionFingerprint stands in for the fingerprint when the connector had
// no session.
const NoSessionFingerprint = "none"

// maxLoginRequestLen bounds what is parsed. A valid request is 37 or 25
// characters.
const maxLoginRequestLen = 64

// LoginRequest is a parsed request.
type LoginRequest struct {
	// At is when the connector made the request.
	At time.Time
	// Fingerprint is the dead session's fingerprint prefix, or
	// NoSessionFingerprint.
	Fingerprint string
}

// RequestFingerprint reduces a full session fingerprint to the form a
// request carries. An empty fingerprint is NoSessionFingerprint.
func RequestFingerprint(full string) string {
	if full == "" {
		return NoSessionFingerprint
	}
	if len(full) > LoginRequestFingerprintLen {
		full = full[:LoginRequestFingerprintLen]
	}
	return strings.ToLower(full)
}

// FormatLoginRequest renders a request made at `at` about the session with
// the given full fingerprint.
func FormatLoginRequest(at time.Time, fullFingerprint string) string {
	return at.UTC().Format(time.RFC3339) + " " + RequestFingerprint(fullFingerprint)
}

// ErrNoLoginRequest is returned by ParseLoginRequest for an empty field.
var ErrNoLoginRequest = errors.New("no login request")

// ParseLoginRequest validates and parses the field. Anything that is not
// exactly a request -- too long, extra words, a bad time, a fingerprint
// that is not lower-case hex of the right length -- is an error, and the
// error never quotes the value: the field is written by a process that
// parses untrusted pages.
func ParseLoginRequest(raw string) (LoginRequest, error) {
	if strings.TrimSpace(raw) == "" {
		return LoginRequest{}, ErrNoLoginRequest
	}
	if len(raw) > maxLoginRequestLen {
		return LoginRequest{}, fmt.Errorf("login request is %d bytes; at most %d are read", len(raw), maxLoginRequestLen)
	}
	parts := strings.Split(raw, " ")
	if len(parts) != 2 {
		return LoginRequest{}, errors.New("login request is not \"<time> <fingerprint>\"")
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return LoginRequest{}, errors.New("login request time is not RFC 3339")
	}
	fp := parts[1]
	if fp != NoSessionFingerprint {
		if len(fp) != LoginRequestFingerprintLen {
			return LoginRequest{}, errors.New("login request fingerprint has the wrong length")
		}
		for _, r := range fp {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return LoginRequest{}, errors.New("login request fingerprint is not lower-case hex")
			}
		}
	}
	return LoginRequest{At: at.UTC(), Fingerprint: fp}, nil
}
