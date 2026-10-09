package schoology

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// RejectedSession remembers, across process restarts, the one session
// Schoology has refused.
//
// A refused session is a permanent error: the process exits and the
// kubelet restarts it. Without a memory of the refusal the new process
// loads the same credentials file and immediately polls with the same dead
// cookie, and does so again on every restart until the refresher replaces
// the session -- a steady stream of failed authenticated requests against a
// real parent account. The marker lets the new process see that nothing
// has changed and wait instead.
//
// The marker holds a SHA-256 of the session id, never the id itself. It
// lives in the connector state directory, which outlives container
// restarts within a pod.
type RejectedSession struct {
	// Path is the marker file. Empty disables the mechanism: IsRejected
	// reports false and Mark is a no-op.
	Path string
}

// NewRejectedSession returns a marker under stateDir, or a disabled one when
// stateDir is empty.
func NewRejectedSession(stateDir string) RejectedSession {
	if stateDir == "" {
		return RejectedSession{}
	}
	return RejectedSession{Path: filepath.Join(stateDir, "rejected-session")}
}

// SessionFingerprint returns a stable, non-reversible identifier for a
// session id.
func SessionFingerprint(sessID string) string {
	sum := sha256.Sum256([]byte(sessID))
	return hex.EncodeToString(sum[:])
}

// IsRejected reports whether the session with this fingerprint is the one
// recorded as refused. A missing or unreadable marker is "not rejected":
// failing open here costs one poll, failing closed would stop the connector
// forever on a transient filesystem error.
func (r RejectedSession) IsRejected(fingerprint string) bool {
	if r.Path == "" {
		return false
	}
	b, err := os.ReadFile(r.Path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == fingerprint
}

// Mark records the session with this fingerprint as refused.
func (r RejectedSession) Mark(fingerprint string) error {
	if r.Path == "" {
		return nil
	}
	if fingerprint == "" {
		return errors.New("schoology: empty session fingerprint")
	}
	return os.WriteFile(r.Path, []byte(fingerprint+"\n"), 0o600)
}
