package records

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// refreshEvery is how often the loop wakes with nothing to do: gauges
// (oldest_seconds keeps growing) and max_age.
const refreshEvery = time.Minute

// Options are what New needs besides the config.
type Options struct {
	// StateDir is the connector's state volume; the spool lives in
	// StateDir/spool. Required, absolute.
	StateDir string
	// Connector is the "connector" label of the metrics.
	Connector string
	// Meter registers the spool metrics (the framework's provider:
	// connector.Metrics.Provider().Meter("glovebox.records")). nil: none.
	Meter metric.Meter
	// Log receives the spool's log lines. nil: slog.Default().
	Log *slog.Logger
	// Now is the clock. nil: time.Now.
	Now func() time.Time

	// Test hooks.
	fs        fsys
	transport http.RoundTripper
	passed    chan<- PassResult
}

// Emitter is what a connector holds: the spool, the sender and its loop.
// A nil *Emitter (records disabled) does nothing.
type Emitter struct {
	spool  *Spool
	sender *Sender
	m      *Metrics
	kick   chan struct{}
	log    *slog.Logger
	now    func() time.Time
	passed chan<- PassResult
}

// New builds the emitter. With cfg.Enabled false it returns (nil, nil)
// and creates nothing: no directory, metric, goroutine or log line. For
// an enabled config it applies the defaults, validates, opens (and
// recovers) the spool under StateDir, and builds the client and metrics.
// It starts nothing: the connector runs Run in one goroutine.
func New(cfg Config, o Options) (*Emitter, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.fs == nil {
		o.fs = osFS{}
	}
	var m *Metrics
	if o.Meter != nil {
		var err error
		if m, err = NewMetrics(o.Meter, o.Connector); err != nil {
			return nil, fmt.Errorf("records: metrics: %w", err)
		}
	}
	client, err := NewClient(cfg.JournalURL, TokenFile(cfg.TokenFile), cfg.Timeout(), o.transport)
	if err != nil {
		return nil, err
	}
	spool, err := openSpool(o.fs, o.StateDir, cfg.Spool, m, o.Log, o.Now)
	if err != nil {
		return nil, err
	}
	return &Emitter{
		spool:  spool,
		sender: NewSender(spool, client, m, cfg.Spool, o.Log, o.Now),
		m:      m,
		kick:   make(chan struct{}, 1),
		log:    o.Log,
		now:    o.Now,
		passed: o.passed,
	}, nil
}

// Emit writes snapshots to the spool and wakes the loop. An error means
// some could not be written: they are logged and counted
// (write_errors_total) and the caller carries on -- items are never
// affected by records. The error carries no path or value.
func (e *Emitter) Emit(snaps []journalapi.Snapshot) error {
	if e == nil {
		return nil
	}
	failed := 0
	for _, s := range snaps {
		if err := e.spool.Put(s); err != nil {
			failed++
		}
	}
	e.wake()
	if failed > 0 {
		e.log.Warn("records snapshots could not be written to the spool", "failed", failed, "total", len(snaps))
		return fmt.Errorf("records: %d of %d snapshots could not be written to the spool", failed, len(snaps))
	}
	return nil
}

// OnCut is the Fields callback that counts cut text values.
func (e *Emitter) OnCut() {
	if e == nil {
		return
	}
	e.m.cutOne()
}

func (e *Emitter) wake() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run drains at once, then whenever Emit wakes it, when a backoff ends,
// and every minute (gauges, max_age). It returns when ctx ends. It is the
// only goroutine the records feature adds; a nil *Emitter returns at once.
func (e *Emitter) Run(ctx context.Context) {
	if e == nil {
		return
	}
	// The first pass covers whatever was emitted before Run started.
	select {
	case <-e.kick:
	default:
	}
	for {
		res := e.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if e.passed != nil {
			select {
			case e.passed <- res:
			case <-ctx.Done():
				return
			}
		}
		wait := refreshEvery
		if !res.NextDue.IsZero() {
			wait = min(wait, max(res.NextDue.Sub(e.now()), 0))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.kick:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// errPanic marks a pass that panicked.
var errPanic = errors.New("records: drain pass panicked")

// pass runs one drain pass. A panic is recovered, logged and treated as a
// transport failure (global backoff); the loop keeps running.
func (e *Emitter) pass(ctx context.Context) (res PassResult) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("records drain pass failed; backing off", "error", errPanic.Error())
			e.sender.FailPass()
			res = PassResult{NextDue: e.sender.globalUntil}
		}
	}()
	res = e.sender.Drain(ctx)
	e.spool.RefreshGauges()
	return res
}
