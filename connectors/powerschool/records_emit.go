package powerschool

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
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
func NewRecordsFactory(cfg PowerSchoolRecords, stateDir string, now func() time.Time) func(metric.Meter) (RecordSink, error) {
	if !cfg.Enabled {
		return nil
	}
	return func(meter metric.Meter) (RecordSink, error) {
		e, err := records.New(cfg.Config, records.Options{StateDir: stateDir, Connector: sourceName, Meter: meter, Now: now})
		if err != nil {
			return nil, err
		}
		slog.Info("powerschool records enabled: snapshots are spooled and sent to the journal in shadow",
			"journal", cfg.JournalURL)
		return e, nil
	}
}

// recordSink returns the sink Wire built, or nil when records are off.
func (c *PowerSchoolConnector) recordSink() RecordSink {
	c.wireMu.Lock()
	defer c.wireMu.Unlock()
	return c.records
}

// startRecords starts the sink's delivery loop for the life of ctx (one
// Watch). The framework restarts Watch with a new context; the loop of the
// previous Watch, whose context has ended, is waited for first, so at most
// one runs. With records off it does nothing.
func (c *PowerSchoolConnector) startRecords(ctx context.Context) {
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

// emitKidRecords builds the snapshots of a kid's verified turn from the
// pages the turn read and hands them to the sink. It runs after the items
// were staged and touches nothing of theirs: a builder panic is recovered
// and counted, and an Emit error is logged; neither changes what was
// staged or checkpointed. Each surface follows its switch and its rule,
// exactly as for items.
func (c *PowerSchoolConnector) emitKidRecords(sink RecordSink, env Env, kid Kid, readAt time.Time,
	grades []*pslib.Grade, reads []courseRead, extras *extraPages) {
	defer func() {
		if r := recover(); r != nil {
			env.Tel.RecordRecordRowSkipped("builder_panic")
			slog.Error("powerschool records for this kid's turn failed; items are unaffected", "kid", kid.Name)
		}
	}()
	bind := func(surface string) (Binding, bool) {
		b, ok := subjectFor(env.Matcher, kid, surface)
		b.OnSkip = env.Tel.RecordRecordRowSkipped
		return b, ok
	}
	var snaps []journalapi.Snapshot
	if c.cfg.GradesEnabled() {
		if b, ok := bind(gradeSurface); ok {
			snaps = append(snaps, BuildCourseMarks(b, grades, readAt)...)
		}
	}
	if c.cfg.AttendanceEnabled() {
		if b, ok := bind(attendanceSurface); ok {
			snaps = append(snaps, BuildAttendanceTotals(b, grades, readAt)...)
		}
	}
	if c.cfg.AssignmentsEnabled() {
		if b, ok := bind(assignmentSurface); ok {
			snaps = append(snaps, BuildAssignments(b, reads, env.tz(), readAt, sink.OnCut)...)
		}
	}
	if extras != nil {
		if c.cfg.AttendanceDaysEnabled() && extras.attendance != nil {
			if b, ok := bind(attendanceDaySurface); ok {
				snaps = append(snaps, BuildAttendanceMarks(b, extras.attendance, c.cfg.AttendanceDays, readAt)...)
				snaps = append(snaps, BuildAttendanceDays(b, extras.attendance, c.cfg.AttendanceDays,
					c.cfg.Records.AttendanceDaySettleSchoolDays, readAt, env.tz(), readAt)...)
			}
		}
		if c.cfg.TermGradesEnabled() && extras.history != nil {
			if b, ok := bind(termGradeSurface); ok {
				snaps = append(snaps, BuildTermGrades(b, extras.history, readAt)...)
			}
		}
		if c.cfg.TeacherCommentsEnabled() && len(extras.comments) > 0 {
			if b, ok := bind(teacherCommentSurface); ok {
				snaps = append(snaps, BuildTermComments(b, extras.comments, readAt, sink.OnCut)...)
			}
		}
	}
	if len(snaps) == 0 {
		return
	}
	if err := sink.Emit(snaps); err != nil {
		slog.Warn("powerschool record snapshots were not all spooled; items are unaffected",
			"kid", kid.Name, "snapshots", len(snaps))
	}
}
