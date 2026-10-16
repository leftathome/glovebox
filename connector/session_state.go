package connector

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Session states reported by SessionState, for connectors that run on a
// saved session of a person's account.
const (
	// SessionStateUnknown: not checked yet since the process started (or
	// since a new session arrived), or the last check could not tell.
	SessionStateUnknown = "unknown"
	// SessionStateUsable: the last check found the session accepted.
	SessionStateUsable = "usable"
	// SessionStateWaiting: the session is known to be refused and the
	// connector is waiting, making no upstream requests with it, for a new
	// one. Healthy and ready; degraded.
	SessionStateWaiting = "waiting_for_new_session"
)

// SessionStateCondition is the Status condition name SessionState sets.
const SessionStateCondition = "session"

var sessionStates = []string{SessionStateUnknown, SessionStateUsable, SessionStateWaiting}

// SessionState publishes a saved-session connector's session state as a
// Status condition and as two Prometheus gauges:
//
//	<prefix>_session_state{state="unknown|usable|waiting_for_new_session"}
//	    1 for the current state, 0 for the others
//	<prefix>_session_state_since_timestamp_seconds{state="<current state>"}
//	    Unix time the current state was entered; only the current state's
//	    series exists, so "waiting for more than N hours" is one expression:
//	    time() - <prefix>_session_state_since_timestamp_seconds{state="waiting_for_new_session"} > N*3600
//
// The gauges are plain client_golang collectors on the registerer given
// (prometheus.DefaultRegisterer in the binaries), so they are served both by
// WaitHealth, before connector.Run, and by the framework's /metrics after
// it. All methods are safe on a nil *SessionState.
type SessionState struct {
	status *Status
	state  *prometheus.GaugeVec
	since  *prometheus.GaugeVec
	now    func() time.Time

	mu  sync.Mutex
	cur string
	at  time.Time
}

// NewSessionState registers the gauges for prefix (e.g. "powerschool") on
// reg and starts in SessionStateUnknown. status may be nil.
func NewSessionState(reg prometheus.Registerer, prefix string, status *Status) (*SessionState, error) {
	s := &SessionState{
		status: status,
		now:    time.Now,
		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_session_state",
			Help: "1 for the connector's current session state (unknown, usable, waiting_for_new_session), 0 for the others. " +
				"waiting_for_new_session is healthy and Ready: the connector makes no upstream requests until a new session arrives.",
		}, []string{"state"}),
		since: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_session_state_since_timestamp_seconds",
			Help: "Unix time the connector's current session state was entered (one series, labelled with the current state).",
		}, []string{"state"}),
	}
	if reg != nil {
		if err := reg.Register(s.state); err != nil {
			return nil, fmt.Errorf("register %s_session_state: %w", prefix, err)
		}
		if err := reg.Register(s.since); err != nil {
			reg.Unregister(s.state)
			return nil, fmt.Errorf("register %s_session_state_since_timestamp_seconds: %w", prefix, err)
		}
	}
	s.Set(SessionStateUnknown, time.Time{}, "not checked yet")
	return s, nil
}

// Set moves to state. since is when the state began; zero means "now", or,
// when the state is unchanged, keeps the time it was first entered.
func (s *SessionState) Set(state string, since time.Time, message string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if since.IsZero() {
		if s.cur == state && !s.at.IsZero() {
			since = s.at
		} else {
			since = s.now()
		}
	}
	s.cur, s.at = state, since
	for _, st := range sessionStates {
		v := 0.0
		if st == state {
			v = 1
		}
		s.state.WithLabelValues(st).Set(v)
	}
	s.since.Reset()
	s.since.WithLabelValues(state).Set(float64(since.Unix()))
	s.status.Set(SessionStateCondition, Condition{
		State:    state,
		Since:    since,
		Degraded: state == SessionStateWaiting,
		Message:  message,
	})
}

// Current returns the current state and when it was entered.
func (s *SessionState) Current() (string, time.Time) {
	if s == nil {
		return "", time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.at
}

// MarkedAt returns when the marker was last written: when the session it
// names was found refused. False when there is no marker.
func (r RejectedSession) MarkedAt() (time.Time, bool) {
	if r.Path == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(r.Path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}
