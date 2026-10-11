package powerschool

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// The portal ends an idle session in well under an hour (measured
// 2026-10-11: a session stored at 19:23Z and never used was refused at
// 20:27Z). Three polls a day therefore never find the previous poll's
// session alive, and a login "an hour before the poll" is an hour too
// early. The owner has ruled out keeping a session alive with requests
// nobody needs. So the connector checks its session at the start of every
// poll, and when it is dead it asks for a login and waits for the result.
//
// SessionGate is that check and that request. It never logs in and holds
// no credentials: "asking" means creating the login workload's Job
// (kube.go), and "the result" is a new session document appearing in the
// mounted file.

// lastLoginRequestKey is the checkpoint key holding when this connector
// last asked for a login (RFC 3339). It survives restarts, so a crashing
// pod cannot ask over and over.
const lastLoginRequestKey = "meta:last_login_request"

const (
	// freshFileAge: a session file written this recently is the result of
	// a login that has just happened. If the portal already refuses it,
	// another login will not help; none is requested.
	freshFileAge = 2 * time.Minute
	// sessionPollInterval is how often the session file is re-read while
	// waiting for a new session.
	sessionPollInterval = 5 * time.Second
	// minLoginInterval is the floor under LoginOnDemandConfig's
	// min_interval_minutes.
	minLoginInterval = 20 * time.Minute
)

// LoginRequester is the Kubernetes side of an on-demand login. KubeClient
// implements it.
type LoginRequester interface {
	// ListJobs returns the login Jobs (LoginJobSelector), running or not.
	ListJobs(ctx context.Context, labelSelector string) ([]KubeJob, error)
	// CreateJobFromCronJob starts the login workload and returns the
	// Job's name.
	CreateJobFromCronJob(ctx context.Context, cronJobName string, now time.Time) (string, error)
	GetJob(ctx context.Context, name string) (KubeJob, error)
}

// SessionSource is where sessions come from and how a client is made of
// one. main wires the real ones.
type SessionSource struct {
	// Load reads the current session document. expired is set when its
	// expires_at has passed. An error means there is no usable document.
	Load func(now time.Time) (session *pslib.SessionExport, expired bool, err error)
	// Build makes the read-only client, and its probe, for a session.
	Build func(session *pslib.SessionExport) (PowerSchoolClient, ProbeFunc, error)
	// FileAge returns how long ago the session file was written; ok is
	// false when that cannot be known.
	FileAge func(now time.Time) (age time.Duration, ok bool)
	// FirstSeen returns when the session with this fingerprint was first
	// loaded by this connector (SessionClock.FirstSeen). May be nil.
	FirstSeen func(fingerprint string, now time.Time) time.Time
}

// GateOutcome is what SessionGate.Ensure tells the poll to do.
type GateOutcome int

const (
	// GateSkip: do not poll. Nothing was established, or a login was
	// asked for and did not produce a usable session in time. Nothing is
	// marked, nothing is counted as a failure.
	GateSkip GateOutcome = iota
	// GateLive: poll with Client.
	GateLive
	// GateDead: the session is dead and on-demand login is off. The
	// caller takes the rejected-session path (the process exits and waits
	// for somebody else to replace the session).
	GateDead
)

// GateResult is the answer of one Ensure call.
type GateResult struct {
	Outcome GateOutcome
	Client  PowerSchoolClient
	// Reason says why, for the log: a SessionVerdict reason or a skip
	// reason from this file.
	Reason string
}

// SessionGate decides, at the start of a poll, whether there is a live
// session to poll with, and gets one if it can.
type SessionGate struct {
	source SessionSource
	// requester and onDemand are nil / zero when on-demand login is off.
	requester LoginRequester
	onDemand  LoginOnDemandConfig

	// Clock and sleep; tests replace them.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// OnDead, when set, is called each time a session is found dead, with
	// how long this connector has had it (negative when unknown).
	OnDead func(age time.Duration, reason string)

	mu          sync.Mutex
	fingerprint string
	client      PowerSchoolClient
	probe       ProbeFunc
	firstSeen   time.Time
	expired     bool
}

// NewSessionGate builds a gate. requester may be nil; on-demand login is
// then off whatever cfg says.
func NewSessionGate(source SessionSource, cfg LoginOnDemandConfig, requester LoginRequester) *SessionGate {
	g := &SessionGate{
		source: source,
		Now:    time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
	if cfg.Enabled && requester != nil {
		g.requester = requester
		g.onDemand = cfg
	}
	return g
}

// OnDemand reports whether the gate asks for logins.
func (g *SessionGate) OnDemand() bool { return g != nil && g.requester != nil }

// Fingerprint returns the fingerprint of the session currently in use
// ("" when there is none).
func (g *SessionGate) Fingerprint() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fingerprint
}

// SessionAge returns how long this connector has had the session in use,
// or a negative duration when there is none. Safe on a nil gate.
func (g *SessionGate) SessionAge() time.Duration {
	if g == nil {
		return -1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fingerprint == "" || g.firstSeen.IsZero() {
		return -1
	}
	if age := g.Now().Sub(g.firstSeen); age > 0 {
		return age
	}
	return 0
}

// reload reads the session document and, when it is a different session
// from the one in use, builds a client for it. It reports whether a usable
// document is in place. A document that cannot be read leaves the session
// in use untouched: the file may be mid-rewrite.
func (g *SessionGate) reload() (have bool) {
	now := g.Now()
	session, expired, err := g.source.Load(now)
	if err != nil {
		return g.Fingerprint() != ""
	}
	fp := SessionFingerprint(session)

	g.mu.Lock()
	defer g.mu.Unlock()
	if fp == g.fingerprint {
		g.expired = expired
		return true
	}
	client, probe, err := g.source.Build(session)
	if err != nil {
		slog.Error("powerschool: could not build a client for the session in the session file", "error", err)
		return g.fingerprint != ""
	}
	g.fingerprint, g.client, g.probe, g.expired = fp, client, probe, expired
	g.firstSeen = now
	if g.source.FirstSeen != nil {
		g.firstSeen = g.source.FirstSeen(fp, now)
	}
	return true
}

// check classifies the session in use with one GET (none when there is no
// session, or its document says it has expired).
func (g *SessionGate) check(ctx context.Context, have bool) SessionVerdict {
	g.mu.Lock()
	probe, expired := g.probe, g.expired
	g.mu.Unlock()
	switch {
	case !have || probe == nil:
		return SessionVerdict{SessionDead, ReasonNoSession}
	case expired:
		// Advisory in on-demand mode: it leads to a request, not to an
		// exit. The library would refuse to send this session anyway.
		return SessionVerdict{SessionDead, ReasonExpiredDocument}
	}
	return probe(ctx)
}

// Ensure is called at the start of every poll, before any kid is read.
//
//	live     -> GateLive with the client to poll with
//	unknown  -> GateSkip; nothing changes and no login is asked for
//	dead     -> without on-demand login: GateDead
//	            with it: ask for a login (at most once), wait for a new
//	            session, check it, and answer GateLive or GateSkip
func (g *SessionGate) Ensure(ctx context.Context, cp connector.Checkpoint) GateResult {
	have := g.reload()
	verdict := g.check(ctx, have)

	switch verdict.State {
	case SessionLive:
		return g.live(verdict.Reason)
	case SessionUnknown:
		slog.Warn("powerschool: could not tell whether the session is still accepted; skipping this poll, changing nothing",
			"reason", verdict.Reason)
		return GateResult{Outcome: GateSkip, Reason: "session_state_unknown:" + verdict.Reason}
	}

	g.reportDead(verdict.Reason)
	if !g.OnDemand() {
		return GateResult{Outcome: GateDead, Reason: verdict.Reason}
	}
	return g.requestAndWait(ctx, cp)
}

func (g *SessionGate) live(reason string) GateResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	return GateResult{Outcome: GateLive, Client: g.client, Reason: reason}
}

// reportDead writes the line an operator greps for. A dead session is
// routine; this is INFO-level news, not an error, and it is never worded as
// a parse failure.
func (g *SessionGate) reportDead(reason string) {
	g.mu.Lock()
	firstSeen, fp := g.firstSeen, g.fingerprint
	g.mu.Unlock()

	age := time.Duration(-1)
	attrs := []any{"reason", reason, "login_on_demand", g.OnDemand()}
	if fp != "" && !firstSeen.IsZero() {
		if age = g.Now().Sub(firstSeen); age < 0 {
			age = 0
		}
		attrs = append(attrs,
			"session_age_seconds", int64(age/time.Second),
			"session_age_minutes", fmt.Sprintf("%.1f", age.Minutes()))
	}
	slog.Info("powerschool session is no longer accepted", attrs...)
	if g.OnDead != nil {
		g.OnDead(age, reason)
	}
}

// skip logs why a poll is not going to happen and returns GateSkip.
func skipPoll(reason, message string, attrs ...any) GateResult {
	slog.Warn("powerschool: "+message+"; skipping this poll", append([]any{"skip_reason", reason}, attrs...)...)
	return GateResult{Outcome: GateSkip, Reason: reason}
}

// requestAndWait is the on-demand path. One call makes at most one login
// request.
//
// A login is NOT requested when:
//
//   - the session file was written less than two minutes ago (a login has
//     just happened; its session is the one that was refused);
//   - a login could still be running at 16:00-21:00 Pacific
//     (psportal.FloorBlocksLoginAt: 15:57:30 to 21:00) -- the login
//     workload would refuse, so no Job is created at all;
//   - this connector asked less than min_interval_minutes ago (kept in the
//     checkpoint, so it holds across restarts);
//   - a login Job, scheduled or on demand, is already running -- then this
//     waits for that one instead.
func (g *SessionGate) requestAndWait(ctx context.Context, cp connector.Checkpoint) GateResult {
	now := g.Now()
	dead := g.Fingerprint()

	if g.source.FileAge != nil && dead != "" {
		if age, ok := g.source.FileAge(now); ok && age < freshFileAge {
			return skipPoll("session_file_fresh",
				"the session file was written moments ago and the portal already refuses it; not asking for another login",
				"session_file_age_seconds", int64(age/time.Second))
		}
	}
	if psportal.FloorBlocksLoginAt(now) {
		return skipPoll("blackout",
			"no login is asked for between 15:57:30 and 21:00 Pacific (a login then would sign the account's owner out)",
			"local_time", now.In(psportal.FloorLocation()).Format("15:04:05 MST"))
	}

	interval := g.onDemand.minInterval()
	wait := g.onDemand.wait()
	jobName := ""

	if last, ok := loadLastLoginRequest(cp); ok && now.Sub(last) < interval && !last.After(now) {
		since := now.Sub(last)
		if since >= wait {
			return skipPoll("login_requested_recently",
				"a login was already asked for and produced no usable session; not asking again yet",
				"requested_seconds_ago", int64(since/time.Second),
				"min_interval_seconds", int64(interval/time.Second))
		}
		// Asked moments ago (a restart in mid-wait): wait out the rest.
		wait -= since
		slog.Info("powerschool: a login was asked for moments ago; waiting for its session",
			"requested_seconds_ago", int64(since/time.Second))
	} else {
		jobs, err := g.requester.ListJobs(ctx, LoginJobSelector)
		if err != nil {
			return skipPoll("login_request_failed",
				"could not list the login Jobs, so cannot tell whether a login is already running; not asking for one",
				"error", err.Error())
		}
		active := ""
		for _, j := range jobs {
			if !j.Finished() {
				active = j.Name
				break
			}
		}
		if active != "" {
			jobName = active
			slog.Info("powerschool: a login Job is already running; waiting for its session instead of asking for another",
				"job", active)
		} else {
			// Recorded before the request: if the answer is lost, the
			// request may still have been honoured.
			if err := cp.Save(lastLoginRequestKey, now.UTC().Format(time.RFC3339)); err != nil {
				return skipPoll("login_request_failed",
					"could not record the login request, so could not limit how often one is made; not asking",
					"error", err.Error())
			}
			name, err := g.requester.CreateJobFromCronJob(ctx, g.onDemand.CronJobName, now)
			if err != nil {
				return skipPoll("login_request_failed",
					"could not create the login Job; check the connector's Role and the CronJob name",
					"cronjob", g.onDemand.CronJobName, "error", err.Error())
			}
			jobName = name
			slog.Info("powerschool: asked for a login; waiting for the new session",
				"job", name, "wait_seconds", int64(wait/time.Second))
		}
	}

	return g.waitForNewSession(ctx, dead, jobName, wait)
}

// waitForNewSession re-reads the session file every few seconds until it
// holds a session other than the dead one, then checks that one. It
// contacts the portal only for that single check.
func (g *SessionGate) waitForNewSession(ctx context.Context, dead, jobName string, wait time.Duration) GateResult {
	deadline := g.Now().Add(wait)
	jobState := ""
	for tick := 0; ; tick++ {
		if have := g.reload(); have && g.Fingerprint() != dead {
			verdict := g.check(ctx, true)
			if verdict.State == SessionLive {
				slog.Info("powerschool: a new session is in place and accepted; polling", "job", jobName)
				return g.live(verdict.Reason)
			}
			if verdict.State == SessionDead {
				g.reportDead(verdict.Reason)
			}
			return skipPoll("new_session_not_accepted",
				"a new session arrived but the portal does not accept it; not asking for another login in this poll",
				"job", jobName, "session_state", verdict.State.String(), "reason", verdict.Reason)
		}

		// Every third look, see how the Job is doing. A Job that has
		// failed will not produce a session; one that succeeded may have
		// (its session reaches the file a little after the Job ends), or
		// may have declined to log in -- its log says which.
		if jobName != "" && tick%3 == 2 {
			if j, err := g.requester.GetJob(ctx, jobName); err == nil {
				jobState = j.State()
				if j.FailedCondition {
					return skipPoll("login_job_failed",
						"the login Job failed, so no new session is coming; see its log and termination message",
						"job", jobName, "job_state", jobState)
				}
			}
		}

		if !g.Now().Before(deadline) {
			if jobState == "" && jobName != "" {
				if j, err := g.requester.GetJob(ctx, jobName); err == nil {
					jobState = j.State()
				}
			}
			message := "no new session appeared in time"
			if jobState == "succeeded" {
				message = "the login Job finished without a new session reaching this pod: it declined to log in " +
					"(blackout, a session it considers good, a blocked or unrecognised sign-in), or the Secret has not synced yet; see its log"
			}
			return skipPoll("no_new_session", message,
				"job", jobName, "job_state", jobState, "waited_seconds", int64(wait/time.Second))
		}
		if err := g.Sleep(ctx, sessionPollInterval); err != nil {
			return skipPoll("interrupted", "interrupted while waiting for a new session", "job", jobName)
		}
	}
}

// loadLastLoginRequest reads when a login was last asked for.
func loadLastLoginRequest(cp connector.Checkpoint) (time.Time, bool) {
	raw, ok := cp.Load(lastLoginRequestKey)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
