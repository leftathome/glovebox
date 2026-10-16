package schoology

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologyauth "github.com/leftathome/schoology-go/auth"
	"github.com/prometheus/client_golang/prometheus"
)

// SessionWaitOptions configures WaitForFreshCredentials.
type SessionWaitOptions struct {
	// HealthAddr is the framework's health port (":8080").
	HealthAddr string
	// ListenerAddr is the trigger listener's port (":8081"). While the
	// connector waits it is Ready, so the Service routes triggers to it; the
	// stand-in answers them 503 "waiting for a session" instead of refusing
	// the connection. Empty: not bound.
	ListenerAddr string
	// Interval between re-reads of the credentials file.
	Interval time.Duration
	Status   *connector.Status
	State    *connector.SessionState
	// Gatherer serves /metrics; nil means the default registry.
	Gatherer prometheus.Gatherer
}

// ErrReleasePort: the stand-in could not give its ports back.
var ErrReleasePort = errors.New("schoology: could not release the health and listener ports")

// WaitForFreshCredentials waits out a session Schoology already refused.
//
// If creds is the session recorded in rejected, it waits -- re-reading the
// credentials every Interval and contacting nothing upstream -- until load
// returns a different session (the refresher writes one and ESO updates the
// mounted Secret in place), or ctx ends. Meanwhile a stand-in serves the
// health port, alive AND ready (waiting is what the process should be
// doing, and readiness gates every Helm upgrade and rollback of the
// release; glovebox-wekc.17), and the listener port, answering 503. The
// session is reported as waiting since the refusal (the marker's time, so
// it survives restarts) and, once a new session is in place, as unknown.
// Both ports are free again when it returns.
func WaitForFreshCredentials(ctx context.Context, rejected RejectedSession, creds *schoologyauth.Credentials,
	load func() (*schoologyauth.Credentials, error), o SessionWaitOptions) (*schoologyauth.Credentials, error) {
	var health *connector.WaitHealth
	waited := false
	var err error
	for rejected.IsRejected(SessionFingerprint(creds.SessID)) {
		slog.Error("schoology: session was already rejected upstream; waiting for a new one, not polling",
			"recovery_doc", "docs/AUTH-RECOVERY.md")
		if !waited {
			waited = true
			since, _ := rejected.MarkedAt()
			o.State.Set(connector.SessionStateWaiting, since,
				"Schoology refused this session; waiting for a different one, not contacting Schoology")
			h, herr := connector.StartWaitHealthWith(connector.WaitHealthOptions{
				Addr: o.HealthAddr, Name: "schoology", Status: o.Status, Gatherer: o.Gatherer,
				UnavailableAddr:    o.ListenerAddr,
				UnavailableMessage: "unavailable: waiting for a new Schoology session; no poll can run",
			})
			if herr != nil {
				slog.Error("schoology: could not serve health endpoints while waiting", "error", herr)
			}
			health = h
		}
		t := time.NewTimer(o.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			err = ctx.Err()
		case <-t.C:
		}
		if err != nil {
			break
		}
		if fresh, lerr := load(); lerr == nil {
			creds = fresh
		}
	}
	if waited && err == nil {
		o.State.Set(connector.SessionStateUnknown, time.Time{}, "a new session is in place; not checked yet")
	}
	if stopErr := health.Stop(); stopErr != nil {
		slog.Error("schoology: could not release the health port", "error", stopErr)
		return nil, ErrReleasePort
	}
	if err != nil {
		return nil, err
	}
	return creds, nil
}
