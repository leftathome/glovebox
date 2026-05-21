package schoology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
)

// SchoologyConnector implements connector.{Connector, Watcher, Listener}.
// One instance per process; the framework wires the staging writer +
// rule matcher + framework metrics via Wire() after constructing them.
//
// The connector struct owns the timezone, RNG, trigger handler, poll
// signal channel, and schema drift counter from construction. Framework-
// provided resources (writer, matcher, telemetry) are attached later in
// Wire() once connector.Run has built the staging backend and metrics.
//
// Concurrency: pollNow can be entered from up to three goroutines
// simultaneously -- the framework's initial poll, the Watch goroutine's
// scheduled / triggered branches, and (theoretically) a re-poll from the
// Watch loop's ticker. pollMu serializes the whole body to make those
// safe; the trigger debouncer already coalesces back-to-back signals.
type SchoologyConnector struct {
	client     SchoologyClient
	cfg        Config
	libVersion string

	// Framework-provided; set by Wire(). wireMu guards a slow Setup
	// callback racing with an HTTP trigger that fires before Wire returns.
	wireMu  sync.Mutex
	writer  *connector.StagingWriter
	matcher *connector.RuleMatcher
	tel     *Telemetry

	// Owned by the connector.
	tz           *time.Location
	rng          *rand.Rand
	rngMu        sync.Mutex // rand.Rand is NOT goroutine-safe.
	trigger      *TriggerHandler
	pollSignal   chan struct{}
	driftCounter *SchemaDriftCounter

	pollMu sync.Mutex // serializes pollNow across Poll/Watch/trigger
}

// NewConnector constructs a SchoologyConnector. The returned value
// implements Connector, Watcher, and Listener but is NOT yet wired:
// the caller must invoke Wire(cc) from a connector.Options.Setup
// callback before the first Poll completes.
//
// Inputs:
//   - client: the schoology-go-backed (or fake) library wrapper.
//   - cfg: a fully-validated Config (run ApplyDefaults + ValidateConfig
//     before calling NewConnector).
//   - libVersion: a stable identifier for the schoology-go library
//     version, embedded in parse-failure receipts so operators can
//     correlate breakage with a specific upstream version.
//
// Environment variables consulted:
//   - SCHOOLOGY_TIMEZONE: scheduler timezone, default America/Los_Angeles
//     (per spec 12 §6.1).
//   - SCHOOLOGY_TRIGGER_TOKEN: bearer token for the POST /v1/poll
//     listener. Empty disables authentication (do not use in
//     production); the operator runbook AUTH-RECOVERY.md sets the
//     production value at deployment time.
//
// Returns an error if SCHOOLOGY_TIMEZONE is set to a name time.LoadLocation
// cannot resolve.
func NewConnector(client SchoologyClient, cfg Config, libVersion string) (*SchoologyConnector, error) {
	tz, err := loadTimezone(os.Getenv("SCHOOLOGY_TIMEZONE"))
	if err != nil {
		return nil, fmt.Errorf("schoology: load timezone: %w", err)
	}

	// pollSignal capacity 1: matches trigger.go's non-blocking send.
	// A trigger fired while a poll is already pending coalesces.
	pollSignal := make(chan struct{}, 1)

	debounce := time.Duration(cfg.Trigger.DebounceSeconds) * time.Second
	trigger := NewTriggerHandler(os.Getenv("SCHOOLOGY_TRIGGER_TOKEN"), debounce, pollSignal, nil)

	c := &SchoologyConnector{
		client:       client,
		cfg:          cfg,
		libVersion:   libVersion,
		tz:           tz,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
		trigger:      trigger,
		pollSignal:   pollSignal,
		driftCounter: NewSchemaDriftCounter(cfg.ParseFailureThreshold),
	}
	return c, nil
}

// Wire attaches the framework-provided resources. Called by main.go from
// the connector.Options.Setup callback after connector.Run has built the
// StagingWriter, RuleMatcher, and Metrics.
//
// Wire builds the schoology Telemetry around the framework's Metrics and
// attaches it to the trigger handler so that trigger requests record the
// schoology_trigger_requests_total counter.
//
// Wire is safe to call once. Subsequent calls overwrite the wired-in
// handles; the framework only ever calls Wire once per process so this
// is not exercised in production.
func (c *SchoologyConnector) Wire(cc connector.ConnectorContext) error {
	if cc.Writer == nil {
		return errors.New("schoology.Wire: ConnectorContext.Writer is nil")
	}
	if cc.Matcher == nil {
		return errors.New("schoology.Wire: ConnectorContext.Matcher is nil")
	}
	if cc.Metrics == nil {
		return errors.New("schoology.Wire: ConnectorContext.Metrics is nil")
	}

	tel, err := NewTelemetry(cc.Metrics, "glovebox.schoology", "glovebox.schoology")
	if err != nil {
		return fmt.Errorf("schoology.Wire: build telemetry: %w", err)
	}

	c.wireMu.Lock()
	c.writer = cc.Writer
	c.matcher = cc.Matcher
	c.tel = tel
	c.trigger.Tel = tel
	c.wireMu.Unlock()
	return nil
}

// Poll satisfies connector.Connector. The framework calls Poll once on
// startup (catch-up) and again from the re-poll branches of RunWatchLoop.
// splaySecs is -1 because catch-up polls aren't scheduled through the
// window scheduler — see pollNow's splay-handling.
func (c *SchoologyConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	return c.pollNow(ctx, cp, "catch_up", -1)
}

// Handler satisfies connector.Listener and returns the POST /v1/poll
// trigger handler.
func (c *SchoologyConnector) Handler() http.Handler {
	return c.trigger.Handler()
}

// Watch satisfies connector.Watcher. It runs until ctx is cancelled or a
// PermanentError condition fires (schema drift threshold crossed, or the
// schoology session expired). On either escalation the framework's
// RunWatchLoop sees the PermanentError, logs it, and os.Exit(1)s -- the
// operator runbook (docs/AUTH-RECOVERY.md) tells them what to do next.
func (c *SchoologyConnector) Watch(ctx context.Context, cp connector.Checkpoint) error {
	for {
		next, splaySecs, window := c.nextPoll(time.Now())
		slog.Debug("schoology splay computed",
			"window", window,
			"splay_seconds", splaySecs,
			"scheduled_for", next.Format(time.RFC3339),
		)

		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)

		var source string
		var pollSplay int
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			source = "scheduled"
			pollSplay = splaySecs
		case <-c.pollSignal:
			timer.Stop()
			source = "triggered"
			// Trigger fired before the scheduled tick; no splay applies.
			pollSplay = -1
		}

		if err := c.pollNow(ctx, cp, source, pollSplay); err != nil {
			return err
		}

		if escErr := c.checkDrift(); escErr != nil {
			return escErr
		}
	}
}

// nextPoll computes the next scheduled poll time and returns the splay
// info for logging. The scheduler holds the rngMu so two callers cannot
// race on the unsafe *rand.Rand.
//
// The returned splaySecs is the offset (in seconds) from the window's
// start time -- enough to satisfy the spec §13.3 "schoology splay
// computed" log without exposing scheduler internals.
func (c *SchoologyConnector) nextPoll(now time.Time) (next time.Time, splaySecs int, window string) {
	c.rngMu.Lock()
	defer c.rngMu.Unlock()
	next, _ = computeNextPollTime(c.cfg, now, c.tz, c.rng)
	// Derive splay seconds by subtracting the window's start time on the
	// scheduled day. We re-derive the window string from the returned
	// time-of-day so the log line says "08:00-09:00" rather than indexing
	// into cfg.PollSchedule.Windows.
	for _, w := range c.cfg.PollSchedule.Windows {
		startMin, _ := parseTimeOfDay(w.Start)
		endMin, _ := parseTimeOfDay(w.End)
		nMin := next.Hour()*60 + next.Minute()
		if nMin >= startMin && nMin < endMin {
			splaySecs = (next.Hour()*3600 + next.Minute()*60 + next.Second()) - startMin*60
			window = w.Start + "-" + w.End
			return next, splaySecs, window
		}
	}
	// Fall-through: the next poll didn't land in any configured window
	// (shouldn't happen given ValidateConfig requires >=1 window). Emit
	// a degraded log shape rather than panicking.
	window = "unknown"
	return next, 0, window
}

// checkDrift inspects the schema-drift counter. Returns a PermanentError
// when the consecutive-empty-with-errors count has crossed the configured
// threshold; otherwise nil. Extracted from the Watch body so the same
// logic is unit-testable without spinning up a Watch goroutine.
//
// Error message follows spec 12 §11.4: explicitly names the schema-drift
// diagnostic and points operators at the library version so alerting
// dashboards / runbooks have the right hint without reading the code.
func (c *SchoologyConnector) checkDrift() error {
	if !c.driftCounter.ShouldEscalate() {
		return nil
	}
	count := c.driftCounter.Count()
	threshold := c.driftCounter.Threshold()
	slog.Error("schoology schema drift escalation",
		"consecutive_polls", count,
		"threshold", threshold,
		"library_version", c.libVersion,
	)
	return connector.PermanentError(fmt.Errorf(
		"schoology library returned 0 items with parse errors for %d consecutive polls (threshold %d); likely upstream schema drift; investigate library version %s",
		count, threshold, c.libVersion,
	))
}

// pollNow runs one poll across all three surfaces (assignments per kid,
// feed per kid, messages once) and updates the schema-drift counter.
//
// pollNow is shared by Poll(), Watch()'s scheduled path, and the trigger
// signal path. The pollMu serializes overlapping entries; the trigger's
// own debouncer already coalesces back-to-back POSTs, but a triggered
// poll could land mid-flight with a scheduled poll and pollMu prevents
// that race.
//
// Bad-credentials handling: the processors do NOT bubble library errors
// up to the connector -- they convert them into parse-failure receipts
// and increment dedup. We detect a session-expired condition by reading
// dedup.AffectedCount("...", "auth") for each parser at the end of the
// poll. If any of them is non-zero, we log the spec §13.3
// "schoology session expired" line and return a PermanentError so the
// framework will exit the process (the operator restarts after running
// the recovery procedure in docs/AUTH-RECOVERY.md).
// splaySecs is the per-poll splay offset in seconds; -1 signals "no
// splay applies" (catch-up Poll() or trigger-driven runs). Per spec
// §13.2 and §13.3 the splay_time attribute and log field are populated
// for scheduled polls only.
func (c *SchoologyConnector) pollNow(ctx context.Context, cp connector.Checkpoint, source string, splaySecs int) error {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()

	c.wireMu.Lock()
	writer, matcher, tel := c.writer, c.matcher, c.tel
	c.wireMu.Unlock()

	if writer == nil || matcher == nil || tel == nil {
		return fmt.Errorf("schoology: connector not wired (Setup callback not run)")
	}

	pollID := uuid.New().String()
	spanAttrs := []attribute.KeyValue{
		attribute.String("trigger_source", source),
		attribute.String("poll_id", pollID),
	}
	if splaySecs >= 0 {
		spanAttrs = append(spanAttrs, attribute.Int("splay_time", splaySecs))
	}
	ctx, span := tel.StartSpan(ctx, "schoology.poll", spanAttrs...)
	defer span.End()

	tel.RecordPollByTrigger(source)

	start := time.Now()
	startFields := []any{
		"trigger_source", source,
		"poll_id", pollID,
		"kids_count", len(c.cfg.Kids),
	}
	if splaySecs >= 0 {
		startFields = append(startFields, "splay_time", splaySecs)
	}
	slog.Info("schoology poll start", startFields...)

	dedup := NewReceiptDedup()
	var itemsProduced int
	var errsLogged bool

	for _, kid := range c.cfg.Kids {
		kidCtx, kidSpan := tel.StartSpan(ctx, "schoology.poll.kid",
			attribute.String("kid", kid.Name),
		)
		// The library performs the view-child cookie switch on the first
		// per-kid call; this metric counts intent (the connector chose to
		// switch), not the underlying HTTP request.
		tel.RecordViewChildSwitch(kid.Name)

		aCount, aErrs := ProcessAssignments(kidCtx, c.client, writer, matcher, cp, dedup, tel, kid, c.libVersion)
		itemsProduced += aCount
		errsLogged = errsLogged || aErrs

		fCount, fAttach, fErrs := ProcessFeed(kidCtx, c.client, writer, matcher, cp, tel, dedup, kid, c.cfg.Attachments.MaxSizeMB, c.libVersion)
		itemsProduced += fCount + fAttach
		errsLogged = errsLogged || fErrs

		kidSpan.End()
	}

	mCount, mErrs := ProcessMessages(ctx, c.client, writer, matcher, cp, dedup, tel, c.libVersion)
	itemsProduced += mCount
	errsLogged = errsLogged || mErrs

	c.driftCounter.RecordPoll(itemsProduced, errsLogged)

	duration := time.Since(start)
	slog.Info("schoology poll complete",
		"trigger_source", source,
		"poll_id", pollID,
		"duration_ms", duration.Milliseconds(),
		"items_produced", itemsProduced,
		"errors", errsLogged,
	)

	// Bad-credentials escalation. The processors classify GetX errors via
	// classifyAssignmentsErr / classifyLibError / classifyMessagesError;
	// all three map a schoology-go IsAuthError sentinel to "auth". When a
	// poll observed at least one auth failure on any surface, we treat
	// the credential file as expired and return a PermanentError so the
	// framework exits the process and the operator runbook kicks in.
	for _, parser := range []string{"assignments", "feed", "messages"} {
		if dedup.AffectedCount(parser, "auth") > 0 {
			slog.Error("schoology session expired",
				"parser", parser,
				"recovery_doc", "docs/AUTH-RECOVERY.md",
			)
			return connector.PermanentError(fmt.Errorf(
				"schoology session expired (parser=%s); see docs/AUTH-RECOVERY.md",
				parser,
			))
		}
	}

	return nil
}
