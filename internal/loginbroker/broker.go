package loginbroker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/glovebox/internal/dailywindow"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
	"github.com/leftathome/glovebox/internal/loginvault"
)

// Defaults and bounds of the policy.
const (
	// DefaultMinInterval is the least time between two logins.
	DefaultMinInterval = 20 * time.Minute
	// FloorMinInterval is the least a broker can be configured with, for
	// any site. A driver may demand more (Constraints.MinInterval).
	FloorMinInterval = 5 * time.Minute
	// DefaultMaxFailures is how many logins in a row may fail to produce a
	// session the site accepts before the latch is set.
	DefaultMaxFailures = 2
	// CeilingMaxFailures bounds that setting.
	CeilingMaxFailures = 5
	// DefaultLoginHeadroom is used when the driver names none.
	DefaultLoginHeadroom = 150 * time.Second
	// DefaultCooldown is how long no login is attempted after one that
	// failed before the credentials were submitted. Such a failure costs
	// the account nothing, but a caller must not be able to make the broker
	// start a browser in a loop.
	DefaultCooldown = 2 * time.Minute
	// DefaultOpTimeout bounds one refresh: the store, the probe, at most
	// two login attempts, the confirmation and the writes.
	DefaultOpTimeout = 6 * time.Minute
	// CeilingDailyLogins is the most logins that may be made for one site
	// in one local calendar day, by every workload sharing the account's
	// record together. It is a constant: configuration can lower it and
	// cannot raise it. Six is three poll windows a day with one retry
	// each. It is the bound that still holds when everything else has been
	// talked round: a caller that restarts, a session that keeps dying, an
	// owner and a connector signing each other out all day.
	CeilingDailyLogins = 6

	// recentLogin: a stored session this young, which is not the one the
	// caller reports dead, is the answer to a request that crossed with a
	// login. It is returned without asking the site about it.
	recentLogin = 2 * time.Minute
	// retryDelay separates a login that failed before the submit from the
	// one further attempt.
	retryDelay = 10 * time.Second
	// maxLoginAttempts: one, plus one more only after LoginNotSubmitted.
	maxLoginAttempts = 2
	// confirmAttempts bounds the read-only checks of a new session.
	confirmAttempts = 3
	confirmDelay    = 5 * time.Second
	// writeAttempts bounds a store write. Retrying a write is safe -- it is
	// not a login -- and worth it: a session that is not stored was bought
	// with the previous one.
	writeAttempts = 3
	writeDelay    = 5 * time.Second
)

// Reasons the latch is set (loginvault.State.BlockedReason). The first is
// also what the scheduled PowerSchool login job writes.
const (
	blockedCredentialsRejected = "credentials_rejected"
	blockedConsecutiveFailures = "consecutive_failures"
)

// Store is where the session and the broker's own record live. The
// production one is *loginvault.Store.
type Store interface {
	// ReadFields returns the stored session's fields (nil when none), when
	// it was created (zero when unknown) and the version to check-and-set
	// against (0 when the path does not exist).
	ReadFields(ctx context.Context) (fields map[string]any, loggedInAt time.Time, version int, err error)
	// CanWrite reports whether the session can be written at all.
	CanWrite(ctx context.Context) (bool, error)
	// Write stores the session's fields only if the path is still at
	// version cas, and returns the new version.
	Write(ctx context.Context, fields map[string]any, cas int) (version int, err error)
	ReadState(ctx context.Context) (*loginvault.State, error)
	WriteState(ctx context.Context, state *loginvault.State) error
	// Close gives up whatever the store authenticated with.
	Close()
}

// Config is everything a Broker needs.
type Config struct {
	Driver      Driver
	Credentials Credentials
	// Connect opens the store. It is called once per refresh and the store
	// is closed at the end of it: the broker holds no long-lived store
	// token between requests.
	Connect func(ctx context.Context) (Store, error)

	// Blackout is the configured windows. The driver's own are added.
	Blackout dailywindow.Schedule
	// MinInterval between logins; raised to the driver's floor. Zero means
	// DefaultMinInterval.
	MinInterval time.Duration
	// MaxFailures: consecutive failed logins before the latch. Zero means
	// DefaultMaxFailures.
	MaxFailures int
	// DailyBudget: logins per local calendar day. Zero means
	// CeilingDailyLogins, which is also the most it can be.
	DailyBudget int
	// Cooldown and OpTimeout; zero means the defaults.
	Cooldown  time.Duration
	OpTimeout time.Duration
	// StatePath names where the latch lives, for the log line that tells an
	// operator what to clear. Not a secret.
	StatePath string

	Logger  *slog.Logger
	Metrics *Metrics

	// Seams for tests.
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	NewSalt func() (string, error)
}

// Broker applies the policy. It is safe for concurrent use; concurrent
// requests share one refresh.
type Broker struct {
	driver      Driver
	creds       Credentials
	connect     func(ctx context.Context) (Store, error)
	blackout    dailywindow.Schedule
	headroom    time.Duration
	minInterval time.Duration
	maxFailures int
	dailyBudget int
	budgetLoc   *time.Location
	cooldown    time.Duration
	opTimeout   time.Duration
	statePath   string
	log         *slog.Logger
	metrics     *Metrics
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	newSalt     func() (string, error)

	mu       sync.Mutex
	inflight *call
	wg       sync.WaitGroup

	// Touched only by the one refresh in flight.
	unsaved *heldSession
}

// call is one refresh in flight and its answer.
type call struct {
	done chan struct{}
	resp *brokerapi.Response
}

// heldSession is a confirmed session that could not be written to the
// store. It is offered again by the next refresh.
type heldSession struct {
	session    Session
	loggedInAt time.Time
}

// New validates cfg and builds a Broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Driver == nil {
		return nil, errors.New("loginbroker: no driver")
	}
	if !validSite(cfg.Driver.Site()) {
		return nil, errors.New("loginbroker: the driver's site name must be lower-case letters, digits and dashes")
	}
	if cfg.Connect == nil {
		return nil, errors.New("loginbroker: no store")
	}
	if cfg.Credentials.Username == "" || cfg.Credentials.Password == "" {
		return nil, errors.New("loginbroker: no credentials")
	}
	b := &Broker{
		driver:      cfg.Driver,
		creds:       cfg.Credentials,
		connect:     cfg.Connect,
		blackout:    append(dailywindow.Schedule{}, cfg.Blackout...),
		headroom:    DefaultLoginHeadroom,
		minInterval: cfg.MinInterval,
		maxFailures: cfg.MaxFailures,
		dailyBudget: cfg.DailyBudget,
		cooldown:    cfg.Cooldown,
		opTimeout:   cfg.OpTimeout,
		statePath:   cfg.StatePath,
		log:         cfg.Logger,
		metrics:     cfg.Metrics,
		now:         cfg.Now,
		sleep:       cfg.Sleep,
		newSalt:     cfg.NewSalt,
	}
	if b.minInterval == 0 {
		b.minInterval = DefaultMinInterval
	}
	floor := FloorMinInterval
	if c, ok := cfg.Driver.(Constrained); ok {
		limits := c.Constraints()
		b.blackout = append(b.blackout, limits.Blackout...)
		if limits.MinInterval > floor {
			floor = limits.MinInterval
		}
		if limits.LoginHeadroom > 0 {
			b.headroom = limits.LoginHeadroom
		}
		b.budgetLoc = limits.Location
	}
	if b.budgetLoc == nil {
		for _, set := range cfg.Blackout {
			if set.Loc != nil {
				b.budgetLoc = set.Loc
				break
			}
		}
	}
	if b.budgetLoc == nil {
		b.budgetLoc = time.UTC
	}
	if b.dailyBudget == 0 {
		b.dailyBudget = CeilingDailyLogins
	}
	if b.dailyBudget < 1 || b.dailyBudget > CeilingDailyLogins {
		return nil, fmt.Errorf("loginbroker: the daily login budget must be between 1 and %d; it can be lowered, not raised", CeilingDailyLogins)
	}
	if b.minInterval < floor {
		return nil, fmt.Errorf("loginbroker: the minimum interval between logins must be at least %s for site %s", floor, cfg.Driver.Site())
	}
	if b.maxFailures == 0 {
		b.maxFailures = DefaultMaxFailures
	}
	if b.maxFailures < 1 || b.maxFailures > CeilingMaxFailures {
		return nil, fmt.Errorf("loginbroker: the failure limit must be between 1 and %d", CeilingMaxFailures)
	}
	if b.cooldown <= 0 {
		b.cooldown = DefaultCooldown
	}
	if b.opTimeout <= 0 {
		b.opTimeout = DefaultOpTimeout
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	b.log = b.log.With("site", cfg.Driver.Site())
	if b.now == nil {
		b.now = time.Now
	}
	if b.sleep == nil {
		b.sleep = sleepCtx
	}
	if b.newSalt == nil {
		return nil, errors.New("loginbroker: no salt source")
	}
	return b, nil
}

// Site returns the driver's site name.
func (b *Broker) Site() string { return b.driver.Site() }

// Blackout returns every window that applies, configured and the driver's.
func (b *Broker) Blackout() dailywindow.Schedule { return b.blackout }

// MinInterval returns the interval in force.
func (b *Broker) MinInterval() time.Duration { return b.minInterval }

// DailyBudget returns the daily login budget in force and the zone whose
// calendar it is counted on.
func (b *Broker) DailyBudget() (int, *time.Location) { return b.dailyBudget, b.budgetLoc }

// LoginHeadroom returns how long before a blackout window no login starts.
func (b *Broker) LoginHeadroom() time.Duration { return b.headroom }

// Refresh answers one request. Concurrent requests share one refresh and
// get the same answer: there is never more than one login in flight, and a
// second caller never causes a second login.
//
// The work runs on its own deadline, not the caller's: a caller that gives
// up does not abort a login halfway, and the session a finished login
// produced is stored for whoever asks next. An error is returned only when
// ctx ends before the answer exists.
func (b *Broker) Refresh(ctx context.Context, req brokerapi.Request) (*brokerapi.Response, error) {
	b.mu.Lock()
	c := b.inflight
	if c == nil {
		c = &call{done: make(chan struct{})}
		b.inflight = c
		b.wg.Add(1)
		go b.run(c, req)
	} else {
		b.metrics.joined()
	}
	b.mu.Unlock()

	select {
	case <-c.done:
		return c.resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Wait blocks until no refresh is in flight. Call it after the HTTP server
// has stopped accepting requests.
func (b *Broker) Wait() { b.wg.Wait() }

func (b *Broker) run(c *call, req brokerapi.Request) {
	defer b.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), b.opTimeout)
	defer cancel()

	resp := func() (resp *brokerapi.Response) {
		defer func() {
			if r := recover(); r != nil {
				// The panic value is not logged: it may quote anything.
				b.log.Error("refresh panicked", "outcome", brokerapi.OutcomeFailed, "code", brokerapi.CodeInternal)
				resp = b.answer(brokerapi.OutcomeFailed, brokerapi.CodeInternal, "")
			}
		}()
		return b.refresh(ctx, req)
	}()
	b.metrics.request(resp.Outcome, resp.Code)

	c.resp = resp
	b.mu.Lock()
	b.inflight = nil
	b.mu.Unlock()
	close(c.done)
}

func (b *Broker) answer(outcome, code, detail string) *brokerapi.Response {
	return &brokerapi.Response{Site: b.driver.Site(), Outcome: outcome, Code: code, Detail: b.token(detail)}
}

// refresh is the policy, in order:
//
//  1. Open the store and read the broker's record. If it says a login is
//     in flight elsewhere, stop: its outcome is not guessed. An attempt
//     that never recorded its outcome and is no longer fresh (the process
//     died) is counted as a failed login. If the latch is set for these
//     credentials: stop.
//  2. Read the stored session. If the site still accepts it, return it. If
//     that cannot be told, stop: a login would end it if it is alive.
//  3. The session is dead or absent. Stop, with the time a login may be
//     made, when inside a blackout window, within the minimum interval of
//     the last login, cooling down after a login that never got as far as
//     the submit, or out of the day's budget.
//  4. Record that a login is starting -- check-and-set, so that a record
//     somebody else changed since step 1 means no login -- then read the
//     clock again, apply step 3 again, and log in: once, or twice when the
//     first attempt failed before the credentials were submitted.
//  5. Confirm the new session with the probe. A rejected login sets the
//     latch; so do MaxFailures logins in a row without a confirmed session.
//  6. Record the outcome, write the session to the store, return it.
func (b *Broker) refresh(ctx context.Context, req brokerapi.Request) *brokerapi.Response {
	log := b.log.With("reason", req.Reason)

	store, err := b.connect(ctx)
	if err != nil {
		log.Error("could not open the store; not logging in",
			"outcome", brokerapi.OutcomeFailed, "code", brokerapi.CodeStoreUnavailable, "error", b.redact(err))
		return b.answer(brokerapi.OutcomeFailed, brokerapi.CodeStoreUnavailable, "")
	}
	defer store.Close()

	// --- 1. the record -------------------------------------------------
	state, err := store.ReadState(ctx)
	if err != nil {
		return b.storeFailure(log, "read_state", err)
	}
	if state == nil {
		state = &loginvault.State{}
	}
	if state.Salt == "" {
		salt, serr := b.newSalt()
		if serr != nil || salt == "" {
			log.Error("no random salt available; not logging in", "outcome", brokerapi.OutcomeFailed, "code", brokerapi.CodeInternal)
			return b.answer(brokerapi.OutcomeFailed, brokerapi.CodeInternal, "")
		}
		state.Salt = salt
	}
	credFP := loginvault.CredentialFingerprint(state.Salt, b.creds.Username, b.creds.Password)

	now := b.now()
	if state.AttemptInProgress(now) {
		// Somebody else -- a second pod of this broker during a restart,
		// the scheduled login job -- is logging in right now. (Not this
		// process: its own refreshes are one at a time.) That login is not
		// a failure and not this request's to judge. Nothing is counted,
		// nothing is written and the site is not asked anything.
		return b.deferred(log, brokerapi.CodeLoginInProgress, now, state.AttemptStartedAt.Add(loginvault.AttemptFreshFor))
	}
	if !state.AttemptStartedAt.IsZero() {
		// A process died between "about to log in" and "this is how it
		// went", long enough ago that it is not coming back. The
		// credentials may have been submitted; nobody knows. It counts as
		// a failed login made at that time.
		log.Warn("an earlier login attempt never recorded its outcome (the process ended during it); counting it as a failed login",
			"attempt_started_at", state.AttemptStartedAt.UTC().Format(time.RFC3339))
		at := state.AttemptStartedAt
		state.RequestLoginAt = at
		state.AttemptStartedAt = time.Time{}
		state.CountLogin(loginvault.DayOf(at, b.budgetLoc))
		b.countFailure(state)
		if err := b.writeState(ctx, store, state); err != nil {
			return b.recordFailure(log, now, "write_state", err)
		}
	}

	if state.Blocked {
		if state.CredentialFingerprint == credFP {
			code := latchCode(state.BlockedReason)
			b.metrics.setLatched(true)
			log.Error("login latched: a person must look at the account and clear the latch before another login is attempted",
				"outcome", brokerapi.OutcomeLatched, "code", code,
				"blocked_at", state.BlockedAt.UTC().Format(time.RFC3339),
				"state_path", b.statePath)
			return b.answer(brokerapi.OutcomeLatched, code, "")
		}
		log.Warn("the login was latched, but the credentials have changed since; the latch no longer applies")
		state.Blocked, state.BlockedReason, state.BlockedAt, state.UnconfirmedCount = false, "", time.Time{}, 0
	}
	b.metrics.setLatched(false)
	b.metrics.loginsToday(state.LoginsOn(loginvault.DayOf(now, b.budgetLoc)))

	// --- 2. the stored session ----------------------------------------
	fields, loggedInAt, version, err := store.ReadFields(ctx)
	if err != nil {
		return b.storeFailure(log, "read_session", err)
	}
	var stored Session
	if sf := stringFields(fields); len(sf) > 0 {
		if s, derr := b.driver.Decode(sf); derr == nil && s != nil {
			stored = s
		} else {
			log.Info("the stored session is not usable; treating the store as empty")
		}
	}
	needsWrite := false
	if u := b.unsaved; u != nil && (stored == nil || u.loggedInAt.After(loggedInAt)) {
		// A login this process made whose session never reached the store.
		stored, loggedInAt, needsWrite = u.session, u.loggedInAt, true
	}

	if stored != nil {
		storedFP := brokerapi.ShortFingerprint(b.driver.Fingerprint(stored))
		age := now.Sub(loggedInAt)
		if storedFP != req.Fingerprint && !loggedInAt.IsZero() && age >= 0 && age < recentLogin {
			// The caller is asking about a session that has already been
			// replaced, moments ago. That login was confirmed then; there
			// is nothing to ask the site.
			log.Info("the caller's session has already been replaced; returning the current one",
				"outcome", brokerapi.OutcomeCurrent, "session_age_seconds", int64(age/time.Second))
			return b.withSession(ctx, log, store, brokerapi.OutcomeCurrent, stored, loggedInAt, version, needsWrite)
		}
		probe := b.driver.Probe(ctx, stored)
		b.metrics.probe(probe.State)
		switch probe.State {
		case ProbeAlive:
			log.Info("the stored session is still accepted; no login",
				"outcome", brokerapi.OutcomeCurrent, "caller_has_it", storedFP == req.Fingerprint)
			return b.withSession(ctx, log, store, brokerapi.OutcomeCurrent, stored, loggedInAt, version, needsWrite)
		case ProbeUnknown:
			log.Warn("could not tell whether the stored session is still accepted; not logging in, since a login would end it if it is",
				"outcome", brokerapi.OutcomeInconclusive, "probe_reason", b.token(probe.Reason))
			return b.answer(brokerapi.OutcomeInconclusive, brokerapi.CodeProbeInconclusive, probe.Reason)
		}
		log.Info("the stored session is no longer accepted", "probe_reason", b.token(probe.Reason))
	} else {
		log.Info("no session is stored")
	}

	// --- 3. may a login be made now? -----------------------------------
	// The clock is read again: the store and the probe took time, and the
	// probe may have taken its whole timeout.
	if resp := b.gate(log, b.now(), state, loggedInAt); resp != nil {
		return resp
	}

	// --- 4. the login ---------------------------------------------------
	if ok, err := store.CanWrite(ctx); err != nil {
		// Not being able to ask is not evidence of a missing grant.
		log.Warn("could not check write access to the session before logging in", "error", b.redact(err))
	} else if !ok {
		log.Error("the store would refuse the session; not logging in",
			"outcome", brokerapi.OutcomeFailed, "code", brokerapi.CodeStoreRejected)
		return b.answer(brokerapi.OutcomeFailed, brokerapi.CodeStoreRejected, "")
	}

	// Written BEFORE the login, check-and-set against the version read in
	// step 1. If the write is refused for any reason there is no login:
	// either the outcome could not be recorded afterwards (and a rejected
	// password would be submitted again), or somebody else has changed the
	// record since -- logged in, set the latch, started a login -- and
	// everything decided above was decided on a stale read.
	started := b.now().UTC()
	state.CredentialFingerprint = credFP
	state.AttemptStartedAt = started
	if err := b.writeState(ctx, store, state); err != nil {
		log.Error("could not record that a login is starting; not logging in")
		return b.recordFailure(log, started, "write_state", err)
	}

	session, lerr, held := b.login(ctx, log, state, loggedInAt)
	if held != nil {
		// The gate closed between the decision and the attempt (the
		// writes above took time, or the pause before a second attempt
		// did). Nothing was submitted by the attempt that was not made.
		if err := b.commit(ctx, store, state, func(s *loginvault.State) { s.AttemptStartedAt = time.Time{} }); err != nil {
			log.Error("could not clear the attempt marker; the next request will treat this attempt as a login in flight, then as a failed one",
				"state_path", b.statePath, "error", b.redact(err))
		}
		return held
	}
	if lerr != nil {
		return b.loginFailed(ctx, log, store, state, started, lerr)
	}

	// --- 5. confirm -----------------------------------------------------
	if session == nil {
		return b.failedLogin(ctx, log, store, state, started, brokerapi.CodeLoginUnconfirmed, "no_session")
	}
	if probe := b.confirm(ctx, session); probe.State != ProbeAlive {
		return b.failedLogin(ctx, log, store, state, started, brokerapi.CodeLoginUnconfirmed, "probe_"+probe.State.String()+":"+probe.Reason)
	}

	// --- 6. record and store -------------------------------------------
	newLoggedInAt := b.now().UTC()
	if err := b.commit(ctx, store, state, func(s *loginvault.State) {
		s.AttemptStartedAt = time.Time{}
		s.RequestLoginAt = started
		s.UnconfirmedCount = 0
		s.CooldownUntil = time.Time{}
		s.CountLogin(loginvault.DayOf(started, b.budgetLoc))
	}); err != nil {
		// The login worked. What is lost is the record of it: the next
		// request will find the attempt marker and, once it is stale,
		// count a failure that did not happen. That errs on the side of
		// fewer logins.
		log.Error("the login succeeded but its outcome could not be recorded; a later request will count it as a failure",
			"state_path", b.statePath, "error", b.redact(err))
	}
	b.metrics.loginResult("success")
	b.metrics.lastLogin(newLoggedInAt)
	b.metrics.loginsToday(state.LoginsOn(loginvault.DayOf(newLoggedInAt, b.budgetLoc)))
	log.Info("logged in; the site accepts the new session", "outcome", brokerapi.OutcomeRefreshed)
	return b.withSession(ctx, log, store, brokerapi.OutcomeRefreshed, session, newLoggedInAt, version, true)
}

// gate decides whether a login may START at now. It returns nil when it
// may, and otherwise the answer that says why not and until when. It reads
// no clock of its own and has no side effects, so it can be -- and is --
// applied more than once: when the session is found dead, and again
// immediately before every call of the driver's Login.
//
// loggedInAt is when the stored session was made (zero when unknown).
func (b *Broker) gate(log *slog.Logger, now time.Time, state *loginvault.State, loggedInAt time.Time) *brokerapi.Response {
	// No login starts unless the whole span from now to now + headroom is
	// clear of every window; the headroom covers one whole call of the
	// driver's Login, which is cancelled if it runs longer.
	if b.blackout.BlocksAt(now, b.headroom) {
		return b.deferred(log, brokerapi.CodeBlackout, now, now)
	}
	// The last login by anybody: the one a workload recorded, or the one
	// that made the stored session. A session that died minutes after it
	// was made was most likely ended by the account's owner signing in;
	// logging in again at once would sign them out.
	last := state.RequestLoginAt
	if loggedInAt.After(last) {
		last = loggedInAt
	}
	if !last.IsZero() {
		if last.After(now) {
			// A clock that jumped must not open the gate.
			return b.deferred(log, brokerapi.CodeMinInterval, now, now.Add(b.minInterval))
		}
		if now.Sub(last) < b.minInterval {
			return b.deferred(log, brokerapi.CodeMinInterval, now, last.Add(b.minInterval))
		}
	}
	if now.Before(state.CooldownUntil) {
		until := state.CooldownUntil
		// A cooldown far beyond what this broker ever sets is a broken
		// clock or a hand edit; it is honoured only as far as one cooldown.
		if limit := now.Add(b.cooldown); until.After(limit) {
			until = limit
		}
		return b.deferred(log, brokerapi.CodeCooldown, now, until)
	}
	day := loginvault.DayOf(now, b.budgetLoc)
	if used := state.LoginsOn(day); used >= b.dailyBudget {
		local := now.In(b.budgetLoc)
		midnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, b.budgetLoc).UTC()
		resp := b.answer(brokerapi.OutcomeBudgetExhausted, "", "")
		resp.RetryAfter = &midnight
		b.metrics.loginsToday(used)
		log.Error("the session is dead and the day's login budget is spent; no login until the next local midnight",
			"outcome", brokerapi.OutcomeBudgetExhausted, "logins_today", used, "daily_budget", b.dailyBudget,
			"local_day", day, "retry_after", midnight.Format(time.RFC3339))
		return resp
	}
	return nil
}

// commit applies mutate to state and stores it. If the record has changed
// since state was read (a check-and-set mismatch) the record is read again,
// mutate is applied to THAT, and it is stored: an outcome that has already
// happened must not be lost because somebody else wrote in between, and it
// must not overwrite what they wrote either. state is updated to what was
// stored.
//
// This is for recording what has happened. Deciding to log in never goes
// through here: that write is plain check-and-set, and a mismatch means no
// login.
func (b *Broker) commit(ctx context.Context, store Store, state *loginvault.State, mutate func(*loginvault.State)) error {
	mutate(state)
	err := b.writeState(ctx, store, state)
	if !loginvault.IsCASMismatch(err) {
		return err
	}
	fresh, rerr := store.ReadState(ctx)
	if rerr != nil || fresh == nil {
		return err
	}
	mutate(fresh)
	if werr := b.writeState(ctx, store, fresh); werr != nil {
		return werr
	}
	*state = *fresh
	return nil
}

// recordFailure answers a write of the record that failed BEFORE a login.
// A check-and-set mismatch is not a broken store: somebody else changed the
// record, and the answer is "not now", with nothing decided.
func (b *Broker) recordFailure(log *slog.Logger, now time.Time, op string, err error) *brokerapi.Response {
	if loginvault.IsCASMismatch(err) {
		log.Warn("the login record changed while this request was deciding; nothing was decided on the stale read")
		return b.deferred(log, brokerapi.CodeStateConflict, now, now.Add(time.Minute))
	}
	return b.storeFailure(log, op, err)
}

// login calls the driver: once, or once more when the first call failed
// before the credentials were submitted. It is the only place the
// credentials leave the Broker.
//
// Immediately before EVERY call the clock is read and the gate applied
// again -- blackout and interval included -- because everything between
// the decision and here took time: the store, the pause before a second
// attempt, a first attempt that used most of its allowance. If the gate
// has closed, no call is made and held is the answer to give. Each call is
// cancelled after the login headroom, which is what makes "the next
// headroom is clear of every window" mean "this login cannot submit inside
// one".
func (b *Broker) login(ctx context.Context, log *slog.Logger, state *loginvault.State, loggedInAt time.Time) (session Session, lerr *LoginError, held *brokerapi.Response) {
	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		if attempt > 1 {
			if err := b.sleep(ctx, retryDelay); err != nil {
				return nil, lerr, nil
			}
		}
		if held := b.gate(log, b.now(), state, loggedInAt); held != nil {
			if lerr != nil {
				// A first attempt was made and failed before the submit;
				// that, not the closed gate, is how this request ends.
				return nil, lerr, nil
			}
			return nil, nil, held
		}
		log.Info("logging in", "attempt", attempt)
		b.metrics.loginAttempt()
		actx, cancel := context.WithTimeout(ctx, b.headroom)
		s, err := b.driver.Login(actx, b.creds)
		cancel()
		if err == nil {
			return s, nil, nil
		}
		lerr = asLoginError(err)
		if lerr.Kind != LoginNotSubmitted {
			return nil, lerr, nil
		}
		log.Warn("the login failed before the credentials were submitted",
			"attempt", attempt, "detail", b.token(lerr.Detail))
	}
	return nil, lerr, nil
}

// asLoginError makes any error a *LoginError. An error of another type is
// LoginUnknown: the kind that is never retried.
func asLoginError(err error) *LoginError {
	var le *LoginError
	if errors.As(err, &le) {
		return le
	}
	return &LoginError{Kind: LoginUnknown, Detail: "unclassified"}
}

// loginFailed turns the driver's failure into the record and the answer.
func (b *Broker) loginFailed(ctx context.Context, log *slog.Logger, store Store, state *loginvault.State,
	started time.Time, lerr *LoginError) *brokerapi.Response {
	switch lerr.Kind {
	case LoginNotSubmitted:
		// Nothing was submitted: not a login for the interval, not a
		// strike for the latch. Only the cooldown.
		b.metrics.loginResult("not_submitted")
		until := b.now().Add(b.cooldown).UTC()
		if err := b.commit(ctx, store, state, func(s *loginvault.State) {
			s.AttemptStartedAt = time.Time{}
			// In the record, not in the process: a restart does not
			// shorten it.
			s.CooldownUntil = until
		}); err != nil {
			log.Error("could not clear the attempt marker; a later request will count this attempt as a failed login",
				"state_path", b.statePath, "error", b.redact(err))
		}
		log.Error("the login could not be attempted: it failed before the credentials were submitted",
			"outcome", brokerapi.OutcomeFailed, "code", brokerapi.CodeLoginNotSubmitted, "detail", b.token(lerr.Detail))
		return b.answer(brokerapi.OutcomeFailed, brokerapi.CodeLoginNotSubmitted, lerr.Detail)
	case LoginRejected:
		b.metrics.loginResult("rejected")
		b.recordLatch(ctx, log, store, state, func(s *loginvault.State) {
			s.AttemptStartedAt = time.Time{}
			s.RequestLoginAt = started
			s.CountLogin(loginvault.DayOf(started, b.budgetLoc))
			b.setLatch(s, blockedCredentialsRejected)
		})
		log.Error("the site rejected the credentials; no further login will be attempted until they change or the latch is cleared",
			"outcome", brokerapi.OutcomeLatched, "code", brokerapi.CodeCredentialsRejected, "state_path", b.statePath)
		return b.answer(brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected, "")
	}
	return b.failedLogin(ctx, log, store, state, started, brokerapi.CodeLoginFailed, lerr.Detail)
}

// failedLogin records a login that may have submitted the credentials and
// did not end with a session the site accepts. The session, if there was
// one, is never stored. After MaxFailures in a row the latch is set: a
// sign-in page that now wants a CAPTCHA, a second factor or a new password
// looks like this, and each further login would end the connector's session
// again without ever replacing it.
func (b *Broker) failedLogin(ctx context.Context, log *slog.Logger, store Store, state *loginvault.State,
	started time.Time, code, detail string) *brokerapi.Response {
	b.metrics.loginResult(code)
	err := b.commit(ctx, store, state, func(s *loginvault.State) {
		s.AttemptStartedAt = time.Time{}
		s.RequestLoginAt = started
		s.CountLogin(loginvault.DayOf(started, b.budgetLoc))
		b.countFailure(s)
	})
	if state.Blocked {
		if err != nil {
			b.latchNotRecorded(log, state, err)
		}
		log.Error("logins keep failing; no further login will be attempted until the latch is cleared",
			"outcome", brokerapi.OutcomeLatched, "code", brokerapi.CodeConsecutiveFailures,
			"last_failure", code, "detail", b.token(detail),
			"consecutive_failures", state.UnconfirmedCount, "state_path", b.statePath)
		return b.answer(brokerapi.OutcomeLatched, brokerapi.CodeConsecutiveFailures, detail)
	}
	if err != nil {
		log.Error("could not record the failed login; a later request will count it from the attempt marker",
			"state_path", b.statePath, "error", b.redact(err))
	}
	log.Error("the login did not produce a session the site accepts",
		"outcome", brokerapi.OutcomeFailed, "code", code, "detail", b.token(detail),
		"consecutive_failures", state.UnconfirmedCount, "failure_limit", b.maxFailures)
	return b.answer(brokerapi.OutcomeFailed, code, detail)
}

// countFailure adds one failed login and sets the latch at the limit.
func (b *Broker) countFailure(state *loginvault.State) {
	state.UnconfirmedCount++
	if state.UnconfirmedCount >= b.maxFailures && !state.Blocked {
		b.setLatch(state, blockedConsecutiveFailures)
	}
}

func (b *Broker) setLatch(state *loginvault.State, reason string) {
	state.Blocked = true
	state.BlockedReason = reason
	state.BlockedAt = b.now().UTC()
	b.metrics.setLatched(true)
}

// recordLatch stores an outcome that sets the latch.
func (b *Broker) recordLatch(ctx context.Context, log *slog.Logger, store Store, state *loginvault.State, mutate func(*loginvault.State)) {
	if err := b.commit(ctx, store, state, mutate); err != nil {
		b.latchNotRecorded(log, state, err)
	}
}

// latchNotRecorded says, loudly, that a latch exists only in this process.
// The attempt marker written before the login is still in the store, so a
// later request counts this login as failed even without the record. A
// REJECTED password, though, is then only a strike and not yet a latch.
func (b *Broker) latchNotRecorded(log *slog.Logger, state *loginvault.State, err error) {
	log.Error("COULD NOT RECORD THE LOGIN LATCH. Scale this broker to zero until the store is writable again",
		"blocked_reason", state.BlockedReason, "state_path", b.statePath, "error", b.redact(err))
}

// latchCode maps a stored latch reason to an answer code. Anything but a
// rejection -- including the scheduled job's "login_unconfirmed" -- is the
// count of failures.
func latchCode(reason string) string {
	if reason == blockedCredentialsRejected {
		return brokerapi.CodeCredentialsRejected
	}
	return brokerapi.CodeConsecutiveFailures
}

// confirm asks the site about a session that has just been made. Reads
// only. An unanswered question is asked again; a refusal is final.
func (b *Broker) confirm(ctx context.Context, session Session) ProbeResult {
	var probe ProbeResult
	for attempt := 1; attempt <= confirmAttempts; attempt++ {
		probe = b.driver.Probe(ctx, session)
		b.metrics.probe(probe.State)
		if probe.State != ProbeUnknown {
			return probe
		}
		if attempt < confirmAttempts {
			if err := b.sleep(ctx, confirmDelay); err != nil {
				break
			}
		}
	}
	return probe
}

// deferred answers "not now", with the earliest time a login may be made.
func (b *Broker) deferred(log *slog.Logger, code string, now, earliest time.Time) *brokerapi.Response {
	resp := b.answer(brokerapi.OutcomeDeferred, code, "")
	// Whatever opens the gate, a blackout may still close it.
	if clear, ok := b.blackout.NextClear(earliest, b.headroom); ok {
		t := clear.UTC()
		resp.RetryAfter = &t
	}
	attrs := []any{"outcome", brokerapi.OutcomeDeferred, "code", code}
	if resp.RetryAfter != nil {
		attrs = append(attrs, "retry_after", resp.RetryAfter.Format(time.RFC3339),
			"retry_in_seconds", int64(resp.RetryAfter.Sub(now)/time.Second))
	}
	if code == brokerapi.CodeBlackout {
		attrs = append(attrs, "blackout", b.blackout.String())
	}
	log.Warn("no login may be made now", attrs...)
	return resp
}

// withSession builds the answer that carries a session and, when write is
// set, stores the session first.
func (b *Broker) withSession(ctx context.Context, log *slog.Logger, store Store, outcome string,
	session Session, loggedInAt time.Time, version int, write bool) *brokerapi.Response {
	fields, err := b.driver.Encode(session, loggedInAt)
	if err != nil || len(fields) == 0 {
		log.Error("the session could not be encoded", "outcome", brokerapi.OutcomeFailed, "code", brokerapi.CodeInternal)
		return b.answer(brokerapi.OutcomeFailed, brokerapi.CodeInternal, "")
	}
	if !loggedInAt.IsZero() {
		fields[loggedInAtField] = loggedInAt.UTC().Format(time.RFC3339)
	}
	resp := b.answer(outcome, "", "")
	resp.Session = fields
	resp.Fingerprint = brokerapi.ShortFingerprint(b.driver.Fingerprint(session))
	if !loggedInAt.IsZero() {
		t := loggedInAt.UTC()
		resp.LoggedInAt = &t
	}
	if !write {
		return resp
	}
	if err := b.writeSession(ctx, log, store, session, fields, version); err != nil {
		if errors.Is(err, errSuperseded) {
			// Another writer's login ended this one. Its session is the
			// live one and it is in the store; this one is worthless.
			b.unsaved = nil
			log.Warn("another login replaced this one while it was being stored; nothing stored",
				"outcome", brokerapi.OutcomeFailed, "code", "superseded")
			return b.answer(brokerapi.OutcomeFailed, "superseded", "")
		}
		// The caller still gets the session: it is live and was bought
		// with the previous one. It is offered to the store again on the
		// next request.
		b.unsaved = &heldSession{session: session, loggedInAt: loggedInAt}
		resp.Code = brokerapi.CodeSessionNotStored
		log.Error("the session could not be written to the store; returning it to the caller and keeping it for the next request",
			"code", brokerapi.CodeSessionNotStored, "error", b.redact(err))
		return resp
	}
	b.unsaved = nil
	return resp
}

// loggedInAtField is the one field of the stored session the core owns.
const loggedInAtField = "logged_in_at"

// errSuperseded: another writer stored a session during this login and the
// site no longer accepts this one.
var errSuperseded = errors.New("superseded")

// writeSession stores the session, check-and-set against the version read
// before the login. A transient failure is retried; a refusal is not.
//
// A check-and-set mismatch means something else (a manual run of the
// scheduled job) wrote a session meanwhile. Only one of the two sessions is
// alive -- the site keeps the later login -- so this one is asked about
// once more: if it is still accepted it replaces the other; if not, the
// other stays.
func (b *Broker) writeSession(ctx context.Context, log *slog.Logger, store Store, session Session,
	fields map[string]string, cas int) error {
	data := make(map[string]any, len(fields))
	for k, v := range fields {
		data[k] = v
	}
	var err error
	rechecked := false
	for attempt := 1; attempt <= writeAttempts; attempt++ {
		if _, err = store.Write(ctx, data, cas); err == nil {
			return nil
		}
		if loginvault.IsCASMismatch(err) && !rechecked {
			rechecked = true
			log.Warn("the stored session changed during the login; checking which session is alive")
			_, _, current, rerr := store.ReadFields(ctx)
			if rerr != nil {
				return rerr
			}
			if probe := b.driver.Probe(ctx, session); probe.State != ProbeAlive {
				return errSuperseded
			}
			cas = current
			attempt--
			continue
		}
		if loginvault.IsClientError(err) {
			return err
		}
		if attempt < writeAttempts {
			if serr := b.sleep(ctx, writeDelay); serr != nil {
				break
			}
		}
	}
	return err
}

// writeState stores the record, retrying a transient failure.
func (b *Broker) writeState(ctx context.Context, store Store, state *loginvault.State) error {
	var err error
	for attempt := 1; attempt <= writeAttempts; attempt++ {
		if err = store.WriteState(ctx, state); err == nil || loginvault.IsClientError(err) {
			return err
		}
		if attempt < writeAttempts {
			if serr := b.sleep(ctx, writeDelay); serr != nil {
				break
			}
		}
	}
	return err
}

// storeFailure answers a store read or write that failed.
func (b *Broker) storeFailure(log *slog.Logger, op string, err error) *brokerapi.Response {
	code := brokerapi.CodeStoreUnavailable
	if loginvault.IsClientError(err) {
		code = brokerapi.CodeStoreRejected
	}
	log.Error("the store failed; not logging in",
		"outcome", brokerapi.OutcomeFailed, "code", code, "store_op", op, "error", b.redact(err))
	return b.answer(brokerapi.OutcomeFailed, code, "")
}

// redact prepares a STORE error for a log line: the account's username and
// password are removed and the text is bounded. Store errors are the only
// free text the broker logs; a driver's errors are never logged at all.
func (b *Broker) redact(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, secret := range []string{b.creds.Password, b.creds.Username} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	const max = 300
	if len(msg) > max {
		msg = msg[:max] + "..."
	}
	return msg
}

// stringFields keeps the string-valued fields of a stored secret.
func stringFields(fields map[string]any) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// token is safeToken for something a driver supplied (a detail, a probe
// reason), with the account's username and password removed first. A
// driver's contract is to supply classified tokens only; this is for the
// driver that does not keep it.
func (b *Broker) token(s string) string {
	for _, secret := range []string{b.creds.Password, b.creds.Username} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "redacted")
		}
	}
	return safeToken(s)
}

// safeToken reduces s to a classified token: lower-case letters, digits,
// underscore and colon, at most 64 bytes. Whatever a driver returns as a
// detail or a reason passes through here before it is logged or sent.
func safeToken(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == ':' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
		if sb.Len() >= 64 {
			break
		}
	}
	return sb.String()
}

func validSite(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
