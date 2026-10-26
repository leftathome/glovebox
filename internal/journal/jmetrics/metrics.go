// Package jmetrics is the record journal's metrics (spec 2026-10-16
// section 4.15), registered on the core's OpenTelemetry provider so they
// are scraped with everything else. No label carries a subject, a key or a
// value. A nil *Metrics records nothing.
package jmetrics

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leftathome/glovebox/internal/journal/record"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics are the journal's instruments.
type Metrics struct {
	snapshots, events, withheld, requests, rosterErrors, mismatches metric.Int64Counter
	apply                                                           metric.Float64Histogram

	held  atomic.Int64
	ready atomic.Int64
	// failing is the source of the roster_reload_failing gauge, read at
	// every collection (SetRosterReloadFailingSource).
	failing atomic.Pointer[func() bool]
	mu      sync.Mutex
	sha     string
	last    map[string]float64
}

// New registers the instruments on mp.
func New(mp metric.MeterProvider) (*Metrics, error) {
	meter := mp.Meter("glovebox/journal")
	m := &Metrics{last: map[string]float64{}}
	var err error
	counter := func(name, desc string) metric.Int64Counter {
		if err != nil {
			return nil
		}
		var c metric.Int64Counter
		c, err = meter.Int64Counter(name, metric.WithDescription(desc))
		return c
	}
	m.snapshots = counter("glovebox_journal_snapshots_total", "Snapshots answered, by record type and status")
	m.events = counter("glovebox_journal_events_total", "Events written, by record type, kind and importance")
	m.withheld = counter("glovebox_journal_scan_withheld_total", "Stored text values whose scan verdict withholds them")
	m.requests = counter("glovebox_journal_api_requests_total", "Journal API requests, by endpoint and status code")
	m.rosterErrors = counter("glovebox_journal_roster_reload_errors_total", "Roster reloads that failed; the previous roster stays in use")
	m.mismatches = counter("glovebox_journal_visibility_mismatch_total", "Rows the SQL filter returned that the Go visibility check refused")
	if err != nil {
		return nil, err
	}
	if m.apply, err = meter.Float64Histogram("glovebox_journal_apply_duration_seconds",
		metric.WithDescription("Time to apply one snapshot")); err != nil {
		return nil, err
	}
	held, err := meter.Int64ObservableGauge("glovebox_journal_held_snapshots", metric.WithDescription("Snapshots held by the removal guard"))
	if err != nil {
		return nil, err
	}
	ready, err := meter.Int64ObservableGauge("glovebox_journal_ready", metric.WithDescription("1 when the journal's database and roster are ready"))
	if err != nil {
		return nil, err
	}
	roster, err := meter.Int64ObservableGauge("glovebox_journal_roster_info", metric.WithDescription("1, labelled with the sha256 of the roster in use"))
	if err != nil {
		return nil, err
	}
	failing, err := meter.Int64ObservableGauge("glovebox_journal_roster_reload_failing",
		metric.WithDescription("1 while the latest roster reload failed (the previous roster stays in use), else 0"))
	if err != nil {
		return nil, err
	}
	last, err := meter.Float64ObservableGauge("glovebox_journal_last_snapshot_timestamp_seconds",
		metric.WithDescription("Newest snapshot received under a configured scope prefix"))
	if err != nil {
		return nil, err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(held, m.held.Load())
		o.ObserveInt64(ready, m.ready.Load())
		var f int64
		if src := m.failing.Load(); src != nil && (*src)() {
			f = 1
		}
		o.ObserveInt64(failing, f)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.sha != "" {
			o.ObserveInt64(roster, 1, metric.WithAttributes(attribute.String("sha256", m.sha)))
		}
		for p, ts := range m.last {
			o.ObserveFloat64(last, ts, metric.WithAttributes(attribute.String("prefix", p)))
		}
		return nil
	}, held, ready, roster, failing, last)
	return m, err
}

func add(c metric.Int64Counter, n int64, attrs ...attribute.KeyValue) {
	c.Add(context.Background(), n, metric.WithAttributes(attrs...))
}

// Snapshot implements journal.Recorder.
func (m *Metrics) Snapshot(recordType, status string) {
	if m != nil {
		add(m.snapshots, 1, attribute.String("record_type", recordType), attribute.String("status", status))
	}
}

// Event implements journal.Recorder.
func (m *Metrics) Event(recordType, kind, importance string) {
	if m != nil {
		add(m.events, 1, attribute.String("record_type", recordType), attribute.String("kind", kind), attribute.String("importance", importance))
	}
}

// ScanWithheld implements journal.Recorder.
func (m *Metrics) ScanWithheld(recordType string, n int) {
	if m != nil {
		add(m.withheld, int64(n), attribute.String("record_type", recordType))
	}
}

// ApplyDuration implements journal.Recorder.
func (m *Metrics) ApplyDuration(d time.Duration) {
	if m != nil {
		m.apply.Record(context.Background(), d.Seconds())
	}
}

// VisibilityMismatch implements journal.Recorder.
func (m *Metrics) VisibilityMismatch() {
	if m != nil {
		add(m.mismatches, 1)
	}
}

// Request counts one API answer. endpoint is a fixed route name, never a
// path carrying an id.
func (m *Metrics) Request(endpoint string, code int) {
	if m != nil {
		add(m.requests, 1, attribute.String("endpoint", endpoint), attribute.String("code", strconv.Itoa(code)))
	}
}

// RosterReloadError counts a failed reload. It counts what the caller
// reports; roster.Holder.Watch reports a repeated error once, so the
// counter is not a reliable "still failing" signal -- the alert uses the
// roster_reload_failing gauge.
func (m *Metrics) RosterReloadError() {
	if m != nil {
		add(m.rosterErrors, 1)
	}
}

// SetRosterReloadFailingSource names the function the
// glovebox_journal_roster_reload_failing gauge reads at every collection:
// 1 when it returns true, 0 otherwise (and 0 while no source is set).
//
// Contract for the core's wiring (Task 28): call it once, right after
// creating the roster.Holder and before starting Holder.Watch, with
// holder.Failing. Every Reload attempt sets Failing, so the gauge follows
// each tick without any per-tick call, and stays 1 for as long as the file
// stays broken -- which Watch's deduplicated reports cannot show. A nil
// function clears the source.
func (m *Metrics) SetRosterReloadFailingSource(f func() bool) {
	if m == nil {
		return
	}
	if f == nil {
		m.failing.Store(nil)
		return
	}
	m.failing.Store(&f)
}

// SetHeld sets the held-snapshot gauge.
func (m *Metrics) SetHeld(n int64) {
	if m != nil {
		m.held.Store(n)
	}
}

// SetReady sets the readiness gauge.
func (m *Metrics) SetReady(ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.ready.Store(1)
	} else {
		m.ready.Store(0)
	}
}

// SetRosterSHA records the hash of the roster in use.
func (m *Metrics) SetRosterSHA(sha string) {
	if m != nil {
		m.mu.Lock()
		m.sha = sha
		m.mu.Unlock()
	}
}

// SetLastSnapshot records the newest snapshot under a configured scope
// prefix. prefix comes from configuration, never from a request, and the
// config validator refuses one below the surface; as a second guard a prefix
// that names a data subject is skipped and logged rather than exported as a
// label.
//
// A prefix that has never received a snapshot has no series at all (Gauges
// omits it), and the values live in memory, so a restart drops them until
// the next snapshot. The chart's staleness alert (journal-prometheusrule.yaml)
// therefore fires on an old value, or on absence for the whole window only
// when the journal itself was already up at the window's start:
// `time() - max by (prefix) (M{prefix="..."}) > W
// or (absent_over_time(M{prefix="..."}[W]) and on() present_over_time(glovebox_journal_ready[1h] offset W))`
// with M glovebox_journal_last_snapshot_timestamp_seconds. Plain absent()
// would page at every enable and after every restart.
func (m *Metrics) SetLastSnapshot(prefix string, t time.Time) {
	if m == nil {
		return
	}
	if strings.Contains(prefix, record.PartSubject+"=") {
		slog.Warn("journal metrics: staleness prefix names a data subject; not exported")
		return
	}
	m.mu.Lock()
	m.last[prefix] = float64(t.Unix())
	m.mu.Unlock()
}
