package powerschool

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// The portal ends an idle session in well under an hour (measured
// 2026-10-11: dead at 30 and at 45 minutes idle, alive at 4). Three polls a
// day therefore never find the previous poll's session alive, and a login
// "an hour before the poll" is an hour too early. The owner has ruled out
// keeping a session alive with requests nobody needs. So the connector
// checks its session at the start of every poll, and when it is dead it
// ASKS for a login and waits for the result.
//
// SessionGate is that check and that request.
//
// What "asks" means, exactly, because this process parses untrusted pages
// from the portal and must be assumed compromisable: it writes one short
// string -- a time and a fingerprint prefix (psportal.FormatLoginRequest)
// -- into one field of one ConfigMap. That is the whole of its reach. It
// cannot create a Job or a pod, cannot read a Secret, holds no credential
// and cannot log in. The login workload reads the field on its own
// schedule and decides by its own rules -- blackout, latch, a minimum
// interval between logins -- whether to act. The worst a compromised
// connector can do with this is cause logins at the rate, and in the
// hours, the login workload allows anyway.

// Checkpoint keys. All three survive restarts.
const (
	// lastLoginRequestKey: when this connector last asked for a login
	// (RFC 3339).
	lastLoginRequestKey = "meta:last_login_request"
	// unansweredKey: "<count> <fingerprint>" -- how many requests in a row
	// did not lead to a session the portal accepts, and the session file's
	// fingerprint when the last of them gave up.
	unansweredKey = "meta:login_requests_unanswered"
	// backoffResetKey: the last operator reset token honoured.
	backoffResetKey = "meta:login_backoff_reset"
)

const (
	// freshFileAge: a session file written this recently is the result of
	// a login that has just happened. If the portal already refuses it,
	// another login will not help; none is requested.
	freshFileAge = 2 * time.Minute
	// sessionPollInterval is how often the session file is re-read while
	// waiting for a new session.
	sessionPollInterval = 5 * time.Second
	// minLoginInterval is the floor under LoginOnDemandConfig's
	// min_interval_minutes. (The login workload enforces its own, which
	// is the one that counts; this one keeps an honest connector quiet.)
	minLoginInterval = 20 * time.Minute
	// maxUnansweredRequests: after this many requests in a row that did
	// not produce a session the portal accepts, the connector stops asking
	// and raises its permanent-failure path. Something is wrong that
	// another login will not fix, and each login costs the account's
	// owner a possible sign-out.
	maxUnansweredRequests = 2
)

// LoginRequester delivers a login request. ConfigMapRequester is the
// production one.
type LoginRequester interface {
	RequestLogin(ctx context.Context, request string) error
}

// ConfigMapRequester writes the request into one key of one ConfigMap,
// with a client that can do nothing else (internal/kubecm).
type ConfigMapRequester struct {
	Client    *kubecm.Client
	ConfigMap string
}

// RequestLogin implements LoginRequester.
func (r ConfigMapRequester) RequestLogin(ctx context.Context, request string) error {
	return r.Client.SetField(ctx, r.ConfigMap, psportal.LoginRequestKey, request)
}

// SessionSource is where sessions come from and how a client is made of
// one. main wires the real ones.
type SessionSource struct {
	// Load reads the current session document. expired is set when its
	// expires_at has passed. An error means there is no usable document.
	Load func(now time.Time) (session *pslib.SessionExport, expired bool, err error)
	// Build makes the read-only client, and its probe, for a session.
	// reset, when not nil, forgets the cookies the client's gateway
	// overlay has collected; the gate calls it at the start of every poll.
	Build func(session *pslib.SessionExport) (client PowerSchoolClient, probe ProbeFunc, reset func(), err error)
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
	// GateSkip: do not poll. See GateResult.Failed for whether that is a
	// failure.
	GateSkip GateOutcome = iota
	// GateLive: poll with Client.
	GateLive
	// GateDead: the session is dead and on-demand login is off. The
	// caller takes the rejected-session path (the process exits and waits
	// for somebody else to replace the session).
	GateDead
	// GateBlocked: the session is dead, logins were asked for and did not
	// help, and the connector has stopped asking. The caller raises its
	// permanent-failure path.
	GateBlocked
)

// GateResult is the answer of one Ensure call.
type GateResult struct {
	Outcome GateOutcome
	Client  PowerSchoolClient
	// Reason says why, for the log and the metric: a SessionVerdict reason
	// or a skip reason from this file.
	Reason string
	// Failed marks a skip that is a failure of the poll: a login was
	// asked for and no usable session came of it. Such a skip counts
	// toward failure_threshold. A skip that is nobody's fault (the state
	// could not be established, the blackout) does not.
	Failed bool
}

// SessionGate decides, at the start of a poll, whether there is a live
// session to poll with, and asks for one if it can.
type SessionGate struct {
	source SessionSource
	// requester and onDemand are nil / zero when on-demand login is off.
	requester LoginRequester
	onDemand  LoginOnDemandConfig

	// BackoffResetToken, when not empty and different from the last one
	// honoured, clears the unanswered-request count at the next poll. It
	// is how an operator says "try again" (env
	// POWERSCHOOL_LOGIN_BACKOFF_RESET; any new value).
	BackoffResetToken string

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
	reset       func()
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

// OnDemand reports whether the gate asks for logins. Safe on a nil gate.
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
	client, probe, reset, err := g.source.Build(session)
	if err != nil {
		slog.Error("powerschool: could not build a client for the session in the session file", "error", err)
		return g.fingerprint != ""
	}
	g.fingerprint, g.client, g.probe, g.reset, g.expired = fp, client, probe, reset, expired
	g.firstSeen = now
	if g.source.FirstSeen != nil {
		g.firstSeen = g.source.FirstSeen(fp, now)
	}
	return true
}

// resetOverlay forgets the gateway cookies the client in use collected
// during an earlier poll. They were good for that poll; a new poll starts
// from the stored session alone.
func (g *SessionGate) resetOverlay() {
	g.mu.Lock()
	reset := g.reset
	g.mu.Unlock()
	if reset != nil {
		reset()
	}
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
//	            session, check it, and answer GateLive, GateSkip or
//	            GateBlocked
func (g *SessionGate) Ensure(ctx context.Context, cp connector.Checkpoint) GateResult {
	g.honourReset(cp)
	g.resetOverlay()
	have := g.reload()
	verdict := g.check(ctx, have)

	switch verdict.State {
	case SessionLive:
		clearUnanswered(cp)
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

// honourReset clears the unanswered-request count when the operator has
// supplied a reset token this connector has not seen before.
func (g *SessionGate) honourReset(cp connector.Checkpoint) {
	token := strings.TrimSpace(g.BackoffResetToken)
	if token == "" {
		return
	}
	if seen, ok := cp.Load(backoffResetKey); ok && seen == token {
		return
	}
	if err := cp.Save(backoffResetKey, token); err != nil {
		slog.Warn("powerschool: could not record the login backoff reset; it will be applied again", "error", err)
	}
	if n, _ := loadUnanswered(cp); n > 0 {
		slog.Info("powerschool: login request backoff cleared by the operator", "unanswered_requests", n)
	}
	clearUnanswered(cp)
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

// skipPoll logs why a poll is not going to happen and returns GateSkip.
func skipPoll(reason, message string, attrs ...any) GateResult {
	slog.Warn("powerschool: "+message+"; skipping this poll", append([]any{"skip_reason", reason}, attrs...)...)
	return GateResult{Outcome: GateSkip, Reason: reason}
}

// failPoll is skipPoll for a skip that is a failure: a login was asked for
// and nothing usable came of it.
func failPoll(reason, message string, attrs ...any) GateResult {
	slog.Error("powerschool: "+message+"; this poll is lost", append([]any{"skip_reason", reason}, attrs...)...)
	return GateResult{Outcome: GateSkip, Reason: reason, Failed: true}
}

// requestAndWait is the on-demand path. One call makes at most one login
// request, and a poll calls it at most once: there is never a second
// request in the same window.
//
// A login is NOT requested when:
//
//   - the last maxUnansweredRequests requests produced no session the
//     portal accepts and the session file has not changed since
//     (GateBlocked);
//   - the session file was written less than two minutes ago (a login has
//     just happened; its session is the one that was refused);
//   - a login could still be running at 16:00-21:00 Pacific
//     (psportal.FloorBlocksLoginAt: 15:57:30 to 21:00) -- the login
//     workload would not act on it;
//   - this connector asked less than min_interval_minutes ago, or the
//     recorded time of its last request is in the future (a clock that
//     jumped must not open the gate). Both are kept in the checkpoint, so
//     they hold across restarts.
func (g *SessionGate) requestAndWait(ctx context.Context, cp connector.Checkpoint) GateResult {
	now := g.Now()
	dead := g.Fingerprint()

	if n, at := loadUnanswered(cp); n >= maxUnansweredRequests {
		if at == dead {
			return g.blocked(n)
		}
		// The session file has changed since the connector gave up: that
		// is somebody's intervention. Start counting again.
		slog.Info("powerschool: the session file changed since login requests stopped; asking is allowed again")
		clearUnanswered(cp)
	}

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

	last, asked := loadLastLoginRequest(cp)
	switch {
	case asked && last.After(now):
		return skipPoll("login_requested_recently",
			"the recorded time of the last login request is in the future; treating it as just made and not asking again",
			"recorded_seconds_ahead", int64(last.Sub(now)/time.Second))
	case asked && now.Sub(last) < interval:
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
	default:
		// Recorded before the request: if the answer is lost, the request
		// may still have been delivered.
		if err := cp.Save(lastLoginRequestKey, now.UTC().Format(time.RFC3339)); err != nil {
			return failPoll("login_request_failed",
				"could not record the login request, so could not limit how often one is made; not asking",
				"error", err.Error())
		}
		if err := g.requester.RequestLogin(ctx, psportal.FormatLoginRequest(now, dead)); err != nil {
			return failPoll("login_request_failed",
				"could not write the login request; check the connector's Role, the request ConfigMap and the path to the API server",
				"error", err.Error())
		}
		slog.Info("powerschool: asked for a login; waiting for the new session",
			"wait_seconds", int64(wait/time.Second))
	}

	return g.waitForNewSession(ctx, cp, dead, wait)
}

// waitForNewSession re-reads the session file every few seconds until it
// holds a session other than the dead one, then checks that one once. It
// contacts the portal only for that single check.
//
// The poll that follows a live answer runs whenever the session arrives
// within the wait, including after its window's nominal end: the window
// decided when to start, the wait bounds how late the one attempt may be.
func (g *SessionGate) waitForNewSession(ctx context.Context, cp connector.Checkpoint, dead string, wait time.Duration) GateResult {
	deadline := g.Now().Add(wait)
	for {
		if have := g.reload(); have && g.Fingerprint() != dead {
			verdict := g.check(ctx, true)
			if verdict.State == SessionLive {
				clearUnanswered(cp)
				slog.Info("powerschool: a new session is in place and accepted; polling")
				return g.live(verdict.Reason)
			}
			if verdict.State == SessionDead {
				g.reportDead(verdict.Reason)
			}
			if n := g.bumpUnanswered(cp); n >= maxUnansweredRequests {
				return g.blocked(n)
			}
			return failPoll("new_session_not_accepted",
				"a new session arrived but the portal does not accept it; not asking for another login in this poll",
				"session_state", verdict.State.String(), "reason", verdict.Reason)
		}
		if !g.Now().Before(deadline) {
			if n := g.bumpUnanswered(cp); n >= maxUnansweredRequests {
				return g.blocked(n)
			}
			return failPoll("no_new_session",
				"no new session reached this pod in time: the login workload declined (its blackout, its minimum interval, "+
					"a blocked or unrecognised sign-in), failed, did not run, or the Secret has not synced; see its log",
				"waited_seconds", int64(wait/time.Second))
		}
		if err := g.Sleep(ctx, sessionPollInterval); err != nil {
			return skipPoll("interrupted", "interrupted while waiting for a new session")
		}
	}
}

// blocked is the answer once requests have stopped.
func (g *SessionGate) blocked(unanswered int) GateResult {
	slog.Error("powerschool: login requests are not producing a session the portal accepts; no more will be made "+
		"until the session file changes or the backoff is reset (POWERSCHOOL_LOGIN_BACKOFF_RESET)",
		"unanswered_requests", unanswered)
	return GateResult{Outcome: GateBlocked, Reason: "login_backoff", Failed: true}
}

// bumpUnanswered records one more request that led nowhere, with the
// session file's fingerprint as it is now, and returns the new count.
func (g *SessionGate) bumpUnanswered(cp connector.Checkpoint) int {
	n, _ := loadUnanswered(cp)
	n++
	if err := cp.Save(unansweredKey, strconv.Itoa(n)+" "+fingerprintOrNone(g.Fingerprint())); err != nil {
		slog.Warn("powerschool: could not record an unanswered login request", "error", err)
	}
	return n
}

func fingerprintOrNone(fp string) string {
	if fp == "" {
		return psportal.NoSessionFingerprint
	}
	return fp
}

// loadUnanswered reads the unanswered-request count and the session
// fingerprint it was recorded against ("" for "no session").
func loadUnanswered(cp connector.Checkpoint) (count int, fingerprint string) {
	raw, ok := cp.Load(unansweredKey)
	if !ok {
		return 0, ""
	}
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return 0, ""
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 {
		return 0, ""
	}
	if fields[1] == psportal.NoSessionFingerprint {
		return n, ""
	}
	return n, fields[1]
}

func clearUnanswered(cp connector.Checkpoint) {
	if _, ok := cp.Load(unansweredKey); ok {
		if err := cp.Delete(unansweredKey); err != nil {
			slog.Warn("powerschool: could not clear the unanswered login request count", "error", err)
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
