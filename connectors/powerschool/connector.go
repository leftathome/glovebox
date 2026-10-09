package powerschool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// lastPollKey is the checkpoint key holding the start time (RFC 3339) of
// the most recent poll that reached the portal. It survives restarts, which
// is how a restarted process knows it does not owe a catch-up poll.
const lastPollKey = "meta:last_poll"

// Kid-skip reasons: the `reason` label of powerschool_kids_skipped_total.
const (
	// skipUnknownStudent: the configured student_id is not on the account.
	skipUnknownStudent = "unknown_student"
	// skipNumberMismatch: the selected student's number is not the
	// configured student_number.
	skipNumberMismatch = "student_number_mismatch"
)

// PowerSchoolConnector implements connector.{Connector, Watcher}. One
// instance per process; the framework wires the staging backend, rule
// matcher and metrics via Wire() after constructing them.
//
// Unlike the Schoology connector there is no Listener: nothing triggers an
// out-of-schedule poll, so the process exposes no inbound endpoint beyond
// the framework's health server.
//
// Concurrency: pollNow can be entered from the framework's catch-up Poll
// and from the Watch goroutine. pollMu serializes the whole body: the
// portal's selected student is state shared by everything on the session,
// and two polls interleaving their kids would read each other's child.
type PowerSchoolConnector struct {
	client     PowerSchoolClient
	cfg        Config
	libVersion string

	// OnSessionRejected, when set, is called once the portal has refused
	// the session, just before the poll returns its PermanentError. The
	// binary uses it to record the rejected session so the restarted
	// process does not present the same dead cookie again (see
	// connector.RejectedSession). Set before Run; not safe to change
	// afterwards.
	//
	// age is how long ago this connector first loaded the session, or
	// negative when SessionFirstSeen was not set.
	OnSessionRejected func(age time.Duration)

	// SessionFirstSeen is when this connector first loaded the session it
	// is running on (SessionClock). Zero means unknown. It is used only to
	// report a session's age when it is rejected.
	SessionFirstSeen time.Time
	// PreviousRejectedAge is the age of the last session that was
	// rejected, from an earlier process, re-exported as a gauge so the
	// measurement survives the exit that follows every rejection. Negative
	// means none is known.
	PreviousRejectedAge time.Duration

	// Framework-provided; set by Wire().
	wireMu  sync.Mutex
	writer  connector.StagingBackend
	matcher *connector.RuleMatcher
	tel     *Telemetry

	// Owned by the connector.
	tz    *time.Location
	rng   *rand.Rand
	rngMu sync.Mutex // rand.Rand is NOT goroutine-safe.
	// now is the clock; tests replace it.
	now func() time.Time

	// caughtUp is set by the first Poll; see Poll.
	caughtUp atomic.Bool

	// notBefore is the end of the window that was last polled; the next
	// scheduled poll is computed from there so no window is polled twice
	// (see windowEnd). A field rather than a Watch local so it also
	// survives the framework restarting Watch. Guarded by schedMu.
	schedMu   sync.Mutex
	notBefore time.Time

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
		now:        time.Now,

		PreviousRejectedAge: -1,
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

	if c.PreviousRejectedAge >= 0 {
		tel.SetRejectedSessionAge(c.PreviousRejectedAge)
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
//
// Only the FIRST call in a process can reach the portal, and only when a
// scheduled window has opened since the last recorded poll:
//
//   - The framework's periodic re-poll is a safety net for connectors whose
//     Watch can silently stall. Here the windowed schedule in Watch is the
//     whole point (one poll per window against a real parent account), so
//     later calls are no-ops.
//   - A restart is not a reason to read the portal. The time of the last
//     poll is kept in the checkpoint; if no window has opened since, the
//     process has nothing to catch up on and waits for the schedule. A
//     process that was down through a window does poll once on startup.
func (c *PowerSchoolConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	if !c.caughtUp.CompareAndSwap(false, true) {
		slog.Debug("powerschool framework re-poll skipped; the schedule is authoritative")
		return nil
	}
	if last, ok := loadLastPoll(cp); ok {
		// Restore "this window is done" across the restart.
		c.deferUntil(windowEnd(c.cfg, last, c.tz))
		if ws := lastWindowStart(c.cfg, c.now(), c.tz); ws.IsZero() || !last.Before(ws) {
			slog.Info("powerschool catch-up poll skipped: no window has opened since the last poll",
				"last_poll", last.In(c.tz).Format(time.RFC3339))
			return nil
		}
	}
	return c.pollNow(ctx, cp, "catch_up")
}

// Watch satisfies connector.Watcher. It sleeps until the next scheduled
// window time, polls, and repeats until ctx is cancelled or a poll
// returns a PermanentError (expired session, too many failed polls), on
// which the framework exits the process.
func (c *PowerSchoolConnector) Watch(ctx context.Context, cp connector.Checkpoint) error {
	for {
		next := c.nextPoll(c.scheduleFrom(c.now()))
		slog.Debug("powerschool next poll scheduled", "scheduled_for", next.Format(time.RFC3339))

		wait := next.Sub(c.now())
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

		// Whatever the poll does, this window has had its poll.
		c.deferUntil(windowEnd(c.cfg, next, c.tz))
		if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
			return err
		}
	}
}

// scheduleFrom returns the instant the next poll is scheduled from: now,
// or the end of the last polled window when that is later.
func (c *PowerSchoolConnector) scheduleFrom(now time.Time) time.Time {
	c.schedMu.Lock()
	defer c.schedMu.Unlock()
	if c.notBefore.After(now) {
		return c.notBefore
	}
	return now
}

// deferUntil moves notBefore forward to t; it never moves it back.
func (c *PowerSchoolConnector) deferUntil(t time.Time) {
	c.schedMu.Lock()
	defer c.schedMu.Unlock()
	if t.After(c.notBefore) {
		c.notBefore = t
	}
}

// nextPoll computes the next scheduled poll time under rngMu.
func (c *PowerSchoolConnector) nextPoll(from time.Time) time.Time {
	c.rngMu.Lock()
	defer c.rngMu.Unlock()
	next, _ := computeNextPollTime(c.cfg, from, c.tz, c.rng)
	return next
}

// loadLastPoll reads the last poll time from the checkpoint.
func loadLastPoll(cp connector.Checkpoint) (time.Time, bool) {
	raw, ok := cp.Load(lastPollKey)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// pollNow runs one poll: every configured kid in turn (see pollKid).
//
// Per-child isolation is enforced here and it fails closed. A kid is read
// only when the page explicitly marks that kid's student as selected (and,
// if student_number is configured, shows that number). When the page marks
// nobody, nothing is posted and the poll is skipped for every kid. Nothing
// is staged for a kid until everything read for that kid in this poll
// agrees on one student and the selection has been confirmed again.
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

	// Record the poll before the first request: a process that dies
	// mid-poll must not treat its restart as a reason to poll again.
	started := c.now()
	if err := cp.Save(lastPollKey, started.UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("powerschool could not record the poll time; a restart may poll again", "error", err)
	}
	c.deferUntil(windowEnd(c.cfg, started, c.tz))

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
		slog.Error("powerschool session expired or rejected",
			append([]any{"library_version", c.libVersion}, errAttrs(authErr)...)...)
		// How long did this session last? The expiry in the session
		// document is a placeholder, so this is the only measurement of
		// the portal's real session lifetime. No identifier is logged.
		age := time.Duration(-1)
		if !c.SessionFirstSeen.IsZero() {
			if age = c.now().Sub(c.SessionFirstSeen); age < 0 {
				age = 0
			}
			tel.SetRejectedSessionAge(age)
			slog.Info("powerschool session rejected: age since this connector first loaded it",
				"session_age_seconds", int64(age/time.Second),
				"session_age_hours", fmt.Sprintf("%.1f", age.Hours()))
		}
		if c.OnSessionRejected != nil {
			c.OnSessionRejected(age)
		}
		// The framework logs this error verbatim, so it carries no library
		// text (errlog.go).
		return connector.PermanentError(errors.New(
			"powerschool session expired or rejected upstream (error_class=auth); a fresh session is required"))
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

// pollKids reads every configured kid, one at a time, in config order. It
// returns the summed Result, the number of kids that were read and staged
// from, and a non-nil authErr when the session was reported expired.
//
// The portal shows one student per session, so the kids cannot be read side
// by side: each kid's turn selects that kid's student, reads, checks the
// selection again and only then stages. pollNow holds pollMu for the whole
// poll, so two polls never interleave their kids either.
func (c *PowerSchoolConnector) pollKids(ctx context.Context, env Env) (total Result, kidsRead int, authErr error) {
	for i, kid := range c.cfg.Kids {
		res, read, abort := c.pollKid(ctx, env, kid)
		total.add(res)
		if read {
			kidsRead++
		}
		if res.AuthErr != nil {
			return total, kidsRead, res.AuthErr
		}
		if abort != "" {
			// The failure was about the page or the connection, not about
			// this kid; the next kid would send the same request into the
			// same failure. Skip the rest without contacting the portal.
			rest := c.cfg.Kids[i+1:]
			for _, other := range rest {
				env.Tel.RecordKidSkipped(other.Name, abort)
			}
			if len(rest) > 0 {
				slog.Warn("powerschool remaining kids skipped after a page-level failure; nothing staged for them",
					"error_class", abort, "kids_skipped", len(rest))
			}
			break
		}
	}
	return total, kidsRead, nil
}

// kidSpecific lists the selection failures that say something about one
// kid only; the poll carries on to the next kid after them. Any other
// failure to reach a kid's page aborts the remaining kids.
var kidSpecific = map[string]string{
	"not_found":               skipUnknownStudent,
	"switch_not_confirmed":    "switch_not_confirmed",
	"selection_mismatch":      "selection_mismatch",
	"student_number_mismatch": skipNumberMismatch,
}

// selectKidTraced wraps client.SelectStudent in the
// powerschool.lib.SelectStudent span.
func selectKidTraced(ctx context.Context, e Env, kid Kid) (*pslib.HomePage, error) {
	ctx, span := e.Tel.StartSpan(ctx, "powerschool.lib.SelectStudent", attribute.String("kid", kid.Name))
	defer span.End()
	return e.Client.SelectStudent(ctx, kid.StudentID)
}

// pollKid is one kid's turn: select, verify, read, verify again, stage.
//
//  1. Select the kid's student. The library switches the session if another
//     student is selected and returns the home page that confirmed it; that
//     page's grades table is this kid's. The connector checks the page
//     itself as well (verifyKidPage): explicit marker on this kid's
//     student, every grade row attributed to that student, and the
//     configured student_number if there is one.
//  2. If the assignments surface is on: read the assignment pages (one
//     scores page and one lookup per course). Each scores page carries the
//     id of the student the session had selected when it was served; every
//     one must carry this kid's bound id (binding.go).
//  3. If step 2 ran: read the home page once more and require it to still
//     mark this kid's student. With assignments off nothing was read after
//     step 1's page, so there is nothing to re-check.
//  4. Only now stage. Any failure in 1-3 discards everything read for this
//     kid in this poll: nothing is staged and no checkpoint moves.
//
// read is false when nothing was staged for the kid. abort is a non-empty
// error class when the failure is not specific to this kid and the
// remaining kids should be skipped.
func (c *PowerSchoolConnector) pollKid(ctx context.Context, env Env, kid Kid) (res Result, read bool, abort string) {
	ctx, span := env.Tel.StartSpan(ctx, "powerschool.poll.kid", attribute.String("kid", kid.Name))
	defer span.End()

	page, err := selectKidTraced(ctx, env, kid)
	if err == nil {
		err = verifyKidPage(page, kid)
	}
	if err != nil {
		class := classifyErr(err)
		env.Tel.RecordSurfaceError("home", class, kid.Name)
		res.Failed++
		if class == "auth" {
			res.AuthErr = err
			return res, false, ""
		}
		reason, specific := kidSpecific[class]
		if !specific {
			reason = class
		}
		env.Tel.RecordKidSkipped(kid.Name, reason)
		slog.Error("powerschool kid skipped: could not select and confirm this kid's student; nothing staged",
			append([]any{"kid", kid.Name, "reason", reason}, errAttrs(err)...)...)
		if specific {
			return res, false, ""
		}
		return res, false, class
	}
	grades := page.Grades

	if len(grades) == 0 {
		// Legitimate out of term; also what a changed page layout looks
		// like. Logged so a run of these is visible.
		slog.Warn("powerschool grades page listed no courses", "kid", kid.Name)
	}

	var reads []courseRead
	if c.cfg.AssignmentsEnabled() {
		var ok bool
		var fetched Result
		binding := newStudentBinding(c.cfg.Kids, env.CP, kid)
		reads, fetched, ok = readAssignments(ctx, env, kid, grades, c.cfg.Assignments, binding)
		res.add(fetched)
		if !ok {
			return res, false, ""
		}

		// Step 3: is the session still on this kid?
		if err := c.confirmStillSelected(ctx, env, kid); err != nil {
			class := classifyErr(err)
			env.Tel.RecordSurfaceError("home", class, kid.Name)
			res.Failed++
			if class == "auth" {
				res.AuthErr = err
				return res, false, ""
			}
			env.Tel.RecordKidSkipped(kid.Name, class)
			slog.Error("powerschool poll discarded for kid: the session's selected student could not be "+
				"confirmed after the reads; nothing staged",
				append([]any{"kid", kid.Name}, errAttrs(err)...)...)
			return res, false, ""
		}

		// Marker confirmed before and after, every scores page agreed: if
		// this kid had no bound id yet, this is the poll that binds it.
		learned, err := binding.learn(env.CP, kid)
		if err != nil {
			class := classifyErr(err)
			env.Tel.RecordSurfaceError(assignmentSurface, class, kid.Name)
			env.Tel.RecordKidSkipped(kid.Name, class)
			res.Failed++
			if class == "student_api_id_conflict" {
				slog.Error("powerschool poll discarded for kid: its scores pages carry the student id "+
					"bound to another kid; nothing staged", "kid", kid.Name, "error_class", class)
			} else {
				// The id could not be saved. Staging without a binding
				// that the next poll can enforce is not safe.
				slog.Error("powerschool poll discarded for kid: could not save the kid's student id "+
					"binding; nothing staged", "kid", kid.Name, "error", err)
			}
			return res, false, ""
		}
		if learned {
			slog.Info("powerschool bound kid to the student id on its scores pages; "+
				"later polls require it on every scores page", "kid", kid.Name)
		}
	}

	if c.cfg.GradesEnabled() {
		res.add(ProcessGrades(ctx, env, kid, grades))
	}
	if c.cfg.AttendanceEnabled() {
		res.add(ProcessAttendance(ctx, env, kid, grades))
	}
	if c.cfg.AssignmentsEnabled() {
		res.add(stageAssignments(ctx, env, kid, reads, c.cfg.Assignments))
	}
	return res, true, ""
}

// confirmStillSelected re-reads the home page and checks it still belongs
// to kid. It never switches: a selection that moved is a reason to discard,
// not to move it back and carry on.
func (c *PowerSchoolConnector) confirmStillSelected(ctx context.Context, env Env, kid Kid) error {
	ctx, span := env.Tel.StartSpan(ctx, "powerschool.lib.GetHomePage", attribute.String("kid", kid.Name))
	defer span.End()
	page, err := env.Client.GetHomePage(ctx)
	if err != nil {
		return err
	}
	return verifyKidPage(page, kid)
}
