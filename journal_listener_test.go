package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/journal/jmetrics"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/satoken/satokentest"
	"github.com/leftathome/glovebox/internal/scan"
)

// testProvider is a meter provider on a registry of its own, so a scrape
// shows exactly what this test registered.
func testProvider(t *testing.T) (*sdkmetric.MeterProvider, func() string) {
	t.Helper()
	reg := promclient.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return mp, func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
}

// metricValue returns the value of the first sample of name whose labels
// contain every fragment, and whether there is one.
func metricValue(text, name string, fragments ...string) (float64, bool) {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `(\{[^}]*\})? (\S+)$`)
	for _, line := range strings.Split(text, "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ok := true
		for _, f := range fragments {
			ok = ok && strings.Contains(m[1], f)
		}
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		return v, err == nil
	}
	return 0, false
}

var (
	testScannerOnce sync.Once
	testScannerVal  *scan.Scanner
	testRulesSHA    string
	testScannerErr  error
)

// testScanner is the core's scanner over the default rules.
func testScanner(t *testing.T) (*scan.Scanner, string) {
	t.Helper()
	testScannerOnce.Do(func() {
		rules, prov, err := engine.LoadRulesFileWithPolicy("configs/default-rules.json", engine.SignaturePolicy{})
		if err != nil {
			testScannerErr = err
			return
		}
		testRulesSHA = prov.SHA256
		testScannerVal, testScannerErr = scan.New(rules, detector.NewDefaultRegistry())
	})
	if testScannerErr != nil {
		t.Fatal(testScannerErr)
	}
	return testScannerVal, testRulesSHA
}

// closedPort is a local port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testJournalConfig is an enabled journal on a random port with the shared
// fixtures, its database at dbURL, its roster at rosterPath.
func testJournalConfig(t *testing.T, dbURL, rosterPath string) config.JournalConfig {
	t.Helper()
	cfg, err := config.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	jc := cfg.Journal
	jc.Enabled = true
	jc.Port = 0
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pw, _ := u.User.Password()
	if pw == "" {
		pw = "unused"
	}
	port, _ := strconv.Atoi(u.Port())
	jc.Database = config.JournalDatabaseConfig{
		Host: u.Hostname(), Port: port, Name: strings.TrimPrefix(u.Path, "/"),
		SSLMode:      u.Query().Get("sslmode"),
		UserFile:     writeFile(t, filepath.Join(dir, "user"), u.User.Username()+"\n"),
		PasswordFile: writeFile(t, filepath.Join(dir, "password"), pw+"\n"),
	}
	jc.Roster.Path = rosterPath
	jc.Roster.ReloadSeconds = 1
	jc.Services = json.RawMessage(`{
		"` + journaltest.SAGateway + `": {"read": true, "readers": "*", "counts": true, "write_types": ["attendance_note"]},
		"` + journaltest.SAPowerSchool + `": {"write_types": ["assignment", "attendance_day", "term_comment"],
			"write_scopes": {"source": ["powerschool"]}, "write_subjects": "roster_subjects"},
		"` + journaltest.SAAdmin + `": {"admin": true}}`)
	jc.RecordTypes = journaltest.Types()
	jc.Rules = journaltest.Rules()
	jc.StalenessPrefixes = []string{"source=powerschool/", "source=schoology/"}
	return jc
}

// unreachableDB names a database nobody serves.
func unreachableDB(t *testing.T) string {
	return "postgres://journal_app:pw@127.0.0.1:" + strconv.Itoa(closedPort(t)) + "/glovebox_journal?sslmode=disable"
}

func rosterFile(t *testing.T) string {
	return writeFile(t, filepath.Join(t.TempDir(), "household.json"), journaltest.HouseholdV1)
}

func fakeAccess(t *testing.T) (*satokentest.FakeAPI, func() (*kubecm.Access, error)) {
	f := satokentest.New(t)
	return f, func() (*kubecm.Access, error) { return f.Access(), nil }
}

// journalGet sends a GET to the journal, with a bearer token when token is set.
func journalGet(t *testing.T, rt *journalRuntime, path, token string) (int, journalapi.Response) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+rt.Addr().String()+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ref journalapi.Response
	_ = json.NewDecoder(resp.Body).Decode(&ref)
	return resp.StatusCode, ref
}

func shutdown(t *testing.T, rt *journalRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// settleGoroutines waits for the goroutine count to fall to at most n.
func settleGoroutines(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		http.DefaultClient.CloseIdleConnections()
		got := runtime.NumGoroutine()
		if got <= n {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d after shutdown, %d before\n%s", got, n, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStartJournalDisabledStartsNothing(t *testing.T) {
	mp, scrape := testProvider(t)
	cfg, _ := config.LoadConfig("")
	if cfg.Journal.Enabled {
		t.Fatal("the journal must be off by default")
	}
	// Everything else set as for an enabled journal: still nothing happens.
	jc := testJournalConfig(t, unreachableDB(t), rosterFile(t))
	jc.Enabled = false
	asked := false
	sc, sha := testScanner(t)
	rt, err := startJournal(jc, sc, sha, mp, func() (*kubecm.Access, error) {
		asked = true
		return nil, errors.New("no")
	})
	if rt != nil || err != nil {
		t.Fatalf("disabled journal: %v %v", rt, err)
	}
	if asked {
		t.Error("a disabled journal asked for Kubernetes access")
	}
	if out := scrape(); strings.Contains(out, "glovebox_journal_") {
		t.Errorf("a disabled journal exports journal metrics:\n%s", out)
	}
	if err := rt.Shutdown(context.Background()); err != nil {
		t.Errorf("nil runtime shutdown: %v", err)
	}
}

func TestStartJournalOutsideAPodRefuses(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	mp, scrape := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, unreachableDB(t), rosterFile(t))
	_, err := startJournal(jc, sc, sha, mp, inClusterAccess)
	if err == nil || !strings.Contains(err.Error(), "the Kubernetes API is needed for TokenReview") {
		t.Errorf("enabled journal outside a pod: %v", err)
	}
	if out := scrape(); strings.Contains(out, "glovebox_journal_") {
		t.Errorf("a refused journal registered metrics:\n%s", out)
	}
}

func TestStartJournalRefusesAPolicyNobodyPasses(t *testing.T) {
	mp, _ := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, unreachableDB(t), rosterFile(t))
	jc.Services = nil
	_, access := fakeAccess(t)
	if _, err := startJournal(jc, sc, sha, mp, access); err == nil || !strings.Contains(err.Error(), "allow-list") {
		t.Errorf("empty policy: %v", err)
	}
}

func TestScannerVersion(t *testing.T) {
	if got := scannerVersion("v1.2.3", "0123456789abcdef0123"); got != "v1.2.3+rules.0123456789ab" {
		t.Errorf("scannerVersion = %s", got)
	}
	if got := scannerVersion("core-scan-1", "0123456789a"); got != "core-scan-1+rules.unknown" {
		t.Errorf("scannerVersion = %s", got)
	}
	if got := scannerVersion("dev", ""); got != "dev+rules.unknown" {
		t.Errorf("scannerVersion = %s", got)
	}
}

// The journal starts with its database unreachable: no error, the listener
// is up and answers 503 unavailable, ready is 0, it keeps retrying, and it
// shuts down cleanly without leaking goroutines.
func TestStartJournalWithUnreachableDatabase(t *testing.T) {
	mp, scrape := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, unreachableDB(t), rosterFile(t))
	_, access := fakeAccess(t)
	before := runtime.NumGoroutine()
	start := time.Now()
	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("startJournal blocked for %v", d)
	}
	if want := jc.ScannerVersion + "+rules." + sha[:12]; rt.scannerVersion != want {
		t.Errorf("scanner version %q, want %q", rt.scannerVersion, want)
	}
	for _, path := range []string{journalapi.PathHealthz, journalapi.PathState + "?reader=e_parent1"} {
		if code, ref := journalGet(t, rt, path, "any-token"); code != http.StatusServiceUnavailable || ref.Reason != journalapi.ReasonUnavailable {
			t.Errorf("%s while not ready: %d %+v", path, code, ref)
		}
	}
	// Still not ready after a few retries; the roster did load.
	time.Sleep(2500 * time.Millisecond)
	out := scrape()
	if v, ok := metricValue(out, "glovebox_journal_ready"); !ok || v != 0 {
		t.Errorf("ready gauge %v %v\n%s", v, ok, out)
	}
	if _, ok := metricValue(out, "glovebox_journal_roster_info", `sha256="`); !ok {
		t.Errorf("roster_info missing\n%s", out)
	}
	if v, ok := metricValue(out, "glovebox_journal_roster_reload_failing"); !ok || v != 0 {
		t.Errorf("roster_reload_failing %v %v", v, ok)
	}
	if rt.ready.Load() {
		t.Error("ready with no database")
	}
	if code, _ := journalGet(t, rt, journalapi.PathHealthz, ""); code != http.StatusServiceUnavailable {
		t.Errorf("still not ready: %d", code)
	}
	shutdown(t, rt)
	if _, err := net.Dial("tcp", rt.Addr().String()); err == nil {
		t.Error("the listener is still open after shutdown")
	}
	settleGoroutines(t, before)
}

// A missing roster: not ready, 503, and the reload-failing gauge (sourced
// from the holder) is 1 for as long as the file is missing.
func TestStartJournalWithMissingRoster(t *testing.T) {
	mp, scrape := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, unreachableDB(t), filepath.Join(t.TempDir(), "absent.json"))
	_, access := fakeAccess(t)
	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, rt)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := scrape()
		f, _ := metricValue(out, "glovebox_journal_roster_reload_failing")
		e, _ := metricValue(out, "glovebox_journal_roster_reload_errors_total")
		if f == 1 && e >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("roster failure not reported:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, ref := journalGet(t, rt, journalapi.PathHealthz, "t"); code != http.StatusServiceUnavailable || ref.Reason != journalapi.ReasonUnavailable {
		t.Errorf("no roster: %d %+v", code, ref)
	}
	if _, ok := metricValue(scrape(), "glovebox_journal_roster_info"); ok {
		t.Error("roster_info without a roster")
	}
}

func TestStartJournalPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mp, _ := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, unreachableDB(t), rosterFile(t))
	jc.Port = ln.Addr().(*net.TCPAddr).Port
	_, access := fakeAccess(t)
	if _, err := startJournal(jc, sc, sha, mp, access); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("port in use: %v", err)
	}
}

// --- the ready loop --------------------------------------------------------

type readyFake struct {
	mu                      sync.Mutex
	calls                   []string
	rosterErr, migErr, sErr []error // consumed one per attempt; empty: success
	warmGate                chan struct{}
	readyCalls              atomic.Int32
	readyCh                 chan struct{}
}

func newReadyFake() *readyFake { return &readyFake{readyCh: make(chan struct{}, 1)} }

func (f *readyFake) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func pop(f *readyFake, q *[]error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(*q) == 0 {
		return nil
	}
	err := (*q)[0]
	*q = (*q)[1:]
	return err
}

func (f *readyFake) steps() readySteps {
	return readySteps{
		warm: func() {
			f.note("warm")
			if f.warmGate != nil {
				<-f.warmGate
			}
		},
		roster:  func() error { f.note("roster"); return pop(f, &f.rosterErr) },
		migrate: func(context.Context) error { f.note("migrate"); return pop(f, &f.migErr) },
		sync:    func(context.Context) error { f.note("sync"); return pop(f, &f.sErr) },
		ready: func() {
			f.note("ready")
			f.readyCalls.Add(1)
			f.readyCh <- struct{}{}
		},
	}
}

func (f *readyFake) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func TestReadyLoopWarmsTheScannerBeforeReady(t *testing.T) {
	f := newReadyFake()
	f.warmGate = make(chan struct{})
	done := make(chan struct{})
	go func() {
		journalReadyLoop(context.Background(), f.steps(), time.Millisecond, time.Millisecond)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if f.readyCalls.Load() != 0 || slices.Contains(f.log(), "roster") {
		t.Fatalf("went on before the warm-up finished: %v", f.log())
	}
	close(f.warmGate)
	<-done
	if got := f.log(); !slices.Equal(got, []string{"warm", "roster", "migrate", "sync", "ready"}) {
		t.Errorf("order %v", got)
	}
}

func TestReadyLoopRetriesUntilEveryStepSucceeds(t *testing.T) {
	f := newReadyFake()
	boom := errors.New("boom")
	f.rosterErr = []error{boom}
	f.migErr = []error{boom}
	f.sErr = []error{boom, boom}
	journalReadyLoop(context.Background(), f.steps(), time.Millisecond, 2*time.Millisecond)
	want := []string{"warm", "roster", "roster", "migrate", "roster", "migrate", "sync", "roster", "migrate", "sync",
		"roster", "migrate", "sync", "ready"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Errorf("calls %v\nwant %v", got, want)
	}
	if f.readyCalls.Load() != 1 {
		t.Errorf("ready %d times", f.readyCalls.Load())
	}
}

func TestReadyLoopRecoversAPanickingStep(t *testing.T) {
	f := newReadyFake()
	st := f.steps()
	var n atomic.Int32
	st.migrate = func(context.Context) error {
		if n.Add(1) == 1 {
			panic("migrate exploded")
		}
		return nil
	}
	st.warm = func() { panic("warm exploded") }
	journalReadyLoop(context.Background(), st, time.Millisecond, time.Millisecond)
	if f.readyCalls.Load() != 1 || n.Load() != 2 {
		t.Errorf("ready %d, migrate attempts %d", f.readyCalls.Load(), n.Load())
	}
}

func TestReadyLoopEndsWithItsContext(t *testing.T) {
	f := newReadyFake()
	for range 1000 {
		f.migErr = append(f.migErr, errors.New("down"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { journalReadyLoop(ctx, f.steps(), time.Hour, time.Hour); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the ready loop outlived its context")
	}
	if f.readyCalls.Load() != 0 {
		t.Error("ready while the database was down")
	}
}

func TestReadyLoopBacksOff(t *testing.T) {
	f := newReadyFake()
	for range 4 {
		f.migErr = append(f.migErr, errors.New("down"))
	}
	start := time.Now()
	journalReadyLoop(context.Background(), f.steps(), 40*time.Millisecond, 80*time.Millisecond)
	// 40 + 80 + 80 + 80 (capped)
	if d := time.Since(start); d < 280*time.Millisecond || d > 2*time.Second {
		t.Errorf("4 retries took %v, want about 280ms", d)
	}
}

// --- shutdown order ----------------------------------------------------------

func TestJournalShutdownOrder(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	note := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }
	cancelled := make(chan struct{})
	err := journalShutdown(context.Background(), shutdownSteps{
		stopListener: func(context.Context) error {
			select {
			case <-cancelled:
				t.Error("context cancelled before the listener stopped")
			default:
			}
			note("listener")
			return nil
		},
		cancel: func() { note("cancel"); close(cancelled) },
		drain: func() {
			// A rescan ends only once the context is cancelled.
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Error("drain waited on a rescan nobody cancelled")
			}
			note("drain")
		},
		closeDB: func() error { note("close"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"listener", "cancel", "drain", "close"}) {
		t.Errorf("order %v", calls)
	}
}

func TestJournalShutdownLeavesTheDatabaseOpenWhileWorkRuns(t *testing.T) {
	closed := false
	stuck := make(chan struct{})
	defer close(stuck)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := journalShutdown(ctx, shutdownSteps{
		stopListener: func(context.Context) error { return nil },
		cancel:       func() {},
		drain:        func() { <-stuck },
		closeDB:      func() error { closed = true; return nil },
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v", err)
	}
	if closed {
		t.Error("closed the database under running work")
	}
}

func TestJournalShutdownReportsAListenerError(t *testing.T) {
	boom := errors.New("listener")
	closed := false
	err := journalShutdown(context.Background(), shutdownSteps{
		stopListener: func(context.Context) error { return boom },
		cancel:       func() {},
		drain:        func() {},
		closeDB:      func() error { closed = true; return nil },
	})
	if !errors.Is(err, boom) || !closed {
		t.Errorf("err %v closed %v", err, closed)
	}
}

// --- gauges ------------------------------------------------------------------

func TestGaugeLoopRefreshesNowAndOnEveryTick(t *testing.T) {
	mp, scrape := testProvider(t)
	jm, err := jmetrics.New(mp)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var gotPrefixes atomic.Value
	t0 := time.Unix(1_800_000_000, 0)
	gauges := func(_ context.Context, prefixes []string) (int64, map[string]time.Time, error) {
		n := calls.Add(1)
		gotPrefixes.Store(prefixes)
		if n == 2 {
			return 0, nil, errors.New("database went away")
		}
		return int64(n), map[string]time.Time{"source=powerschool/": t0.Add(time.Duration(n) * time.Second)}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		journalGaugeLoop(ctx, time.Hour, gauges, []string{"source=powerschool/"}, jm)
		close(done)
	}()
	waitFor(t, func() bool { return calls.Load() >= 1 })
	out := scrape()
	if v, _ := metricValue(out, "glovebox_journal_held_snapshots"); v != 1 {
		t.Errorf("held %v\n%s", v, out)
	}
	if v, _ := metricValue(out, "glovebox_journal_last_snapshot_timestamp_seconds", `prefix="source=powerschool/"`); v != float64(t0.Unix()+1) {
		t.Errorf("last snapshot %v\n%s", v, out)
	}
	if p, _ := gotPrefixes.Load().([]string); !slices.Equal(p, []string{"source=powerschool/"}) {
		t.Errorf("prefixes %v", p)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the gauge loop outlived its context")
	}

	// Ticks: an error keeps the previous values; the next refresh wins.
	calls.Store(0)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go journalGaugeLoop(ctx, 20*time.Millisecond, gauges, []string{"source=powerschool/"}, jm)
	waitFor(t, func() bool { return calls.Load() >= 3 })
	cancel()
	if v, _ := metricValue(scrape(), "glovebox_journal_held_snapshots"); v < 3 {
		t.Errorf("held %v after three refreshes", v)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Shutdown returns nil only once every journal goroutine has ended; until
// then the database stays open. Here the scanner warm-up is held.
func TestShutdownWaitsForTheLoops(t *testing.T) {
	mp, _ := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, unreachableDB(t), rosterFile(t))
	_, access := fakeAccess(t)
	gate := make(chan struct{})
	entered := make(chan struct{})
	orig := warmScanner
	warmScanner = func(*scan.Scanner) { close(entered); <-gate }
	defer func() { warmScanner = orig }()
	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := rt.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shutdown with the warm-up still running: %v", err)
	}
	if _, err := rt.j.SchemaVersion(context.Background()); err != nil && strings.Contains(err.Error(), "database is closed") {
		t.Error("the database was closed under a running loop")
	}
	close(gate)
	shutdown(t, rt)
	if _, err := rt.j.SchemaVersion(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Errorf("after shutdown: %v", err)
	}
}
