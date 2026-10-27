// Package httpapi serves the record journal's API (spec 2026-10-16 4.3,
// 4.9, 4.12, 5.3) on its own listener in the core service. See
// docs/journal-api.md.
//
// The listener is plain HTTP inside the pod network: a NetworkPolicy admits
// only named peers to it (chart, Task 29), and every request carries a
// ServiceAccount token verified by TokenReview, as for the login broker.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/jmetrics"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
	"github.com/leftathome/glovebox/internal/satoken"
)

// Deps are what the server needs.
type Deps struct {
	Journal  *journal.Journal
	Registry *record.Registry
	Auth     satoken.Authenticator
	Policy   *policy.Policy
	Roster   func() *roster.Roster
	Metrics  *jmetrics.Metrics // nil records nothing
	Log      *slog.Logger
	Now      func() time.Time
	Ready    func() bool
	// RequestTimeout bounds each request's context: the TokenReview, the
	// database work and the scans (config journal.request_timeout_seconds).
	// Zero leaves only the listener's own timeouts.
	RequestTimeout time.Duration
	// MaxInFlight bounds the requests doing work at once (TokenReview plus
	// the handler); 0 is DefaultMaxInFlight. A request beyond it is 503
	// unavailable with Retry-After.
	MaxInFlight int
	// Context bounds background work (rescans): the process's lifetime.
	// Required. Cancelling it ends a running rescan at its next batch;
	// Server.Wait then waits for it (see Wait).
	Context context.Context
}

// DefaultMaxInFlight is Deps.MaxInFlight's default.
const DefaultMaxInFlight = 16

// Server is the journal's HTTP surface.
type Server struct {
	d     Deps
	slots chan struct{}
	// snapshotKeys and noteKeys cap the keys of a POST /v1/records and a
	// POST /v1/notes body.
	snapshotKeys strictjson.Limits
	noteKeys     strictjson.Limits
	// noteBytes caps a POST /v1/notes body (noteByteLimit).
	noteBytes int64
	// rescanning is set while a background rescan runs (handleRescan): at
	// most one at a time. rescans counts the running rescan goroutine.
	rescanning atomic.Bool
	rescans    sync.WaitGroup
}

// Wait returns once no background rescan runs. The shutdown contract
// (Task 28): stop the listener first (http.Server.Shutdown), so no request
// can start another rescan; cancel Deps.Context, which ends a running
// rescan at its next batch; then Wait; only then close the database. A
// rescan started after Wait returned is not waited for.
func (s *Server) Wait() { s.rescans.Wait() }

// New checks the dependencies.
func New(d Deps) (*Server, error) {
	if d.Journal == nil || d.Registry == nil || d.Auth == nil || d.Policy == nil || d.Roster == nil || d.Context == nil {
		return nil, errors.New("httpapi: journal, registry, authenticator, policy, roster and context are required")
	}
	if d.RequestTimeout < 0 {
		return nil, errors.New("httpapi: the request timeout must not be negative")
	}
	if d.MaxInFlight < 0 {
		return nil, errors.New("httpapi: the in-flight limit must not be negative")
	}
	if d.MaxInFlight == 0 {
		d.MaxInFlight = DefaultMaxInFlight
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Ready == nil {
		d.Ready = func() bool { return true }
	}
	return &Server{d: d, slots: make(chan struct{}, d.MaxInFlight), snapshotKeys: snapshotKeyLimits(d.Registry),
		noteKeys: noteKeyLimits(d.Registry), noteBytes: noteByteLimit(d.Registry)}, nil
}

// Handler routes the API. Route names, not paths, label the request metric.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+journalapi.PathRecords, s.route("records", s.handleRecords))
	mux.Handle("POST "+journalapi.PathNotes, s.route("notes", s.handleNotes))
	mux.Handle("GET "+journalapi.PathState, s.route("state", s.handleState))
	mux.Handle("GET "+journalapi.PathEvents, s.route("events", s.handleEvents))
	mux.Handle("GET "+journalapi.PathCounts, s.route("counts", s.handleCounts))
	mux.Handle("GET "+journalapi.PathHealthz, s.route("healthz", s.handleHealthz))
	mux.Handle("POST "+journalapi.PathAdminHeld+"{target}", s.route("admin_held", s.handleHeld))
	mux.Handle("POST "+journalapi.PathAdminErase, s.route("admin_erase", s.handleErase))
	mux.Handle("POST "+journalapi.PathAdminRescan, s.route("admin_rescan", s.handleRescan))
	// Everything else, including a known path with another method, is a
	// 404 in the refusal shape rather than net/http's plain-text answer.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.d.Metrics.Request("unrouted", http.StatusNotFound)
		refuse(w, http.StatusNotFound, journalapi.ReasonNotFound, "")
	}))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// refuse answers with the journalapi refusal shape. detail is a fixed text
// of this package or a record.Rejection's detail (which names fields and
// positions, never values); never an error's text.
func refuse(w http.ResponseWriter, status int, reason, detail string) {
	writeJSON(w, status, journalapi.Response{Status: journalapi.StatusRejected, Reason: reason, Detail: detail})
}

func reject(w http.ResponseWriter, rej *record.Rejection) {
	refuse(w, rej.Status, rej.Reason, rej.Detail)
}

// internal logs err for the operator and answers 500 without it.
func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.d.Log.Error("journal request failed", "what", what, "err", err)
	refuse(w, http.StatusInternalServerError, journalapi.ReasonInternal, "")
}
