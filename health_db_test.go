package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

// glovebox-q0ch regression guard (database harness: scripts/journal-test-db.sh ./).
// The core's metrics port must answer while the journal's database is
// saturated: many snapshot applies stuck behind a held advisory lock (every
// pooled connection busy, the in-flight slots full), a database gauge refresh
// waiting for a connection, and a scrape of the journal's gauges at the same
// time. Liveness is the process's, never the database's speed.
//
// This passes on 0.13.1 as well: the investigation found that /healthz never
// touched the journal or its database (the restart was the delivery-mount
// write probe, TestHealthzAnswersWhileDeliveryMountIsSlow). It is here so a
// future change cannot make it so.
func TestMetricsPortAnswersWhileTheJournalDatabaseIsStuck(t *testing.T) {
	dbURL := journaltest.NewDBURL(t)
	reg := promclient.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	sc, sha := testScanner(t)
	jc := testJournalConfig(t, dbURL, rosterFile(t))
	api, access := fakeAccess(t)
	api.Allow("ps", saUser(journaltest.SAPowerSchool), journalapi.Audience)
	rt, err := startJournal(jc, sc, sha, mp, access)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, rt)
	waitReady(t, rt)

	p := newDeliveryProber(t.TempDir(), deliveryWritable)
	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	go p.Run(pctx)
	ready := &atomic.Bool{}
	ready.Store(true)
	core := httptest.NewServer(metricsMux(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), p, ready))
	defer core.Close()

	// Hold the journal's events lock: every apply that writes an event waits
	// for it, holding a pooled connection and an in-flight slot.
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lock, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "glovebox_journal:events"); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = lock.Rollback()
		}
	}
	defer release()

	const n = 32 // twice the in-flight slots, four times the pool
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- postSnapshotCode(rt, "ps", journalapi.Snapshot{
				SnapshotID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), RecordType: "assignment", SchemaVersion: 1,
				Scope: []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"},
					{"data_subject", "e_child1"}, {"course", fmt.Sprintf("c%d", i)}},
				Complete: true, ObservedAt: time.Now().Add(-time.Minute),
				Records: []journalapi.Record{{Key: fmt.Sprintf("a%d", i), DataSubject: "e_child1",
					Fields: map[string]json.RawMessage{"title": json.RawMessage(`"Essay"`)}}},
			})
		}(i)
	}
	gaugesDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _, err := rt.j.Gauges(ctx, jc.StalenessPrefixes)
		gaugesDone <- err
	}()

	// Wait until the database really is the bottleneck.
	waitFor(t, func() bool {
		var waiting int
		_ = db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting)
		return waiting >= 4
	})

	for i := 0; i < 10; i++ {
		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			code, d := timedGet(t, core.URL+path)
			if code != http.StatusOK || d > probeBudget {
				t.Fatalf("%s with the journal's database stuck: status %d after %v, want 200 within %v",
					path, code, d.Round(time.Millisecond), probeBudget)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	release()
	wg.Wait()
	close(codes)
	for c := range codes {
		// Beyond the 16 in-flight slots the journal answers 503 at once;
		// everything it took is applied once the lock goes.
		if c != http.StatusOK && c != http.StatusServiceUnavailable {
			t.Errorf("snapshot answered %d", c)
		}
	}
	if err := <-gaugesDone; err != nil {
		t.Errorf("gauges after the lock went: %v", err)
	}
}

// postSnapshotCode posts s and returns the status (0 on a transport error);
// safe off the test goroutine.
func postSnapshotCode(rt *journalRuntime, token string, s journalapi.Snapshot) int {
	body, _ := json.Marshal(s)
	req, _ := http.NewRequest("POST", "http://"+rt.Addr().String()+journalapi.PathRecords, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}
