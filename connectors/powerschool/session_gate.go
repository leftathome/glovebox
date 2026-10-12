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
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
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

// There are two ways to ask, chosen by configuration, and the gate is the
// same around both -- the pre-poll check, what counts as dead, the count of
// requests that led nowhere and the permanent failure it ends in:
//
//   - the request mailbox (LoginRequester): as above; the session arrives
//     later, in the session file;
//   - the login broker (SessionBroker): one authenticated call to a service
//     that holds the credentials and answers with the outcome and, when
//     there is one, the session. The limits on logins -- blackout, minimum
//     interval, latch -- are enforced there, where the connector cannot
//     reach them, and are not repeated here.

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
	// brokerHoldKey (login broker only): "<fingerprint|none> <RFC 3339|-> <code>"
	// -- the session this connector last found dead, and, when the broker
	// said when a login may next be made, that time and why. While the
	// session in place is still that one, the portal is not asked about it
	// again, and before that time the broker is not asked either.
	brokerHoldKey = "meta:login_broker_hold"
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

// SessionBroker asks a login broker for a session. *brokerapi.Client is the
// production one. reason is a brokerapi.Reason constant; fullFingerprint is
// the fingerprint of the session found dead ("" when there is none).
type SessionBroker interface {
	Refresh(ctx context.Context, reason, fullFingerprint string) (*brokerapi.Response, error)
}

// SessionField is the field of a broker answer's session that holds the
// session document: the same JSON the session file holds. The broker's
// PowerSchool driver (internal/pslogin) writes it under this name.
const SessionField = "session"

// unknownStreakFails is how many polls in a row may be skipped because the
// session's state could not be established before the skip is counted as a
// failed poll. One unanswered check is weather; three in a row is a portal,
// a gateway or a network path that needs somebody's attention, and without
// this the connector would skip every poll forever, quietly. Applied with
// the login broker only; see Ensure.
const unknownStreakFails = 3

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
	// broker, when set, is asked instead of requester.
	requester LoginRequester
	broker    SessionBroker
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
	// held is set while the session in use came from the broker's answer
	// rather than the session file; replaced is the fingerprint of the
	// file's session at that moment ("" when the file had none). Until the
	// file catches up it still holds that replaced session, which must not
	// displace the newer one in use.
	held     bool
	replaced string
	// unknownStreak counts consecutive polls skipped as "state unknown".
	unknownStreak int
	// askedThisPoll is set once the broker has been asked in the poll that
	// began with the last Ensure. One poll asks the broker at most once.
	askedThisPoll bool
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

// NewBrokerSessionGate builds a gate that asks a login broker when it finds
// the session dead. broker may be nil; on-demand login is then off.
func NewBrokerSessionGate(source SessionSource, cfg LoginOnDemandConfig, broker SessionBroker) *SessionGate {
	g := NewSessionGate(source, LoginOnDemandConfig{}, nil)
	if cfg.Enabled && broker != nil {
		g.broker = broker
		g.onDemand = cfg
	}
	return g
}

// OnDemand reports whether the gate asks for logins. Safe on a nil gate.
func (g *SessionGate) OnDemand() bool { return g != nil && (g.requester != nil || g.broker != nil) }

// Synchronous reports whether asking for a login answers with the session
// (the broker) rather than leaving it to arrive later (the mailbox). Safe
// on a nil gate.
func (g *SessionGate) Synchronous() bool { return g != nil && g.broker != nil }

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
		// The file has caught up with the broker's answer.
		g.held, g.replaced = false, ""
		return true
	}
	if g.held && fp == g.replaced {
		// The file still holds the session the broker has since replaced.
		// The one in use is newer; keep it.
		return true
	}
	client, probe, reset, err := g.source.Build(session)
	if err != nil {
		slog.Error("powerschool: could not build a client for the session in the session file", "error", err)
		return g.fingerprint != ""
	}
	g.fingerprint, g.client, g.probe, g.reset, g.expired = fp, client, probe, reset, expired
	g.held, g.replaced = false, ""
	g.firstSeen = now
	if g.source.FirstSeen != nil {
		g.firstSeen = g.source.FirstSeen(fp, now)
	}
	return true
}

// adopt puts a session delivered by the broker into use, ahead of the
// session file (which receives the same session through Vault and ESO a
// little later).
func (g *SessionGate) adopt(session *pslib.SessionExport) error {
	now := g.Now()
	fp := SessionFingerprint(session)
	client, probe, reset, err := g.source.Build(session)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		g.replaced = g.fingerprint
	}
	g.held = true
	g.fingerprint, g.client, g.probe, g.reset = fp, client, probe, reset
	g.expired = !session.ExpiresAt.After(now)
	g.firstSeen = now
	if g.source.FirstSeen != nil {
		g.firstSeen = g.source.FirstSeen(fp, now)
	}
	return nil
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
	g.askedThisPoll = false
	have := g.reload()

	if g.Synchronous() {
		// The session in place is one this connector has already found
		// dead (in an earlier poll, or before a restart) and nothing has
		// replaced it. Asking the portal about it again would tell nothing
		// new and would be one more request with a dead session at every
		// poll, for as long as the broker cannot or may not log in -- its
		// latch, its blackout, its budget. So the portal is NOT asked; the
		// broker is, at most once in this poll, and not at all before a
		// time it has named.
		if hold, ok := loadBrokerHold(cp); ok && hold.fingerprint == fingerprintOrNone(g.Fingerprint()) {
			return g.requestAndWait(ctx, cp, brokerapi.ReasonSessionDead)
		}
	}
	verdict := g.check(ctx, have)

	switch verdict.State {
	case SessionLive:
		g.unknownStreak = 0
		clearUnanswered(cp)
		clearBrokerHold(cp)
		return g.live(verdict.Reason)
	case SessionUnknown:
		// Nothing is changed and no login is asked for: a login would end
		// the session if it is in fact alive.
		//
		// With the login broker, "could not tell" is not allowed to go on
		// for ever unnoticed: from the third poll in a row it is reported
		// as a failed poll (an ERROR line and the skip metric; the process
		// stays up). The mailbox path and the path without on-demand login
		// keep what they do on main -- an unknown state is never a failure
		// there (glovebox-wekc.14, review round 2) -- and are not changed
		// here.
		g.unknownStreak++
		failed := g.Synchronous() && g.unknownStreak >= unknownStreakFails
		slog.Warn("powerschool: could not tell whether the session is still accepted; skipping this poll, changing nothing",
			"reason", verdict.Reason, "consecutive", g.unknownStreak, "counts_as_failed_poll", failed)
		return GateResult{Outcome: GateSkip, Reason: "session_state_unknown:" + verdict.Reason, Failed: failed}
	}
	g.unknownStreak = 0

	g.reportDead(verdict.Reason)
	if !g.OnDemand() {
		return GateResult{Outcome: GateDead, Reason: verdict.Reason}
	}
	if g.Synchronous() {
		// Remembered across polls and restarts: this session is dead.
		saveBrokerHold(cp, brokerHold{fingerprint: fingerprintOrNone(g.Fingerprint())})
	}
	return g.requestAndWait(ctx, cp, brokerReason(verdict.Reason))
}

// Renew is for a session that stopped being accepted in the MIDDLE of a
// poll: with a login broker the gate asks for a new one at once, so the
// poll can be tried again, once, on it. Without a broker (the mailbox, or
// no on-demand login) it answers GateSkip and changes nothing; the next
// poll's Ensure deals with the dead session.
func (g *SessionGate) Renew(ctx context.Context, cp connector.Checkpoint) GateResult {
	if !g.Synchronous() {
		return GateResult{Outcome: GateSkip, Reason: "no_login_broker"}
	}
	g.reportDead(ReasonSignInPage)
	saveBrokerHold(cp, brokerHold{fingerprint: fingerprintOrNone(g.Fingerprint())})
	if g.askedThisPoll {
		// The session this poll was given by the broker minutes ago is
		// dead already. The broker is asked once per poll, not twice; the
		// next poll takes it up, without probing this session again.
		return GateResult{Outcome: GateSkip, Reason: "login_broker_already_asked"}
	}
	return g.requestAndWait(ctx, cp, brokerapi.ReasonAuthFailure)
}

// brokerReason maps why the gate holds a session to be dead onto the
// broker's closed list of reasons.
func brokerReason(verdictReason string) string {
	switch verdictReason {
	case ReasonNoSession:
		return brokerapi.ReasonNoSession
	case ReasonExpiredDocument:
		return brokerapi.ReasonSessionExpired
	}
	return brokerapi.ReasonSessionDead
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
	clearBrokerHold(cp)
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
func (g *SessionGate) requestAndWait(ctx context.Context, cp connector.Checkpoint, reason string) GateResult {
	now := g.Now()
	dead := g.Fingerprint()

	if g.broker != nil {
		// After maxUnansweredRequests sessions in a row that the broker
		// delivered and the portal then refused here: stop asking. The
		// count is in the checkpoint and is NOT tied to which session
		// happens to be in place: with a broker the session in place
		// changes every time one is delivered, and after a restart it is
		// whatever the file holds, so "the fingerprint changed" proves
		// nothing. Only a session the portal accepts, or the operator's
		// reset, clears it.
		if n, _ := loadUnanswered(cp); n >= maxUnansweredRequests {
			return g.blocked(n)
		}
		// The broker has said when a login may next be made. Until then
		// there is nothing to ask.
		if hold, ok := loadBrokerHold(cp); ok && hold.fingerprint == fingerprintOrNone(dead) && now.Before(hold.until) {
			return skipPoll("login_deferred:"+hold.code,
				"the session is dead and the login broker has said when a login may next be made; not asking before then",
				"retry_after", hold.until.UTC().Format(time.RFC3339))
		}
		// The broker decides whether a login may be made -- its blackout,
		// its minimum interval, its budget, its latch -- and says so in
		// its answer. The limits below belong to the mailbox, where the
		// connector cannot see what became of a request.
		return g.askBroker(ctx, cp, dead, reason)
	}

	// The mailbox: after maxUnansweredRequests requests in a row that led
	// to no session the portal accepts, stop.
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

// askBroker is the on-demand path through the login broker: one call, and
// the answer says how it went.
//
//	refreshed, current  a session came back. It is put into use and checked
//	                    once; live -> poll with it
//	deferred            the broker will not log in now (blackout, minimum
//	                    interval). Nobody's failure; this poll is skipped
//	latched             logins are stopped until a person clears the latch.
//	                    The process stays up and says so at every poll
//	budget_exhausted    as deferred: nothing more today
//	inconclusive,       no session; reported as a failed poll. The process
//	failed, no answer   stays up (see pollNow)
//
// Only a session that was delivered and then NOT accepted counts as an
// unanswered request here. A login the broker could not make is the
// broker's to count: it has its own latch, in Vault, and two counters for
// one fact would need two resets.
//
// Whatever the answer, the session found dead stays recorded as dead
// (brokerHoldKey) until one the portal accepts is in place: the next poll
// does not ask the portal about it again.
func (g *SessionGate) askBroker(ctx context.Context, cp connector.Checkpoint, dead, reason string) GateResult {
	slog.Info("powerschool: asking the login broker for a session", "reason", reason)
	g.askedThisPoll = true
	resp, err := g.broker.Refresh(ctx, reason, dead)
	if err != nil {
		return failPoll("login_broker_unreachable",
			"the login broker did not answer; check that it is running, its NetworkPolicy, and that this connector's ServiceAccount is on its allow-list",
			"error", brokerErrorText(err))
	}
	switch resp.Outcome {
	case brokerapi.OutcomeRefreshed, brokerapi.OutcomeCurrent:
		// Handled below.
	case brokerapi.OutcomeDeferred, brokerapi.OutcomeBudgetExhausted:
		code := resp.Code
		if resp.Outcome == brokerapi.OutcomeBudgetExhausted {
			code = brokerapi.OutcomeBudgetExhausted
		}
		hold := brokerHold{fingerprint: fingerprintOrNone(dead), code: code}
		attrs := []any{"code", code}
		if resp.RetryAfter != nil {
			// Believed only so far: a time a day or more away is asked
			// about again tomorrow.
			hold.until = *resp.RetryAfter
			if limit := g.Now().Add(maxBrokerHold); hold.until.After(limit) {
				hold.until = limit
			}
			attrs = append(attrs, "retry_after", resp.RetryAfter.UTC().Format(time.RFC3339))
		}
		saveBrokerHold(cp, hold)
		return skipPoll("login_deferred:"+code,
			"the session is dead and the login broker will not log in now", attrs...)
	case brokerapi.OutcomeLatched:
		// Not an exit: an exit is a restart, and a restart is another
		// poll. The connector stays up and says so at every poll window,
		// with one question to the broker and none to the portal.
		slog.Error("powerschool: the login broker's latch is set; it will not log in until a person clears it "+
			"(fix the cause, then clear the latch in Vault as the login broker's design note describes)",
			"code", resp.Code)
		return GateResult{Outcome: GateBlocked, Reason: "login_latched:" + resp.Code, Failed: true}
	case brokerapi.OutcomeInconclusive:
		return failPoll("login_broker_inconclusive",
			"the login broker could not tell whether its stored session is alive, and does not log in on a guess",
			"code", resp.Code, "detail", resp.Detail)
	default:
		return failPoll("login_broker_failed",
			"the login broker could not produce a session; see its log",
			"outcome", resp.Outcome, "code", resp.Code, "detail", resp.Detail)
	}

	if !resp.HasSession() {
		return failPoll("login_broker_bad_session", "the login broker's answer carried no session")
	}
	// Errors of parseSessionDocument never quote the document.
	session, err := parseSessionDocument([]byte(resp.Session[SessionField]))
	if err != nil {
		return failPoll("login_broker_bad_session", "the login broker's answer carried no usable session", "problem", err.Error())
	}
	if dead != "" && SessionFingerprint(session) == dead {
		return g.secondOpinion(ctx, cp)
	}
	if err := g.adopt(session); err != nil {
		return failPoll("login_broker_bad_session", "could not build a client for the session the login broker returned", "error", err.Error())
	}

	verdict := g.check(ctx, true)
	switch verdict.State {
	case SessionLive:
		clearUnanswered(cp)
		clearBrokerHold(cp)
		slog.Info("powerschool: the login broker's session is in place and accepted; polling",
			"outcome", resp.Outcome)
		return g.live(verdict.Reason)
	case SessionDead:
		g.reportDead(verdict.Reason)
		// This one is known dead now, too: it is not probed again.
		saveBrokerHold(cp, brokerHold{fingerprint: fingerprintOrNone(g.Fingerprint())})
		if n := g.bumpUnanswered(cp); n >= maxUnansweredRequests {
			return g.blocked(n)
		}
	}
	return failPoll("new_session_not_accepted",
		"the login broker returned a session but the portal does not accept it here; not asking again in this poll",
		"session_state", verdict.State.String(), "reason", verdict.Reason)
}

// maxBrokerHold bounds how long a "not before" from the broker is believed
// without asking again.
const maxBrokerHold = 24 * time.Hour

// brokerHold is what brokerHoldKey stores.
type brokerHold struct {
	// fingerprint of the session found dead ("none": there was none).
	fingerprint string
	// until: the broker need not be asked before this time. Zero: ask at
	// the next poll.
	until time.Time
	// code says why (blackout, min_interval, budget_exhausted, ...).
	code string
}

func loadBrokerHold(cp connector.Checkpoint) (brokerHold, bool) {
	raw, ok := cp.Load(brokerHoldKey)
	if !ok {
		return brokerHold{}, false
	}
	fields := strings.Fields(raw)
	if len(fields) != 3 || fields[0] == "" {
		return brokerHold{}, false
	}
	h := brokerHold{fingerprint: fields[0]}
	if fields[1] != "-" {
		if t, err := time.Parse(time.RFC3339, fields[1]); err == nil {
			h.until = t
		}
	}
	if fields[2] != "-" {
		h.code = fields[2]
	}
	return h, true
}

func saveBrokerHold(cp connector.Checkpoint, h brokerHold) {
	until, code := "-", "-"
	if !h.until.IsZero() {
		until = h.until.UTC().Format(time.RFC3339)
	}
	if h.code != "" && !strings.ContainsAny(h.code, " \t\n") {
		code = h.code
	}
	if err := cp.Save(brokerHoldKey, h.fingerprint+" "+until+" "+code); err != nil {
		slog.Warn("powerschool: could not record that the session is dead; the next poll will ask the portal again", "error", err)
	}
}

func clearBrokerHold(cp connector.Checkpoint) {
	if _, ok := cp.Load(brokerHoldKey); ok {
		if err := cp.Delete(brokerHoldKey); err != nil {
			slog.Warn("powerschool: could not clear the dead-session record", "error", err)
		}
	}
}

// secondOpinion is what happens when the broker answers with the very
// session this connector holds to be dead. The broker has just asked the
// portal and was told it is alive; this connector was told otherwise --
// by a gateway page that happened to be served once, by one auth error in
// the middle of a poll -- possibly polls ago, since a session recorded as
// dead is not probed again. The disagreement is settled by the only party
// that knows: the portal is asked once more, with ONE GET, and the record
// that suppressed that question is dropped first.
//
//	alive    the earlier verdict was wrong or has passed. Poll; clear the
//	         strikes.
//	dead     twice now, against the broker's word: a strike, and the
//	         session is recorded as dead again.
//	unknown  neither. No strike, and no record: the next poll starts with
//	         an ordinary check.
//
// This is the only way a poll asks the portal about a session a second
// time, and it does so at most once: a poll calls the broker at most once.
func (g *SessionGate) secondOpinion(ctx context.Context, cp connector.Checkpoint) GateResult {
	clearBrokerHold(cp)
	g.resetOverlay()
	verdict := g.check(ctx, true)
	switch verdict.State {
	case SessionLive:
		g.unknownStreak = 0
		clearUnanswered(cp)
		slog.Info("powerschool: the login broker says the session is alive, and asked again the portal accepts it; polling")
		return g.live(verdict.Reason)
	case SessionDead:
		g.reportDead(verdict.Reason)
		saveBrokerHold(cp, brokerHold{fingerprint: fingerprintOrNone(g.Fingerprint())})
		if n := g.bumpUnanswered(cp); n >= maxUnansweredRequests {
			return g.blocked(n)
		}
		return failPoll("broker_session_is_the_dead_one",
			"the login broker returned the session the portal refuses here, and asked again the portal still refuses it",
			"reason", verdict.Reason)
	}
	return failPoll("broker_session_unverified",
		"the login broker says the session is alive and the portal could not be asked again; nothing is concluded",
		"reason", verdict.Reason)
}

// brokerErrorText renders a broker client error for a log line. The client
// builds its errors from classified parts only (a status and a code, or a
// transport cause); this bounds the text all the same.
func brokerErrorText(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
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
