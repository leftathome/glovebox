package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/httpapi"
	"github.com/leftathome/glovebox/internal/journal/jmetrics"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/satoken"
	"github.com/leftathome/glovebox/internal/scan"
)

// serviceAccountDir is where the core's own ServiceAccount token is
// mounted; the journal's TokenReview client authenticates with it. A
// variable only so that the root package's re-executed core (main_test.go)
// can point it at a test directory.
var serviceAccountDir = kubecm.ServiceAccountDir

// inClusterAccess is how the core reaches the API server for TokenReview.
func inClusterAccess() (*kubecm.Access, error) {
	return kubecm.InCluster(os.Getenv, serviceAccountDir)
}

// Timings of the journal's background loops.
const (
	journalRetryFirst   = time.Second
	journalRetryMax     = time.Minute
	journalGaugeEvery   = time.Minute
	journalWriteSlack   = 5 * time.Second
	journalHeaderBudget = 10 * time.Second
)

// warmUpText is scanned once, untimed, before the journal reports ready:
// lingua loads its language models lazily on the first detection, which
// takes long enough (0.66s, 5s under -race) to time out the first journal
// scans and withhold their values (glovebox-wekc.18.3). It is long enough
// prose for the language detector to run.
const warmUpText = "This sentence warms the scanner so that its language models are loaded before the journal is ready."

// warmScanner runs the warm-up scan. A variable so a test can hold it.
var warmScanner = func(sc *scan.Scanner) { _, _ = sc.Scan([]byte(warmUpText), "text/plain") }

// newTextScanner builds the journal's text scanner. A variable so a test
// can hold a rescan inside a verdict.
var newTextScanner = journal.NewTextScanner

// rosterStrictWarning is logged once at start: the roster contract adopters
// must know before they ever edit household.json.
const rosterStrictWarning = "journal: WARNING the household roster is parsed strictly: " +
	"a field this glovebox does not know makes it refuse the whole file and keep the last roster it accepted, " +
	"so members removed after that keep their access until glovebox is updated. " +
	"Update glovebox before adding a field to household.json (docs/journal-roster.md, " +
	"\"Adding a field to household.json\"); alert GloveboxJournalRosterReloadFailing fires while this lasts"

// rosterReloadMessage is the log line for a failed roster load. An unknown
// field gets its own, explicit line: it is the case an operator causes by
// adding an openclaw field, and the one where revocations silently stop.
func rosterReloadMessage(err error) string {
	if errors.Is(err, roster.ErrUnknownField) {
		return "journal: ERROR roster refused, " + err.Error() + ": this glovebox does not know that household.json field. " +
			"Keeping the last roster accepted: membership changes, INCLUDING REMOVALS, are not applied " +
			"until glovebox is updated to know the field (docs/journal-roster.md, \"Adding a field to household.json\")"
	}
	return fmt.Sprintf("journal: roster reload failed, keeping the previous roster: %v", err)
}

// readyAttemptTimeout bounds one ready-loop attempt (migrations and type
// sync); gaugeRefreshTimeout bounds one gauge refresh. Variables so tests
// can shorten them.
var (
	readyAttemptTimeout = 60 * time.Second
	gaugeRefreshTimeout = 30 * time.Second
)

// scannerVersion names the scanner a stored verdict came from: the
// configured journal.scanner_version and the digest of the rules file in
// use. A rescan re-judges values from any other version, so a rules update
// re-scans stored text without anyone bumping scanner_version.
func scannerVersion(version, rulesSHA256 string) string {
	short := "unknown"
	if len(rulesSHA256) >= 12 {
		short = rulesSHA256[:12]
	}
	return version + "+rules." + short
}

// journalRuntime is a started record journal: its listener, its background
// loops and its database.
type journalRuntime struct {
	srv            *http.Server
	ln             net.Listener
	api            *httpapi.Server
	j              *journal.Journal
	cancel         context.CancelFunc
	k8sTransport   http.RoundTripper // TokenReview's; idle connections closed at shutdown
	loops          sync.WaitGroup
	ready          atomic.Bool
	scannerVersion string
	served         chan struct{} // closed when Serve returns
}

// startJournal brings up the record journal when journal.enabled is set:
// its own listener, readiness and metrics. It returns nil, nil when the
// journal is off, having touched nothing -- no metric is registered, no
// database is opened, nothing listens.
//
// An error is a configuration problem (outside a pod, a policy no caller
// can pass, a port that cannot be bound). Nothing that can go wrong later
// -- the database, the schema, the roster -- is an error here: the journal
// answers 503 while journalReadyLoop retries, and the item pipeline never
// waits for it.
func startJournal(jc config.JournalConfig, sc *scan.Scanner, rulesSHA256 string, mp metric.MeterProvider,
	k8s func() (*kubecm.Access, error)) (*journalRuntime, error) {
	if !jc.Enabled {
		return nil, nil
	}
	access, err := k8s()
	if err != nil {
		return nil, fmt.Errorf("journal: the Kubernetes API is needed for TokenReview: %w", err)
	}
	reg, rs, pol, err := jc.Build()
	if err != nil {
		return nil, err
	}
	reviewer, err := satoken.NewTokenReviewer(access, journalapi.Audience, pol.Callers())
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	ver := scannerVersion(jc.ScannerVersion, rulesSHA256)
	ts, err := newTextScanner(sc, ver, jc.ScanTimeout())
	if err != nil {
		return nil, err
	}
	dsn, password, err := jc.Database.Connection()
	if err != nil {
		return nil, err
	}
	// Registered only here, inside the enabled branch: a core with the
	// journal off must not export glovebox_journal_ready, or the NotReady
	// alert would fire on every install that never enabled it.
	jm, err := jmetrics.New(mp)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", jc.Port))
	if err != nil {
		return nil, fmt.Errorf("journal: listen on :%d: %w", jc.Port, err)
	}
	db, err := journal.Open(dsn, password) // lazy: dials nothing yet
	if err != nil {
		ln.Close()
		return nil, err
	}
	holder := roster.NewHolder(jc.Roster.Path, jc.Roster.Format)
	// Before Watch starts: the gauge reads holder.Failing at every scrape,
	// so it follows every reload attempt (Task 29).
	jm.SetRosterReloadFailingSource(holder.Failing)
	j := journal.New(db, journal.Options{
		Registry: reg, Rules: rs, Guard: jc.Guard(), Scanner: ts,
		Roster: holder.Current, Now: time.Now, Metrics: jm,
	})

	// The journal's own lifetime, not the core's: shutdown must stop the
	// listener before this is cancelled (journalShutdown).
	ctx, cancel := context.WithCancel(context.Background())
	rt := &journalRuntime{ln: ln, j: j, cancel: cancel, k8sTransport: access.Transport,
		scannerVersion: ver, served: make(chan struct{})}
	api, err := httpapi.New(httpapi.Deps{
		Journal: j, Registry: reg, Auth: reviewer, Policy: pol, Roster: holder.Current,
		Metrics: jm, Log: slog.Default(), Now: time.Now, Ready: rt.ready.Load,
		RequestTimeout: jc.RequestTimeout(), MaxInFlight: httpapi.DefaultMaxInFlight,
		Context: ctx,
	})
	if err != nil {
		cancel()
		ln.Close()
		db.Close()
		return nil, err
	}
	rt.api = api
	timeout := jc.RequestTimeout()
	rt.srv = &http.Server{Handler: api.Handler(), ReadHeaderTimeout: journalHeaderBudget,
		ReadTimeout: timeout, WriteTimeout: timeout + journalWriteSlack, IdleTimeout: 2 * time.Minute,
		ErrorLog: log.Default()}

	rt.goLoop("ready loop", func() {
		journalReadyLoop(ctx, readySteps{
			warm: func() { warmScanner(sc) },
			roster: func() error {
				if holder.Current() != nil {
					return nil
				}
				if _, err := holder.Reload(); err != nil {
					if errors.Is(err, roster.ErrUnknownField) {
						log.Print(rosterReloadMessage(err))
					}
					return fmt.Errorf("roster: %w", err)
				}
				jm.SetRosterSHA(holder.Current().SHA256())
				return nil
			},
			migrate: j.Migrate,
			sync:    j.SyncTypes,
			ready: func() {
				rt.ready.Store(true)
				jm.SetReady(true)
				// The gauges read the database, so they start with it:
				// refreshed now, then every journalGaugeEvery.
				rt.goLoop("gauge loop", func() {
					journalGaugeLoop(ctx, journalGaugeEvery, j.Gauges, jc.StalenessPrefixes, jm)
				})
			},
		}, journalRetryFirst, journalRetryMax)
	})
	rt.goLoop("roster watch", func() {
		holder.Watch(ctx, jc.Roster.Reload(), func(r *roster.Roster, changed bool, err error) {
			if err != nil {
				jm.RosterReloadError()
				log.Print(rosterReloadMessage(err))
				return
			}
			if changed && r != nil {
				jm.SetRosterSHA(r.SHA256())
				log.Printf("journal: roster in use, sha256 %s", r.SHA256())
			}
		})
	})
	log.Print(rosterStrictWarning)
	go func() {
		defer close(rt.served)
		log.Printf("journal listening on %s (%d record type versions, %d services, scanner %s)",
			ln.Addr(), len(reg.Types()), len(pol.Callers()), ver)
		if err := rt.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("journal server error: %v", err)
		}
	}()
	return rt, nil
}

// goLoop runs fn in a goroutine counted by rt.loops. A panic is logged and
// ends that loop only: no journal failure may take the core down.
func (rt *journalRuntime) goLoop(name string, fn func()) {
	rt.loops.Add(1)
	go func() {
		defer rt.loops.Done()
		defer func() {
			if p := recover(); p != nil {
				log.Printf("journal: %s panicked and stopped: %v", name, p)
			}
		}()
		fn()
	}()
}

// Addr is the listener's address.
func (rt *journalRuntime) Addr() net.Addr { return rt.ln.Addr() }

// Shutdown stops the journal in the order httpapi.Server.Wait requires.
// Nil-safe, so the core calls it whether or not the journal is on.
func (rt *journalRuntime) Shutdown(ctx context.Context) error {
	if rt == nil {
		return nil
	}
	defer func() {
		if t, ok := rt.k8sTransport.(interface{ CloseIdleConnections() }); ok {
			t.CloseIdleConnections()
		}
	}()
	err := journalShutdown(ctx, shutdownSteps{
		stopListener: func(ctx context.Context) error {
			err := rt.srv.Shutdown(ctx)
			if err != nil {
				rt.srv.Close() // the deadline passed: drop what is still open
			}
			// Check served first: once the deadline has passed both cases
			// can be ready, and select would pick one at random.
			select {
			case <-rt.served:
				return err
			default:
			}
			select {
			case <-rt.served:
			case <-ctx.Done():
				err = errors.Join(err, fmt.Errorf("journal: listener still serving: %w", ctx.Err()))
			}
			return err
		},
		cancel: rt.cancel,
		drain: func() {
			rt.api.Wait()
			rt.loops.Wait()
		},
		closeDB: rt.j.Close,
	})
	if err == nil {
		log.Printf("journal stopped")
	}
	return err
}

// shutdownSteps are the parts of a journal shutdown, apart so the order is
// testable.
type shutdownSteps struct {
	stopListener func(context.Context) error
	cancel       func()
	drain        func() // the background rescan and the loops
	closeDB      func() error
}

// journalShutdown: stop the listener (no request can start another rescan),
// cancel the journal's context (a running rescan stops at its next batch,
// the loops return), wait for both, and only then close the database. If
// ctx ends first the database is left open -- the process is exiting, and
// closing it under a running rescan is what the order exists to prevent.
func journalShutdown(ctx context.Context, s shutdownSteps) error {
	lerr := s.stopListener(ctx)
	s.cancel()
	drained := make(chan struct{})
	go func() {
		s.drain()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		return errors.Join(lerr, fmt.Errorf("journal: background work still running at shutdown: %w", ctx.Err()))
	}
	return errors.Join(lerr, s.closeDB())
}

// readySteps are what journalReadyLoop runs.
type readySteps struct {
	warm    func()                      // once, untimed, before anything else
	roster  func() error                // a roster is in use
	migrate func(context.Context) error // the schema is current
	sync    func(context.Context) error // the record types are stored
	ready   func()                      // all of the above succeeded
}

// journalReadyLoop warms the scanner, then retries roster, migrate and sync
// with backoff until all three succeed in one attempt, then calls ready.
// It ends when ctx does. A panic in an attempt counts as a failed attempt.
func journalReadyLoop(ctx context.Context, s readySteps, first, maxBackoff time.Duration) {
	func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("journal: scanner warm-up panicked: %v", p)
			}
		}()
		s.warm()
	}()
	backoff := first
	for {
		if ctx.Err() != nil {
			return
		}
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			if err := s.roster(); err != nil {
				return err
			}
			// Each attempt gets its own deadline: with no connect_timeout in
			// the DSN, a silently dropping network would otherwise hold an
			// attempt for the OS SYN timeout, and a half-dead connection
			// could hang it indefinitely.
			actx, cancel := context.WithTimeout(ctx, readyAttemptTimeout)
			defer cancel()
			if err := s.migrate(actx); err != nil {
				return err
			}
			return s.sync(actx)
		}()
		if err == nil {
			s.ready()
			log.Printf("journal ready")
			return
		}
		log.Printf("journal not ready: %v (retrying in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// journalGaugeLoop refreshes the database-derived gauges (held snapshots,
// newest snapshot per staleness prefix) now and every interval; the ready
// loop starts it once the database is there. A failed refresh is logged and
// the gauges keep their values.
func journalGaugeLoop(ctx context.Context, every time.Duration,
	gauges func(context.Context, []string) (int64, map[string]time.Time, error), prefixes []string, jm *jmetrics.Metrics) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		func() {
			defer func() {
				if p := recover(); p != nil {
					log.Printf("journal gauges panicked: %v", p)
				}
			}()
			// A deadline per refresh, so a hung query cannot freeze
			// held_snapshots and mask the held-snapshot alert.
			gctx, cancel := context.WithTimeout(ctx, gaugeRefreshTimeout)
			defer cancel()
			held, last, err := gauges(gctx, prefixes)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("journal gauges: %v", err)
				}
				return
			}
			jm.SetHeld(held)
			for p, ts := range last {
				jm.SetLastSnapshot(p, ts)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
