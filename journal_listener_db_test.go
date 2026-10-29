package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/scan"
)

// Database tests of the core's journal wiring. They skip without
// JOURNAL_TEST_DATABASE_URL (scripts/journal-test-db.sh ... ./).

func saUser(sa string) string {
	ns, name, _ := bytes.Cut([]byte(sa), []byte("/"))
	return "system:serviceaccount:" + string(ns) + ":" + string(name)
}

func waitReady(t *testing.T, rt *journalRuntime) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second) // the scanner warm-up is slow under -race
	for !rt.ready.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the journal never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func postSnapshot(t *testing.T, rt *journalRuntime, token string, s journalapi.Snapshot) (int, journalapi.Response) {
	t.Helper()
	body, _ := json.Marshal(s)
	req, _ := http.NewRequest("POST", "http://"+rt.Addr().String()+journalapi.PathRecords, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out journalapi.Response
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The journal comes up with its database: ready, served, records written
// with the core's scanner under the rules-qualified version, the gauges
// refreshed from the database at once after a restart, and a clean
// shutdown that closes the database and leaks nothing.
func TestStartJournalBecomesReadyWithItsDatabase(t *testing.T) {
	dbURL := journaltest.NewDBURL(t)
	mp, scrape := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, dbURL, rosterFile(t))
	api, access := fakeAccess(t)
	api.Allow("gw", saUser(journaltest.SAGateway), journalapi.Audience)
	api.Allow("ps", saUser(journaltest.SAPowerSchool), journalapi.Audience)
	before := runtime.NumGoroutine()

	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, rt)
	if v, _ := metricValue(scrape(), "glovebox_journal_ready"); v != 1 {
		t.Errorf("ready gauge %v", v)
	}
	if code, _ := journalGet(t, rt, journalapi.PathHealthz, ""); code != http.StatusUnauthorized {
		t.Errorf("ready, no token: %d", code)
	}
	req, _ := http.NewRequest("GET", "http://"+rt.Addr().String()+journalapi.PathHealthz, nil)
	req.Header.Set("Authorization", "Bearer gw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var hz journalapi.Healthz
	_ = json.NewDecoder(resp.Body).Decode(&hz)
	resp.Body.Close()
	if resp.StatusCode != 200 || hz.RosterSHA256 != journaltest.Roster(t).SHA256() || hz.SchemaVersion < 1 {
		t.Errorf("healthz %d %+v", resp.StatusCode, hz)
	}

	snap := journalapi.Snapshot{SnapshotID: "00000000-0000-4000-8000-000000000001", RecordType: "assignment", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"}, {"data_subject", "e_child1"}, {"course", "c1"}},
		Complete: true, ObservedAt: time.Now().Add(-time.Minute),
		Records: []journalapi.Record{{Key: "a1", DataSubject: "e_child1",
			Fields: map[string]json.RawMessage{"title": json.RawMessage(`"Essay"`), "comment": json.RawMessage(`"Well argued, see me about the sources."`)}}}}
	if code, out := postSnapshot(t, rt, "ps", snap); code != http.StatusOK {
		t.Fatalf("post: %d %+v", code, out)
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var scanned string
	if err := db.QueryRow(`SELECT scan::text FROM record_current WHERE key = 'a1'`).Scan(&scanned); err != nil {
		t.Fatal(err)
	}
	if want := jc.ScannerVersion + "+rules." + sha[:12]; !bytes.Contains([]byte(scanned), []byte(want)) {
		t.Errorf("stored verdict %s does not name scanner %s", scanned, want)
	}

	shutdown(t, rt)
	if err := rt.j.Close(); err != nil { // already closed: sql.DB.Close is idempotent
		t.Errorf("second close: %v", err)
	}
	if _, err := rt.j.SchemaVersion(context.Background()); err == nil {
		t.Error("the database is still open after shutdown")
	}

	// A restart on the same database: the gauges come from the database as
	// soon as it is ready, not a minute later.
	mp2, scrape2 := testProvider(t)
	rt2, err := startJournal(jc, sc, sha, mp2, access)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, rt2)
	waitFor(t, func() bool {
		_, ok := metricValue(scrape2(), "glovebox_journal_last_snapshot_timestamp_seconds", `prefix="source=powerschool/"`)
		return ok
	})
	out := scrape2()
	if v, ok := metricValue(out, "glovebox_journal_held_snapshots"); !ok || v != 0 {
		t.Errorf("held %v %v", v, ok)
	}
	if _, ok := metricValue(out, "glovebox_journal_last_snapshot_timestamp_seconds", `prefix="source=schoology/"`); ok {
		t.Error("a prefix with no snapshot has a series")
	}
	shutdown(t, rt2)
	db.Close()
	settleGoroutines(t, before)
}

// The roster appears after the start: the journal retries and becomes
// ready without a restart.
func TestStartJournalWaitsForTheRoster(t *testing.T) {
	dbURL := journaltest.NewDBURL(t)
	mp, scrape := testProvider(t)
	sc, sha := testScanner(t)
	path := filepath.Join(t.TempDir(), "household.json")
	jc := testJournalConfig(t, dbURL, path)
	_, access := fakeAccess(t)
	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, rt)
	time.Sleep(300 * time.Millisecond)
	if rt.ready.Load() {
		t.Fatal("ready without a roster")
	}
	writeFile(t, path, journaltest.HouseholdV1)
	waitReady(t, rt)
	out := scrape()
	if _, ok := metricValue(out, "glovebox_journal_roster_info", `sha256="`+journaltest.Roster(t).SHA256()+`"`); !ok {
		t.Errorf("roster_info\n%s", out)
	}
}

// blockingTransport never answers: a TokenReview through it ends only when
// the request's context does.
type blockingTransport struct{}

func (blockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// journal.request_timeout_seconds bounds a request, TokenReview included:
// a hung API server answers 503 auth_unavailable after the configured
// second, not after the TokenReview client's own 15.
func TestStartJournalAppliesTheRequestTimeout(t *testing.T) {
	dbURL := journaltest.NewDBURL(t)
	mp, _ := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, dbURL, rosterFile(t))
	jc.RequestTimeoutSeconds = 1
	rt, err := startJournal(jc, sc, sha, mp, func() (*kubecm.Access, error) {
		return &kubecm.Access{BaseURL: "https://kubernetes.invalid:443", Transport: blockingTransport{},
			Token: func() (string, error) { return "own", nil }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, rt)
	waitReady(t, rt)
	start := time.Now()
	code, ref := journalGet(t, rt, journalapi.PathHealthz, "some-token")
	if d := time.Since(start); code != http.StatusServiceUnavailable || ref.Reason != journalapi.ReasonAuthUnavailable || d > 5*time.Second {
		t.Errorf("hung TokenReview: %d %+v after %v", code, ref, d)
	}
}

// The journal reports ready only after the scanner warm-up has finished,
// even with its database and roster in place (glovebox-wekc.18.3).
func TestStartJournalWarmsTheScannerBeforeReady(t *testing.T) {
	dbURL := journaltest.NewDBURL(t)
	mp, scrape := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, dbURL, rosterFile(t))
	_, access := fakeAccess(t)
	gate := make(chan struct{})
	var warmed atomic.Int32
	orig := warmScanner
	warmScanner = func(got *scan.Scanner) {
		if got != sc {
			t.Error("warmed another scanner")
		}
		warmed.Add(1)
		<-gate
	}
	defer func() { warmScanner = orig }()
	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, rt)
	time.Sleep(time.Second)
	if rt.ready.Load() || warmed.Load() != 1 {
		t.Fatalf("ready %v during the warm-up (warmed %d)", rt.ready.Load(), warmed.Load())
	}
	if code, ref := journalGet(t, rt, journalapi.PathHealthz, "t"); code != http.StatusServiceUnavailable || ref.Reason != journalapi.ReasonUnavailable {
		t.Errorf("during the warm-up: %d %+v", code, ref)
	}
	if v, _ := metricValue(scrape(), "glovebox_journal_ready"); v != 0 {
		t.Errorf("ready gauge %v during the warm-up", v)
	}
	close(gate)
	waitReady(t, rt)
	if warmed.Load() != 1 {
		t.Errorf("warmed %d times", warmed.Load())
	}
}

// heldScanner wraps the journal's text scanner; while hold is set, a
// Verdict waits for release.
type heldScanner struct {
	inner   journal.TextScanner
	hold    atomic.Bool
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func (h *heldScanner) Version() string { return h.inner.Version() }

func (h *heldScanner) Verdict(text string) string {
	if h.hold.Load() {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return h.inner.Verdict(text)
}

// The shutdown contract with a rescan running (Task 27 S-2): Shutdown
// waits for it, the journal's context is cancelled so it stops instead of
// running to the end, and only then is the database closed.
func TestShutdownStopsARunningRescan(t *testing.T) {
	dbURL := journaltest.NewDBURL(t)
	mp, _ := testProvider(t)
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, dbURL, rosterFile(t))
	api, access := fakeAccess(t)
	api.Allow("ps", saUser(journaltest.SAPowerSchool), journalapi.Audience)
	api.Allow("admin", saUser(journaltest.SAAdmin), journalapi.Audience)
	held := &heldScanner{entered: make(chan struct{}), release: make(chan struct{})}
	origScanner := newTextScanner
	newTextScanner = func(s *scan.Scanner, v string, d time.Duration) (journal.TextScanner, error) {
		inner, err := origScanner(s, v, d)
		held.inner = inner
		return held, err
	}
	defer func() { newTextScanner = origScanner }()
	var logs syncBuffer
	origLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(origLog)

	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, rt)
	snap := journalapi.Snapshot{SnapshotID: "00000000-0000-4000-8000-000000000002", RecordType: "assignment", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"}, {"data_subject", "e_child1"}, {"course", "c1"}},
		Complete: true, ObservedAt: time.Now().Add(-time.Minute),
		Records: []journalapi.Record{{Key: "a1", DataSubject: "e_child1",
			Fields: map[string]json.RawMessage{"comment": json.RawMessage(`"Please bring the permission slip."`)}}}}
	if code, out := postSnapshot(t, rt, "ps", snap); code != http.StatusOK {
		t.Fatalf("post: %d %+v", code, out)
	}
	held.hold.Store(true)
	req, _ := http.NewRequest("POST", "http://"+rt.Addr().String()+journalapi.PathAdminRescan, strings.NewReader(`{"all": true}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rescan: %d", resp.StatusCode)
	}
	<-held.entered // the rescan is inside a verdict

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done <- rt.Shutdown(ctx)
	}()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned with a rescan running: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := net.Dial("tcp", rt.Addr().String()); err == nil {
		t.Error("the listener still accepts while the rescan drains")
	}
	close(held.release)
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "journal rescan failed") || strings.Contains(out, "journal rescan finished") {
		t.Errorf("the rescan was not stopped by the shutdown:\n%s", out)
	}
}
