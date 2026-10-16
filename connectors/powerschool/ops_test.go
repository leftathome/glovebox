package powerschool

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
	"github.com/prometheus/client_golang/prometheus"
)

// --- health while waiting for a session ---------------------------------

func httpStatus(t *testing.T, url string) int {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestWaitHealth_AliveAndReadyThenPortFree: while the binary waits for a
// session it answers the kubelet -- alive AND ready, because waiting is
// what it should be doing and readiness gates every Helm upgrade and
// rollback of the release (glovebox-wekc.17) -- and when the wait ends the
// port is free again for the framework's own health server.
func TestWaitHealth_AliveAndReadyThenPortFree(t *testing.T) {
	h, err := StartWaitHealth("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("StartWaitHealth: %v", err)
	}
	addr := h.Addr()

	if got := httpStatus(t, "http://"+addr+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 (the pod must not be killed for waiting)", got)
	}
	if got := httpStatus(t, "http://"+addr+"/readyz"); got != http.StatusOK {
		t.Errorf("/readyz = %d, want 200 (waiting is not a fault; a 503 here stalls helm upgrade and rollback)", got)
	}
	// A second server on the same port must fail while it is up...
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		t.Fatal("the port was free while the wait server was running")
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// ...and succeed immediately after Stop returns: this is the hand-over
	// to connector.Run, which binds the same port.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s still in use after Stop: %v", addr, err)
	}
	framework := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = framework.Serve(ln) }()
	defer framework.Close()
	if got := httpStatus(t, "http://"+addr+"/readyz"); got != http.StatusOK {
		t.Errorf("the next server on the port answered %d, want its own 200", got)
	}

	if err := h.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	var none *WaitHealth
	if err := none.Stop(); err != nil || none.Addr() != "" {
		t.Errorf("nil WaitHealth: Stop=%v Addr=%q", err, none.Addr())
	}
}

func TestStartWaitHealth_PortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if h, err := StartWaitHealth(ln.Addr().String(), nil); err == nil {
		h.Stop()
		t.Fatal("StartWaitHealth succeeded on a port that is in use")
	}
}

// TestWaitForFreshSession_ServesHealthWhileWaiting wires the wait the way
// main.go does: the stand-in health server comes up when (and only when)
// the wait begins, a Secret update written to the mounted file ends the
// wait without a restart, and the port is free afterwards.
func TestWaitForFreshSession_ServesHealthWhileWaiting(t *testing.T) {
	captureLogs(t)
	sessionFile := filepath.Join(t.TempDir(), "session.json")
	writeSession(t, sessionFile, "synthetic-dead")
	getenv := func(k string) string {
		if k == "POWERSCHOOL_SESSION_FILE" {
			return sessionFile
		}
		return ""
	}
	load := func() (*pslib.SessionExport, error) { return LoadSession(getenv, time.Now()) }
	dead, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rejected := connector.NewRejectedSession(t.TempDir())
	if err := rejected.Mark(SessionFingerprint(dead)); err != nil {
		t.Fatal(err)
	}

	// As main.go wires it: one Status and one SessionState, on the default
	// registry the stand-in's /metrics serves. (The only test in this
	// package that registers there.)
	status := connector.NewStatus()
	state, err := connector.NewSessionState(prometheus.DefaultRegisterer, "powerschool", status)
	if err != nil {
		t.Fatalf("NewSessionState: %v", err)
	}
	refusedAt, ok := rejected.MarkedAt()
	if !ok {
		t.Fatal("no marker time")
	}

	var mu sync.Mutex
	var health *WaitHealth
	started := make(chan string, 1)
	type result struct {
		s   *pslib.SessionExport
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := WaitForFreshSession(context.Background(), rejected, load, 2*time.Millisecond, func() {
			since, _ := rejected.MarkedAt()
			state.Set(connector.SessionStateWaiting, since, "waiting")
			h, err := StartWaitHealth("127.0.0.1:0", status)
			if err != nil {
				t.Errorf("StartWaitHealth: %v", err)
				return
			}
			mu.Lock()
			health = h
			mu.Unlock()
			started <- h.Addr()
		})
		done <- result{s, err}
	}()

	var addr string
	select {
	case addr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not start the health server")
	}
	if got := httpStatus(t, "http://"+addr+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz while waiting = %d, want 200", got)
	}
	if got := httpStatus(t, "http://"+addr+"/readyz"); got != http.StatusOK {
		t.Errorf("/readyz while waiting = %d, want 200: waiting is healthy, and a NotReady pod stalls helm upgrade and rollback", got)
	}
	if body := httpBody(t, "http://"+addr+"/readyz"); !strings.Contains(body, "degraded: session=waiting_for_new_session") {
		t.Errorf("/readyz body %q does not name the degraded session", body)
	}
	var doc struct {
		Connector  string                         `json:"connector"`
		Ready      bool                           `json:"ready"`
		Degraded   bool                           `json:"degraded"`
		Conditions map[string]connector.Condition `json:"conditions"`
	}
	if err := json.Unmarshal([]byte(httpBody(t, "http://"+addr+"/statusz")), &doc); err != nil {
		t.Fatalf("/statusz is not JSON: %v", err)
	}
	cond := doc.Conditions["session"]
	if doc.Connector != "powerschool" || !doc.Ready || !doc.Degraded ||
		cond.State != connector.SessionStateWaiting || !cond.Degraded ||
		!cond.Since.Equal(refusedAt.UTC().Truncate(0)) {
		t.Errorf("/statusz = %+v, want ready, degraded, session waiting since the marker (%s)", doc, refusedAt.UTC())
	}
	metrics := httpBody(t, "http://"+addr+"/metrics")
	for _, want := range []string{
		`powerschool_session_state{state="waiting_for_new_session"} 1`,
		`powerschool_session_state{state="usable"} 0`,
		`powerschool_session_state{state="unknown"} 0`,
		`powerschool_session_state_since_timestamp_seconds{state="waiting_for_new_session"} `,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics while waiting lacks %q", want)
		}
	}

	// The Secret is updated in place.
	writeSession(t, sessionFile, "synthetic-fresh")
	select {
	case r := <-done:
		if r.err != nil || r.s.Cookies[0].Value != "synthetic-fresh" {
			t.Fatalf("wait ended with (%+v, %v), want the fresh session", r.s, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end when the session file changed")
	}

	mu.Lock()
	h := health
	mu.Unlock()
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("health port still in use after the wait: %v", err)
	}
	ln.Close()
}

func httpBody(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}

// --- session delivery ---------------------------------------------------

// TestLoadSession_FileIsPreferredAndReRead: the mounted file wins over the
// environment variable, and each load reads it afresh.
func TestLoadSession_FileIsPreferredAndReRead(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "session.json")
	writeSession(t, sessionFile, "synthetic-from-file")
	envDoc := syntheticSession(t, time.Now().Add(time.Hour), &http.Cookie{Name: "JSESSIONID", Value: "synthetic-from-env"})
	getenv := func(k string) string {
		switch k {
		case "POWERSCHOOL_SESSION_FILE":
			return sessionFile
		case "POWERSCHOOL_SESSION":
			return envDoc
		}
		return ""
	}

	s, err := LoadSession(getenv, time.Now())
	if err != nil || s.Cookies[0].Value != "synthetic-from-file" {
		t.Fatalf("LoadSession = (%+v, %v), want the file's session", s, err)
	}
	writeSession(t, sessionFile, "synthetic-rotated")
	s, err = LoadSession(getenv, time.Now())
	if err != nil || s.Cookies[0].Value != "synthetic-rotated" {
		t.Fatalf("LoadSession after an in-place update = (%+v, %v), want the rotated session", s, err)
	}
	// A configured file that is missing is an error, not a silent fall
	// back to the environment.
	if err := os.Remove(sessionFile); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(getenv, time.Now()); err == nil {
		t.Error("a missing session file fell back to the environment variable")
	}
}

// --- how long did the session last --------------------------------------

func TestSessionClock(t *testing.T) {
	dir := t.TempDir()
	clock := SessionClock{Dir: dir}
	t0 := time.Date(2026, 10, 5, 6, 10, 0, 0, time.UTC)

	if got := clock.FirstSeen("fp-a", t0); !got.Equal(t0) {
		t.Fatalf("first sighting = %s, want %s", got, t0)
	}
	// A later process, same session: the original time.
	if got := (SessionClock{Dir: dir}).FirstSeen("fp-a", t0.Add(30*time.Hour)); !got.Equal(t0) {
		t.Errorf("second sighting = %s, want the first-seen time %s", got, t0)
	}
	// A new session resets it.
	t1 := t0.Add(48 * time.Hour)
	if got := clock.FirstSeen("fp-b", t1); !got.Equal(t1) {
		t.Errorf("new session first seen = %s, want %s", got, t1)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "session-first-seen"))
	if err != nil || !strings.HasPrefix(string(raw), "fp-b ") {
		t.Errorf("first-seen file = %q, %v", raw, err)
	}

	if _, ok := clock.LastRejectedAge(); ok {
		t.Error("a rejected age is reported before any was saved")
	}
	if err := clock.SaveRejectedAge(31*time.Hour + 20*time.Second); err != nil {
		t.Fatalf("SaveRejectedAge: %v", err)
	}
	if got, ok := (SessionClock{Dir: dir}).LastRejectedAge(); !ok || got != 31*time.Hour+20*time.Second {
		t.Errorf("LastRejectedAge = (%s, %v)", got, ok)
	}
	// Unknown ages are not recorded.
	if err := clock.SaveRejectedAge(-1); err != nil {
		t.Fatal(err)
	}
	if got, _ := clock.LastRejectedAge(); got != 31*time.Hour+20*time.Second {
		t.Errorf("an unknown age overwrote the record: %s", got)
	}

	var off SessionClock
	now := time.Now()
	if got := off.FirstSeen("fp", now); !got.Equal(now) {
		t.Error("disabled clock did not return now")
	}
	if _, ok := off.LastRejectedAge(); ok || off.SaveRejectedAge(time.Hour) != nil {
		t.Error("disabled clock recorded something")
	}
}

// TestPoll_RejectedSessionAgeIsRecorded: at the moment a session is first
// rejected, its age goes to a gauge, to one INFO line with no identifiers,
// and to the callback that persists it.
func TestPoll_RejectedSessionAgeIsRecorded(t *testing.T) {
	client := pollFixture("111111")
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
	c, cp, _ := newWiredConnector(t, client, testConfig())
	rec := recordMetrics(t, c)
	logs := captureLogs(t)

	loaded := time.Date(2026, 10, 5, 6, 10, 0, 0, time.UTC)
	c.SessionFirstSeen = loaded
	c.now = func() time.Time { return loaded.Add(34*time.Hour + 30*time.Minute) }
	var reported []time.Duration
	c.OnSessionRejected = func(age time.Duration) { reported = append(reported, age) }

	if err := c.Poll(context.Background(), cp); !connector.IsPermanent(err) {
		t.Fatalf("Poll = %v, want a PermanentError", err)
	}
	want := 34*time.Hour + 30*time.Minute
	if len(reported) != 1 || reported[0] != want {
		t.Errorf("OnSessionRejected ages = %v, want [%s]", reported, want)
	}
	if got := rec.counters()["powerschool_rejected_session_age_seconds{}"]; got != int64(want/time.Second) {
		t.Errorf("powerschool_rejected_session_age_seconds = %d, want %d", got, int64(want/time.Second))
	}
	var ageLines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "session_age_seconds=") {
			ageLines = append(ageLines, line)
		}
	}
	if len(ageLines) != 1 || !strings.Contains(ageLines[0], "level=INFO") ||
		!strings.Contains(ageLines[0], "session_age_seconds=124200") {
		t.Fatalf("session age log lines = %q, want exactly one INFO line with session_age_seconds=124200", ageLines)
	}
	for _, id := range []string{"111111", "111112", "9000001", "9000002", "999001", "999002", "JSESSIONID", "k1", "k2"} {
		if strings.Contains(ageLines[0], id) {
			t.Errorf("session age line carries an identifier (%s): %s", id, ageLines[0])
		}
	}
}

func TestPoll_RejectedSessionAgeUnknown(t *testing.T) {
	client := pollFixture("111111")
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
	c, cp, _ := newWiredConnector(t, client, testConfig())
	rec := recordMetrics(t, c)
	logs := captureLogs(t)
	var reported []time.Duration
	c.OnSessionRejected = func(age time.Duration) { reported = append(reported, age) }

	if err := c.Poll(context.Background(), cp); !connector.IsPermanent(err) {
		t.Fatalf("Poll = %v, want a PermanentError", err)
	}
	if len(reported) != 1 || reported[0] >= 0 {
		t.Errorf("ages = %v, want one negative (unknown) age", reported)
	}
	if _, ok := rec.counters()["powerschool_rejected_session_age_seconds{}"]; ok {
		t.Error("an age was exported although none is known")
	}
	if strings.Contains(logs.String(), "session_age_seconds") {
		t.Error("an age was logged although none is known")
	}
}

// TestWire_ReexportsThePreviousRejectedAge: the process exits right after a
// rejection, usually before a scrape; the next process exports the figure.
func TestWire_ReexportsThePreviousRejectedAge(t *testing.T) {
	base, err := connector.NewMetrics("powerschool-prev-age-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = base.Shutdown() })
	w, err := connector.NewStagingWriter(t.TempDir(), "powerschool")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	c, err := NewConnector(pollFixture("111111"), cfg, "v0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	c.PreviousRejectedAge = 7654 * time.Second
	if err := c.Wire(connector.ConnectorContext{Backend: w, Matcher: connector.NewRuleMatcher(cfg.Rules), Metrics: base}); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	rr := httptest.NewRecorder()
	base.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rr.Body)
	found := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "powerschool_rejected_session_age_seconds") && strings.HasSuffix(strings.TrimSpace(line), " 7654") {
			found = true
		}
	}
	if !found {
		t.Errorf("/metrics does not export powerschool_rejected_session_age_seconds 7654")
	}
}

// --- staging over HTTP ingest -------------------------------------------

// In-cluster the framework delivers over HTTP ingest (GLOVEBOX_INGEST_URL):
// ConnectorContext.Backend is an HTTP backend and the deprecated filesystem
// Writer is nil. This wires the connector exactly that way and checks the
// items reach the ingest endpoint.
func TestConnector_WiresAndStagesOverHTTPIngest(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/ingest" {
			bodies = append(bodies, string(b))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	captureLogs(t)

	cfg := oneKidConfig()
	c, err := NewConnector(pollFixture("111111"), cfg, "v0.0.0-test")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	metrics, err := connector.NewMetrics("powerschool-http-backend-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })

	err = c.Wire(connector.ConnectorContext{
		// Writer deliberately nil: this is the HTTP-ingest shape.
		Backend: connector.NewHTTPStagingBackend(srv.URL+"/v1/ingest", "powerschool", srv.Client()),
		Matcher: connector.NewRuleMatcher(cfg.Rules),
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("Wire with Backend only (HTTP ingest mode): %v", err)
	}
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("ingest endpoint received %d POSTs, want 3 (grade, attendance, assignment)", len(bodies))
	}
	all := strings.Join(bodies, "\n")
	for _, want := range []string{"e_333333", "guardians", "Lab report", "Example Science 7", "B+88"} {
		if !strings.Contains(all, want) {
			t.Errorf("ingest payloads are missing %q", want)
		}
	}
	// Delivered means checkpointed.
	for _, key := range fingerprintKeys("k1") {
		if _, ok := cp.Load(key); !ok {
			t.Errorf("checkpoint %q missing after delivery over HTTP", key)
		}
	}
}

// --- scheduler edge -----------------------------------------------------

// zeroSource is a rand.Source that always returns 0, forcing every splay to
// the first second of its window.
type zeroSource struct{}

func (zeroSource) Int63() int64 { return 0 }
func (zeroSource) Seed(int64)   {}

// TestScheduler_AdjacentWindowsZeroSplay: two windows that touch, and a
// splay of zero. After the first window's poll the next one is scheduled
// from that window's end, which IS the second window's start and, with a
// zero splay, its scheduled time. A strict "after now" comparison skipped
// the second window every day; an inclusive one must not make the first
// window repeat either.
func TestScheduler_AdjacentWindowsZeroSplay(t *testing.T) {
	tz := losAngeles(t)
	cfg := threeWindowConfig()
	cfg.PollSchedule.Windows = []PollWindow{
		{Start: "07:00", End: "07:30"},
		{Start: "07:30", End: "08:00"},
		{Start: "08:00", End: "08:30"},
	}
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, tz) // a Monday
	end := start.AddDate(0, 0, 7)

	// The arithmetic alone.
	rng := rand.New(zeroSource{})
	perDay, perWindow := map[string]int{}, map[string]int{}
	now := start
	for {
		next, _ := computeNextPollTime(cfg, now, tz, rng)
		if !next.Before(end) {
			break
		}
		we := windowEnd(cfg, next, tz)
		if we.Equal(next) || next.Before(now) {
			t.Fatalf("bad schedule: next %s from %s", next, now)
		}
		perDay[next.Format("2006-01-02")]++
		perWindow[we.Format(time.RFC3339)]++
		now = we
	}
	assertWeek(t, 0, perDay, perWindow, 3)

	// And through the connector's own scheduling state, as Watch uses it.
	c, _, _ := newWiredConnector(t, pollFixture("111111"), cfg)
	c.rng = rand.New(zeroSource{})
	perDay, perWindow = map[string]int{}, map[string]int{}
	now = start
	for {
		next := c.nextPoll(c.scheduleFrom(now))
		if !next.Before(end) {
			break
		}
		now = next
		c.deferUntil(windowEnd(cfg, next, tz))
		perDay[next.Format("2006-01-02")]++
		perWindow[windowEnd(cfg, next, tz).Format(time.RFC3339)]++
	}
	assertWeek(t, 0, perDay, perWindow, 3)
}

// TestSampleSchedule_AvoidsTheOwnersHours: the shipped default windows stay
// outside 16:00-21:00 local, when the account's owner may be signed in.
func TestSampleSchedule_AvoidsTheOwnersHours(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	if !cfg.PollSchedule.WeekdaysOnly || len(cfg.PollSchedule.Windows) != 3 {
		t.Fatalf("sample schedule = %+v, want three weekday windows", cfg.PollSchedule)
	}
	for _, w := range cfg.PollSchedule.Windows {
		startMin, err1 := parseTimeOfDay(w.Start)
		endMin, err2 := parseTimeOfDay(w.End)
		if err1 != nil || err2 != nil {
			t.Fatalf("window %+v does not parse", w)
		}
		if startMin < 21*60 && endMin > 16*60 {
			t.Errorf("window %s-%s overlaps 16:00-21:00", w.Start, w.End)
		}
	}
}
