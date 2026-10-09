package schoology

import "github.com/leftathome/glovebox/connector"

// RejectedSession remembers, across process restarts, the one session
// Schoology has refused, so a restarted process does not present the same
// dead cookie again. The implementation is shared with the other
// saved-session connectors; see connector.RejectedSession.
type RejectedSession = connector.RejectedSession

// NewRejectedSession returns a marker under stateDir, or a disabled one when
// stateDir is empty.
func NewRejectedSession(stateDir string) RejectedSession {
	return connector.NewRejectedSession(stateDir)
}

// SessionFingerprint returns a stable, non-reversible identifier for a
// session id.
func SessionFingerprint(sessID string) string {
	return connector.SessionFingerprint(sessID)
}
