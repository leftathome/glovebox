package powerschool

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// WaitHealth is the stand-in health server for the time the binary spends
// waiting for a fresh session (WaitForFreshSession); see
// connector.WaitHealth. It answers /healthz 200 and /readyz 200 -- waiting,
// by design, is healthy, and readiness gates every Helm upgrade and rollback
// of the release (glovebox-wekc.17) -- and serves /statusz and /metrics, where
// the session state (powerschool_session_state) says what it is waiting for
// and since when.
type WaitHealth = connector.WaitHealth

// StartWaitHealth listens on addr (":8080", or "127.0.0.1:0" in tests).
// status, which may be nil, is what /statusz and the /readyz body report.
func StartWaitHealth(addr string, status *connector.Status) (*WaitHealth, error) {
	return connector.StartWaitHealth(addr, "powerschool", status)
}

// SessionClock keeps, in the connector's state directory, when the
// connector first saw the session it is running on and how old the last
// rejected session was. It exists to measure the portal's real session
// lifetime: the expiry in the session document is a placeholder.
//
// "Age" is time since this connector first loaded that session, which is a
// little less than time since login. Files hold a fingerprint and numbers,
// never a cookie. An empty Dir disables it.
type SessionClock struct {
	Dir string
}

func (c SessionClock) firstSeenPath() string   { return filepath.Join(c.Dir, "session-first-seen") }
func (c SessionClock) rejectedAgePath() string { return filepath.Join(c.Dir, "session-rejected-age") }

// FirstSeen returns when the session with this fingerprint was first seen,
// recording now if it has not been seen before. A disabled or unwritable
// clock returns now.
func (c SessionClock) FirstSeen(fingerprint string, now time.Time) time.Time {
	if c.Dir == "" || fingerprint == "" {
		return now
	}
	if raw, err := os.ReadFile(c.firstSeenPath()); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) == 2 && fields[0] == fingerprint {
			if t, err := time.Parse(time.RFC3339, fields[1]); err == nil && !t.After(now) {
				return t
			}
		}
	}
	_ = os.WriteFile(c.firstSeenPath(), []byte(fingerprint+" "+now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	return now
}

// SaveRejectedAge records how old the session was when it was rejected.
func (c SessionClock) SaveRejectedAge(age time.Duration) error {
	if c.Dir == "" || age < 0 {
		return nil
	}
	return os.WriteFile(c.rejectedAgePath(), []byte(strconv.FormatInt(int64(age/time.Second), 10)+"\n"), 0o600)
}

// LastRejectedAge returns the age recorded by SaveRejectedAge, if any.
func (c SessionClock) LastRejectedAge() (time.Duration, bool) {
	if c.Dir == "" {
		return 0, false
	}
	raw, err := os.ReadFile(c.rejectedAgePath())
	if err != nil {
		return 0, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}
