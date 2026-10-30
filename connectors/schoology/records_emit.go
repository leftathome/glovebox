package schoology

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// RecordSink is where a kid's record snapshots go: the records.Emitter in
// the binary, a recorder in tests.
type RecordSink interface {
	// Emit spools snapshots. An error is a spool write failure, already
	// logged and counted by the sink; items are never affected.
	Emit([]journalapi.Snapshot) error
	// OnCut counts a text value cut to 64 KiB.
	OnCut()
	// Run delivers the spool until ctx ends.
	Run(context.Context)
}

// NewRecordsFactory returns the NewRecords hook for the binary: nil when
// records are off (nothing is built, nothing changes), else a function
// that opens the spool under stateDir and builds the emitter on the
// framework's meter, with now as the spool's clock (nil: time.Now). A
// spool that cannot be opened fails Wire, so a config that asked for
// records and cannot have them stops the process.
func NewRecordsFactory(cfg records.Config, stateDir string, now func() time.Time) func(metric.Meter) (RecordSink, error) {
	if !cfg.Enabled {
		return nil
	}
	return func(meter metric.Meter) (RecordSink, error) {
		e, err := records.New(cfg, records.Options{StateDir: stateDir, Connector: recordSource, Meter: meter, Now: now})
		if err != nil {
			return nil, err
		}
		slog.Info("schoology records enabled: snapshots are spooled and sent to the journal in shadow",
			"journal", cfg.JournalURL)
		return e, nil
	}
}

// recordSink returns the sink Wire built, or nil when records are off.
func (c *SchoologyConnector) recordSink() RecordSink {
	c.wireMu.Lock()
	defer c.wireMu.Unlock()
	return c.records
}

// startRecords starts the sink's delivery loop for the life of ctx (one
// Watch). The framework restarts Watch with a new context; the loop of the
// previous Watch, whose context has ended, is waited for first, so at most
// one runs. With records off it does nothing.
func (c *SchoologyConnector) startRecords(ctx context.Context) {
	sink := c.recordSink()
	if sink == nil {
		return
	}
	c.recordsMu.Lock()
	defer c.recordsMu.Unlock()
	if prev := c.recordsDone; prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			return
		}
	}
	done := make(chan struct{})
	c.recordsDone = done
	go func() {
		defer close(done)
		sink.Run(ctx)
	}()
}

// clock is the connector's clock: c.now when a test set it, else time.Now.
func (c *SchoologyConnector) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// emitListing spools the lms_assignment snapshot of a kid's listing that
// answered. It runs after the listing's items were staged and touches
// nothing of theirs: a panic is recovered and counted, an Emit error is
// logged. Without the kid's rule (or its data_subject) there is nothing to
// send, as there are no items.
func emitListing(sink RecordSink, tel *Telemetry, matcher *connector.RuleMatcher, kid Kid, l KidAssignments, observedAt time.Time) {
	defer recoverRecords(tel, kid)
	b, ok := subjectFor(matcher, kid, assignmentSurface)
	if !ok {
		return
	}
	b.OnSkip = tel.RecordRecordRowSkipped
	s := BuildAssignmentListing(b, l.Rows, observedAt)
	if !b.fits(s) {
		return
	}
	emitSnapshots(sink, kid, []journalapi.Snapshot{s})
}

// emitDetails spools the lms_assignment_detail snapshots of each kid's
// clean detail reads in this poll, kids in config order. Same rules as
// emitListing.
func emitDetails(sink RecordSink, tel *Telemetry, matcher *connector.RuleMatcher, kids []Kid, reads map[string][]VerifiedDetail) {
	for _, kid := range kids {
		if len(reads[kid.Name]) == 0 {
			continue
		}
		func() {
			defer recoverRecords(tel, kid)
			b, ok := subjectFor(matcher, kid, detailSurface)
			if !ok {
				return
			}
			b.OnSkip = tel.RecordRecordRowSkipped
			kr := reads[kid.Name]
			emitSnapshots(sink, kid, BuildAssignmentDetails(b, kr, kr[0].ReadAt, sink.OnCut))
		}()
	}
}

func emitSnapshots(sink RecordSink, kid Kid, snaps []journalapi.Snapshot) {
	if len(snaps) == 0 {
		return
	}
	if err := sink.Emit(snaps); err != nil {
		slog.Warn("schoology record snapshots were not all spooled; items are unaffected",
			"kid", kid.Name, "record_type", snaps[0].RecordType, "snapshots", len(snaps))
	}
}

func recoverRecords(tel *Telemetry, kid Kid) {
	if r := recover(); r != nil {
		tel.RecordRecordRowSkipped("builder_panic")
		slog.Error("schoology records for this kid failed; items are unaffected", "kid", kid.Name)
	}
}

// fits reports whether the journal would take the snapshot's size. A
// snapshot over a limit is refused whole (413, permanent) and would be
// refused again at every poll; it is left out here, logged and counted
// instead. The log line names the record type only.
func (b Binding) fits(s journalapi.Snapshot) bool {
	ok := len(s.Records) <= record.MaxRecords
	if ok {
		raw, err := json.Marshal(s)
		ok = err == nil && len(raw) <= maxSnapshotBytes
	}
	if !ok {
		b.skip("snapshot_too_large")
		slog.Warn("schoology record snapshot over the journal's size limits; not sent", "record_type", s.RecordType)
	}
	return ok
}
