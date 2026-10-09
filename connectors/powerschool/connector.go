package powerschool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// PowerSchoolConnector implements connector.{Connector, Watcher}. One
// instance per process; the framework wires the staging backend, rule
// matcher and metrics via Wire() after constructing them.
//
// Unlike the Schoology connector there is no Listener: nothing triggers an
// out-of-schedule poll, so the process exposes no inbound endpoint beyond
// the framework's health server.
//
// Concurrency: pollNow can be entered from the framework's initial and
// periodic polls and from the Watch goroutine. pollMu serializes the whole
// body; the session's selected student is shared state upstream and two
// interleaved polls must not race on it.
type PowerSchoolConnector struct {
	client     PowerSchoolClient
	cfg        Config
	libVersion string

	// Framework-provided; set by Wire().
	wireMu  sync.Mutex
	writer  connector.StagingBackend
	matcher *connector.RuleMatcher
	tel     *Telemetry

	// Owned by the connector.
	tz    *time.Location
	rng   *rand.Rand
	rngMu sync.Mutex // rand.Rand is NOT goroutine-safe.

	pollMu sync.Mutex // serializes pollNow across Poll/Watch
	// failedPolls counts consecutive polls that read no student at all.
	// Guarded by pollMu.
	failedPolls int
}

// NewConnector constructs a PowerSchoolConnector. The returned value is
// NOT yet wired: the caller must invoke Wire(cc) from a
// connector.Options.Setup callback before the first Poll completes.
//
// cfg must already have been through ApplyDefaults + ValidateConfig.
// libVersion is the powerschool-go version, logged with every escalation
// so breakage can be correlated with an upstream change.
//
// POWERSCHOOL_TIMEZONE sets the scheduler and due-date timezone (default
// America/Los_Angeles); an unresolvable name is an error.
func NewConnector(client PowerSchoolClient, cfg Config, libVersion string) (*PowerSchoolConnector, error) {
	if client == nil {
		return nil, errors.New("powerschool: nil client")
	}
	tz, err := loadTimezone(os.Getenv("POWERSCHOOL_TIMEZONE"))
	if err != nil {
		return nil, fmt.Errorf("powerschool: load timezone: %w", err)
	}
	return &PowerSchoolConnector{
		client:     client,
		cfg:        cfg,
		libVersion: libVersion,
		tz:         tz,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// Wire attaches the framework-provided resources. Called by main.go from
// the connector.Options.Setup callback.
func (c *PowerSchoolConnector) Wire(cc connector.ConnectorContext) error {
	if cc.Backend == nil {
		return errors.New("powerschool.Wire: ConnectorContext.Backend is nil")
	}
	if cc.Matcher == nil {
		return errors.New("powerschool.Wire: ConnectorContext.Matcher is nil")
	}
	if cc.Metrics == nil {
		return errors.New("powerschool.Wire: ConnectorContext.Metrics is nil")
	}

	tel, err := NewTelemetry(cc.Metrics, "glovebox.powerschool", "glovebox.powerschool")
	if err != nil {
		return fmt.Errorf("powerschool.Wire: build telemetry: %w", err)
	}
	if c.cfg.ConfigIdentity != nil {
		cc.Backend.SetConfigIdentity(c.cfg.ConfigIdentity)
	}

	c.wireMu.Lock()
	c.writer = cc.Backend
	c.matcher = cc.Matcher
	c.tel = tel
	c.wireMu.Unlock()
	return nil
}

// Poll satisfies connector.Connector. The framework calls Poll once on
// startup (catch-up) and again from the re-poll branches of RunWatchLoop.
func (c *PowerSchoolConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	return c.pollNow(ctx, cp, "catch_up")
}

// Watch satisfies connector.Watcher. It sleeps until the next scheduled
// window time, polls, and repeats until ctx is cancelled or a poll
// returns a PermanentError (expired session, too many failed polls), on
// which the framework exits the process.
func (c *PowerSchoolConnector) Watch(ctx context.Context, cp connector.Checkpoint) error {
	for {
		next := c.nextPoll(time.Now())
		slog.Debug("powerschool next poll scheduled", "scheduled_for", next.Format(time.RFC3339))

		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
			return err
		}
	}
}

// nextPoll computes the next scheduled poll time under rngMu.
func (c *PowerSchoolConnector) nextPoll(now time.Time) time.Time {
	c.rngMu.Lock()
	defer c.rngMu.Unlock()
	next, _ := computeNextPollTime(c.cfg, now, c.tz, c.rng)
	return next
}

// pollNow runs one poll: list the account's students, work out which one
// the session is showing, and read the enabled surfaces for the configured
// kid that matches.
//
// Per-child isolation is enforced here. The library returns the selected
// student's records whatever id it is asked for, so a kid is only read
// when its configured student_id IS the selected student. Every other kid
// is skipped with a logged reason and stages nothing -- a kid's label is
// never attached to records the connector cannot attribute to that kid.
//
// Returns nil for transient failures (logged, counted, retried at the next
// window) and a PermanentError when the session is expired or when
// failure_threshold consecutive polls read no student.
func (c *PowerSchoolConnector) pollNow(ctx context.Context, cp connector.Checkpoint, source string) error {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()

	c.wireMu.Lock()
	writer, matcher, tel := c.writer, c.matcher, c.tel
	c.wireMu.Unlock()

	if writer == nil || matcher == nil || tel == nil {
		return fmt.Errorf("powerschool: connector not wired (Setup callback not run)")
	}

	pollID := uuid.New().String()
	ctx, span := tel.StartSpan(ctx, "powerschool.poll",
		attribute.String("trigger_source", source),
		attribute.String("poll_id", pollID),
	)
	defer span.End()

	tel.RecordPollByTrigger(source)
	start := time.Now()
	slog.Info("powerschool poll start",
		"trigger_source", source, "poll_id", pollID, "kids_count", len(c.cfg.Kids))

	env := Env{Client: c.client, Writer: writer, Matcher: matcher, CP: cp, Tel: tel, TZ: c.tz}

	total, kidsRead, authErr := c.pollKids(ctx, env)

	slog.Info("powerschool poll complete",
		"trigger_source", source,
		"poll_id", pollID,
		"duration_ms", time.Since(start).Milliseconds(),
		"items_produced", total.Staged,
		"failures", total.Failed,
		"kids_read", kidsRead,
	)

	if authErr != nil {
		slog.Error("powerschool session expired", "library_version", c.libVersion, "error", authErr)
		return connector.PermanentError(fmt.Errorf("powerschool session expired or rejected: %w", authErr))
	}

	if kidsRead > 0 {
		c.failedPolls = 0
		return nil
	}
	c.failedPolls++
	if c.cfg.FailureThreshold > 0 && c.failedPolls >= c.cfg.FailureThreshold {
		slog.Error("powerschool repeated poll failure",
			"consecutive_polls", c.failedPolls,
			"threshold", c.cfg.FailureThreshold,
			"library_version", c.libVersion,
		)
		return connector.PermanentError(fmt.Errorf(
			"powerschool read no student for %d consecutive polls (threshold %d); "+
				"check the session, the configured student ids and library version %s",
			c.failedPolls, c.cfg.FailureThreshold, c.libVersion,
		))
	}
	return nil
}

// pollKids does the reading for pollNow. It returns the summed Result, the
// number of kids whose grades page was read, and a non-nil authErr when
// the session was reported expired.
func (c *PowerSchoolConnector) pollKids(ctx context.Context, env Env) (total Result, kidsRead int, authErr error) {
	students, err := c.client.GetStudents(ctx)
	if err == nil {
		var selected string
		selected, err = SelectedStudentID(students)
		if err == nil {
			return c.pollSelected(ctx, env, students, selected)
		}
	}

	class := classifyErr(err)
	env.Tel.RecordSurfaceError("students", class, "")
	slog.Warn("powerschool student listing failed", "error_class", class, "error", err)
	total.Failed++
	if class == "auth" {
		return total, 0, err
	}
	return total, 0, nil
}

// pollSelected reads the surfaces for the one configured kid whose
// student_id equals selected, and records why each other kid was skipped.
func (c *PowerSchoolConnector) pollSelected(ctx context.Context, env Env, students []*pslib.Student, selected string) (total Result, kidsRead int, authErr error) {
	known := make(map[string]bool, len(students))
	for _, s := range students {
		if s != nil {
			known[s.ID] = true
		}
	}

	for _, kid := range c.cfg.Kids {
		switch {
		case !known[kid.StudentID]:
			// Wrong id in config, or the child left the account.
			slog.Error("powerschool kid not on this account", "kid", kid.Name)
			env.Tel.RecordKidSkipped(kid.Name, "unknown_student")
			continue
		case kid.StudentID != selected:
			slog.Warn("powerschool kid skipped: session has another student selected "+
				"and the library cannot switch students", "kid", kid.Name)
			env.Tel.RecordKidSkipped(kid.Name, "not_selected")
			continue
		}

		res, read := c.pollKid(ctx, env, kid)
		total.add(res)
		if read {
			kidsRead++
		}
		if res.AuthErr != nil {
			return total, kidsRead, res.AuthErr
		}
	}
	return total, kidsRead, nil
}

// pollKid reads one kid's grades page and runs the enabled surfaces over
// it. read is false when the grades page itself could not be fetched.
func (c *PowerSchoolConnector) pollKid(ctx context.Context, env Env, kid Kid) (res Result, read bool) {
	ctx, span := env.Tel.StartSpan(ctx, "powerschool.poll.kid", attribute.String("kid", kid.Name))
	defer span.End()

	grades, err := c.client.GetGrades(ctx, kid.StudentID)
	if err != nil {
		class := classifyErr(err)
		env.Tel.RecordSurfaceError(gradeSurface, class, kid.Name)
		slog.Warn("powerschool grades read failed", "kid", kid.Name, "error_class", class, "error", err)
		res.Failed++
		if class == "auth" {
			res.AuthErr = err
		}
		return res, false
	}
	if len(grades) == 0 {
		// Legitimate out of term; also what a changed page layout looks
		// like. Logged so a run of these is visible.
		slog.Warn("powerschool grades page listed no courses", "kid", kid.Name)
	}

	if c.cfg.GradesEnabled() {
		res.add(ProcessGrades(ctx, env, kid, grades))
	}
	if c.cfg.AttendanceEnabled() {
		res.add(ProcessAttendance(ctx, env, kid, grades))
	}
	if c.cfg.AssignmentsEnabled() {
		res.add(ProcessAssignments(ctx, env, kid, grades, c.cfg.Assignments))
	}
	return res, true
}
