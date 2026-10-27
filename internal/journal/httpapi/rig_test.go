package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/jmetrics"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
	gloveboxmetrics "github.com/leftathome/glovebox/internal/metrics"
	"github.com/leftathome/glovebox/internal/satoken"
	"github.com/leftathome/glovebox/internal/satoken/satokentest"
)

var T0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// SAScoped is a test-only writer whose write_scopes restrict a part other
// than the source, so the HTTP tests reach MayWrite's scope check (the
// source part is already tied to the type's owner by validation).
const SAScoped = "openclaw/test-scoped-writer"

// Test-only services that separate the read guards (Task 26): one may read
// but not count, one may count but not read, both only as e_parent1; and a
// note writer that may write only on behalf of e_parent2.
const (
	SAReadOnly   = "openclaw/test-read-only"
	SACountsOnly = "openclaw/test-counts-only"
	SANoteWriter = "openclaw/test-note-writer"
)

type rig struct {
	t     *testing.T
	db    *sql.DB
	srv   *httptest.Server
	api   *satokentest.FakeAPI
	j     *journal.Journal
	s     *Server
	ready atomic.Bool
	ros   atomic.Pointer[roster.Roster]
	now   atomic.Pointer[time.Time]
	core  *gloveboxmetrics.Metrics
	logs  *syncBuffer
	scan  *gateScanner
}

// gateScanner is the rig's scanner: the fake scanner, which hold can stop
// inside Verdict until released (to keep a rescan running).
type gateScanner struct {
	*journaltest.FakeScanner
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
	// panicVersion makes Version panic (a rescan reads it first).
	panicVersion atomic.Bool
}

func (g *gateScanner) Version() string {
	if g.panicVersion.Load() {
		panic("scanner version unavailable")
	}
	return g.FakeScanner.Version()
}

func (g *gateScanner) Verdict(text string) string {
	g.mu.Lock()
	gate, entered := g.gate, g.entered
	g.mu.Unlock()
	if gate != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
	}
	return g.FakeScanner.Verdict(text)
}

// hold makes every later Verdict wait until release; entered receives once
// a Verdict is waiting.
func (g *gateScanner) hold() (entered <-chan struct{}, release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gate, in := make(chan struct{}), make(chan struct{}, 1)
	g.gate, g.entered = gate, in
	var once sync.Once
	return in, func() {
		once.Do(func() {
			g.mu.Lock()
			g.gate, g.entered = nil, nil
			g.mu.Unlock()
			close(gate)
		})
	}
}

// syncBuffer is a log sink the server may write to from its goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// token names: the token string is the ServiceAccount's short name.
var tokens = map[string]string{
	"gw": journaltest.SAGateway, "ps": journaltest.SAPowerSchool, "sg": journaltest.SASchoology,
	"rss": journaltest.SARSS, "core": journaltest.SACore, "dash": journaltest.SADashboard,
	"admin": journaltest.SAAdmin, "shared": journaltest.SAShared, "scoped": SAScoped,
	"ro": SAReadOnly, "co": SACountsOnly, "nw": SANoteWriter,
}

func testServices() map[string]policy.Service {
	s := journaltest.Services()
	s[SAScoped] = policy.Service{WriteTypes: []string{"assignment"},
		WriteScopes:   map[string][]string{"source": {"powerschool"}, "course": {"c1"}},
		WriteSubjects: policy.RosterSubjects}
	s[SAReadOnly] = policy.Service{Read: true, Readers: policy.Readers{IDs: []string{"e_parent1"}}}
	s[SACountsOnly] = policy.Service{Counts: true, Readers: policy.Readers{IDs: []string{"e_parent1"}}}
	s[SANoteWriter] = policy.Service{WriteTypes: []string{"attendance_note"}, Readers: policy.Readers{IDs: []string{"e_parent2"}}}
	return s
}

// rigOption adjusts the dependencies before the server is built.
type rigOption func(*Deps)

// newRig is the database-backed rig: it skips without a test database.
func newRig(t *testing.T, opts ...rigOption) *rig {
	t.Helper()
	return buildRig(t, journaltest.NewDB(t), true, opts...)
}

// newAuthRig serves the same API over a journal whose database is never
// reachable: for tests that must not get past authentication and policy,
// so they run without a test database. A request that does reach the
// database gets a 500.
func newAuthRig(t *testing.T, opts ...rigOption) *rig {
	t.Helper()
	db, err := journal.Open("postgres://journal@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return buildRig(t, db, false, opts...)
}

func buildRig(t *testing.T, db *sql.DB, migrate bool, opts ...rigOption) *rig {
	t.Helper()
	r := &rig{t: t, db: db, api: satokentest.New(t), logs: &syncBuffer{}}
	for tok, sa := range tokens {
		ns, name, _ := strings.Cut(sa, "/")
		r.api.Allow(tok, "system:serviceaccount:"+ns+":"+name, journalapi.Audience)
	}
	r.api.Allow("wrong-audience", "system:serviceaccount:openclaw:openclaw-gateway", "glovebox-login-broker-powerschool")
	r.api.Deny("denied", journalapi.Audience)
	r.api.Allow("not-a-serviceaccount", "system:node:node1", journalapi.Audience)
	// A username shaped like an allow-listed caller but without the
	// system:serviceaccount: prefix.
	r.api.Allow("no-prefix", "openclaw:openclaw-gateway", journalapi.Audience)
	t0 := T0
	r.now.Store(&t0)
	now := func() time.Time { return *r.now.Load() }
	reg := journaltest.Registry(t)
	rs, err := rules.Compile(journaltest.Rules(), reg)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(testServices(), reg)
	if err != nil {
		t.Fatal(err)
	}
	r.core, err = gloveboxmetrics.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.core.Shutdown() })
	jm, err := jmetrics.New(r.core.Provider())
	if err != nil {
		t.Fatal(err)
	}
	r.ros.Store(journaltest.Roster(t))
	r.scan = &gateScanner{FakeScanner: journaltest.NewFakeScanner("scanner-1")}
	r.j = journal.New(db, journal.Options{Registry: reg, Rules: rs, Guard: diff.DefaultGuard,
		Scanner: r.scan, Roster: r.ros.Load, Now: now, Metrics: jm})
	if migrate {
		if err := r.j.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := r.j.SyncTypes(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	auth, err := satoken.NewTokenReviewer(r.api.Access(), journalapi.Audience, pol.Callers())
	if err != nil {
		t.Fatal(err)
	}
	r.ready.Store(true)
	d := Deps{Journal: r.j, Registry: reg, Auth: auth, Policy: pol, Roster: r.ros.Load, Metrics: jm,
		Log: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: now, Ready: r.ready.Load, RequestTimeout: 30 * time.Second, Context: context.Background()}
	for _, o := range opts {
		o(&d)
	}
	r.s, err = New(d)
	if err != nil {
		t.Fatal(err)
	}
	h := r.s.Handler()
	if !migrate {
		// The auth rig has no database, and the real healthz reads the
		// schema version from it. Its authentication tests use healthz as a
		// request that passes the real route wrapper (ready, bearer, slot,
		// TokenReview, policy) and then needs nothing else: a stub behind
		// the same wrapper. The real healthz is tested with the database.
		mux := http.NewServeMux()
		mux.Handle("GET "+journalapi.PathHealthz, r.s.route("healthz", func(w http.ResponseWriter, _ *http.Request, _ call) {
			writeJSON(w, http.StatusOK, map[string]string{"status": journalapi.HealthOK})
		}))
		mux.Handle("/", h)
		h = mux
	}
	r.srv = httptest.NewServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// setNow moves the clock of the journal and the server.
func (r *rig) setNow(t time.Time) { r.now.Store(&t) }

// do sends a request as token and decodes a JSON answer into out (if set).
func (r *rig) do(method, path, token string, body any, out any) int {
	r.t.Helper()
	code, raw := r.send(method, path, token, "application/json", body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			r.t.Fatalf("%s %s: %d %s", method, path, code, raw)
		}
	}
	return code
}

// send is do with an explicit Content-Type ("" sends none), returning the raw
// answer.
func (r *rig) send(method, path, token, contentType string, body any) (int, []byte) {
	r.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, rd)
	if rd != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// scrape returns the core's /metrics page.
func (r *rig) scrape() string {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.core.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func uid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func assignments(n int, subject, source string, observed time.Time, recs ...journalapi.Record) journalapi.Snapshot {
	return journalapi.Snapshot{SnapshotID: uid(n), RecordType: "assignment", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", source}, {"surface", "assignments"}, {"data_subject", subject}, {"course", "c1"}},
		Complete: true, ObservedAt: observed, Records: recs}
}

func one(key, subject, fields string) journalapi.Record {
	var f map[string]json.RawMessage
	_ = json.Unmarshal([]byte(fields), &f)
	return journalapi.Record{Key: key, DataSubject: subject, Fields: f}
}
