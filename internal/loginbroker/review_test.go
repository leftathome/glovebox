package loginbroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/dailywindow"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
	"github.com/leftathome/glovebox/internal/loginvault"
)

// Tests for the findings of the security review of !99 (abb19f1a).

// --- 1. the blackout is checked on the clock the login starts on -----------

// The decision is not made on a clock read before the slow steps. At
// 15:56:00 a login may start (the headroom is 150 s). The probe of the
// stored session then takes 100 seconds: it is 15:57:40, inside the
// headroom, and no login may start any more.
func TestASlowProbeDoesNotCarryAStaleClockPastTheHeadroom(t *testing.T) {
	r := newRig(t, afternoonBlackout)
	r.clock = time.Date(2026, 10, 12, 15, 56, 0, 0, losAngeles)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.onProbe = func(string) { r.clock = r.clock.Add(100 * time.Second) }
	r.driver.succeed("new")

	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeBlackout)
	if r.driver.loginCalls != 0 {
		t.Fatalf("a login was started at %s on a clock read at 15:56:00", r.clock.In(losAngeles).Format("15:04:05"))
	}
	if r.back.stateWrites != 0 {
		t.Error("an attempt marker was written for a login that was never started")
	}
}

// The same for the steps between the decision and the driver: the store
// taking its time over the attempt marker must not push the login past the
// headroom either. The marker is cleared again.
func TestASlowStoreBeforeTheLoginDoesNotPushItPastTheHeadroom(t *testing.T) {
	r := newRig(t, afternoonBlackout)
	r.clock = time.Date(2026, 10, 12, 15, 57, 0, 0, losAngeles)
	r.store("old", ProbeDead, 9*time.Hour)
	r.back.onStateWrite = func(n int) {
		if n == 1 {
			r.clock = r.clock.Add(45 * time.Second) // Vault was slow
		}
	}
	r.driver.succeed("new")

	r.want(r.ask(fp("old")), brokerapi.OutcomeDeferred, brokerapi.CodeBlackout)
	if r.driver.loginCalls != 0 {
		t.Fatal("a login was started after the headroom had been used up by the store")
	}
	if st := r.back.state; st == nil || !st.AttemptStartedAt.IsZero() || st.LoginDayCount != 0 || !st.RequestLoginAt.IsZero() {
		t.Errorf("state after a login that was not started = %+v", st)
	}
}

// A first attempt that fails before the submit, slowly, is not followed by
// a second one that would start inside the headroom.
func TestASlowFirstAttemptIsNotRetriedIntoTheBlackout(t *testing.T) {
	r := newRig(t, afternoonBlackout)
	r.clock = time.Date(2026, 10, 12, 15, 55, 0, 0, losAngeles)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.onLogin = func(context.Context) { r.clock = r.clock.Add(140 * time.Second) }
	r.driver.failWith(LoginNotSubmitted, "browser_or_network_before_submit")
	r.driver.succeed("new") // would be the retry, at 15:57:30

	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeFailed, brokerapi.CodeLoginNotSubmitted)
	if r.driver.loginCalls != 1 {
		t.Fatalf("driver logins = %d: the retry started inside the headroom", r.driver.loginCalls)
	}
}

// Every call of the driver's Login is cancelled after the headroom, so
// "the next headroom is clear" really bounds when a submission can happen.
func TestEachLoginCallIsBoundedByTheHeadroom(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	var left time.Duration
	r.driver.onLogin = func(ctx context.Context) {
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
	}
	r.driver.succeed("new")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if left <= 0 || left > r.b.LoginHeadroom() {
		t.Errorf("the login call's deadline was %s away; want within the %s headroom", left, r.b.LoginHeadroom())
	}
}

// The interval is re-read too: nothing here makes it shorter, and a clock
// that was set back between the decision and the login closes the gate.
func TestTheIntervalIsCheckedAgainOnTheClockTheLoginStartsOn(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 21*time.Minute)
	r.back.onStateWrite = func(n int) {
		if n == 1 {
			r.clock = r.clock.Add(-5 * time.Minute) // the clock was corrected backwards
		}
	}
	r.driver.succeed("new")
	r.want(r.ask(fp("old")), brokerapi.OutcomeDeferred, brokerapi.CodeMinInterval)
	if r.driver.loginCalls != 0 {
		t.Error("logged in inside the interval")
	}
}

// --- 2. two writers -----------------------------------------------------------

// The attempt marker is written check-and-set against the record as it was
// read. If somebody else changed the record in between -- here, the
// scheduled job starting a login of its own -- there is no login.
func TestARecordChangedSinceItWasReadMeansNoLogin(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	theirs := morning()
	r.driver.onProbe = func(string) {
		r.back.otherWriter(func(s *loginvault.State) { s.AttemptStartedAt = theirs })
	}
	r.driver.succeed("new")

	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeStateConflict)
	if r.driver.loginCalls != 0 {
		t.Fatal("logged in on a record somebody else had changed")
	}
	if st := r.back.state; !st.AttemptStartedAt.Equal(theirs) || st.UnconfirmedCount != 0 || st.Blocked {
		t.Errorf("the other writer's record was overwritten: %+v", st)
	}
}

// A second instance arriving while another's login is in flight does not
// take the marker for a dead process: nothing is counted, nothing is
// latched, nothing is written, and the site is not asked anything.
func TestAnAttemptInFlightElsewhereIsNotCountedAsAFailure(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxFailures = 1 }) // one strike would latch
	r.store("old", ProbeDead, 9*time.Hour)
	started := morning().Add(-40 * time.Second)
	r.back.state = &loginvault.State{Salt: "salt-for-tests", AttemptStartedAt: started,
		CredentialFingerprint: loginvault.CredentialFingerprint("salt-for-tests", testUsername, testPassword)}

	for i := 0; i < 3; i++ {
		resp := r.ask(fp("old"))
		r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeLoginInProgress)
		if resp.RetryAfter == nil || !resp.RetryAfter.Equal(started.Add(loginvault.AttemptFreshFor)) {
			t.Errorf("retry_after = %v", resp.RetryAfter)
		}
		r.clock = r.clock.Add(2 * time.Minute)
	}
	st := r.back.state
	if st.Blocked || st.UnconfirmedCount != 0 || !st.AttemptStartedAt.Equal(started) || r.back.stateWrites != 0 {
		t.Errorf("an in-flight attempt was judged: %+v (state writes %d)", st, r.back.stateWrites)
	}
	if r.driver.loginCalls != 0 || len(r.driver.probeCalls) != 0 {
		t.Errorf("the site was contacted: logins %d, probes %d", r.driver.loginCalls, len(r.driver.probeCalls))
	}

	// The other instance finishes and records its outcome: business as
	// usual, with its session.
	r.store("theirs", ProbeAlive, time.Minute)
	r.back.otherWriter(func(s *loginvault.State) { s.AttemptStartedAt = time.Time{}; s.RequestLoginAt = started })
	r.want(r.ask(fp("old")), brokerapi.OutcomeCurrent, "")
}

// An outcome that has happened is recorded on top of whatever is in the
// record by then: it neither gets lost nor wipes out the other writer's
// change.
func TestAnOutcomeIsRecordedOnTopOfAConcurrentChange(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.logins = append(r.driver.logins, func(Credentials) (Session, error) {
		// While the browser runs, an operator edits the record.
		r.back.otherWriter(func(s *loginvault.State) { s.ServedRequest = "edited-by-hand" })
		return nil, &LoginError{Kind: LoginRejected, Detail: "credentials_rejected"}
	})
	r.want(r.ask(fp("old")), brokerapi.OutcomeLatched, brokerapi.CodeCredentialsRejected)
	st := r.back.state
	if !st.Blocked || st.BlockedReason != "credentials_rejected" || st.LoginDayCount != 1 {
		t.Errorf("the rejection was not recorded: %+v", st)
	}
	if st.ServedRequest != "edited-by-hand" {
		t.Errorf("the concurrent change was overwritten: %+v", st)
	}
}

// --- 5. the daily budget -------------------------------------------------------

// Six logins in one local day, however they are spaced and whatever keeps
// ending the sessions; the seventh request is told so, with the next local
// midnight, and nothing more is sent to the site that day.
func TestTheDailyLoginBudget(t *testing.T) {
	r := newRig(t, func(c *Config) {
		c.Blackout = dailywindow.Schedule{{Windows: []dailywindow.Window{{Start: 2 * 3600, End: 3 * 3600}}, Loc: losAngeles}}
	})
	r.clock = time.Date(2026, 10, 12, 4, 0, 0, 0, losAngeles)
	prev := "none"
	r.store(prev, ProbeDead, 9*time.Hour)
	for i := 1; i <= CeilingDailyLogins; i++ {
		id := "s" + string(rune('0'+i))
		r.driver.succeed(id)
		r.want(r.ask(fp(prev)), brokerapi.OutcomeRefreshed, "")
		r.driver.set(id, ProbeDead) // and it dies again
		prev = id
		r.clock = r.clock.Add(time.Hour)
	}
	if got := r.back.state.LoginsOn("2026-10-12"); got != CeilingDailyLogins {
		t.Fatalf("logins counted for the day = %d", got)
	}

	logins, probes := r.driver.loginCalls, len(r.driver.probeCalls)
	midnight := time.Date(2026, 10, 13, 0, 0, 0, 0, losAngeles)
	for i := 0; i < 4; i++ {
		resp := r.ask(fp(prev))
		r.want(resp, brokerapi.OutcomeBudgetExhausted, "")
		if resp.RetryAfter == nil || !resp.RetryAfter.Equal(midnight) {
			t.Errorf("retry_after = %v, want the next local midnight %s", resp.RetryAfter, midnight.UTC())
		}
		r.clock = r.clock.Add(time.Hour)
	}
	if r.driver.loginCalls != logins {
		t.Fatalf("logins over budget: %d", r.driver.loginCalls-logins)
	}
	_ = probes

	// A restart does not refill it: the count is in the record.
	r.start()
	r.want(r.ask(fp(prev)), brokerapi.OutcomeBudgetExhausted, "")

	// The local day -- not the UTC one -- turns over at midnight Pacific.
	r.clock = time.Date(2026, 10, 12, 23, 59, 0, 0, losAngeles) // already the 13th in UTC
	r.want(r.ask(fp(prev)), brokerapi.OutcomeBudgetExhausted, "")
	r.clock = time.Date(2026, 10, 13, 0, 1, 0, 0, losAngeles)
	r.driver.succeed("tomorrow")
	r.want(r.ask(fp(prev)), brokerapi.OutcomeRefreshed, "")
	if st := r.back.state; st.LoginDay != "2026-10-13" || st.LoginDayCount != 1 {
		t.Errorf("the new day's count = %s %d", st.LoginDay, st.LoginDayCount)
	}
}

// The budget is shared with whoever else logs in for the account (the
// scheduled job counts into the same record), and failed logins that may
// have submitted the credentials spend it like successful ones. A login
// that never got to the submit does not.
func TestTheBudgetCountsEveryWorkloadAndEverySubmission(t *testing.T) {
	r := newRig(t, func(c *Config) { c.DailyBudget = 3; c.MaxFailures = 5 })
	r.store("old", ProbeDead, 9*time.Hour)
	r.back.state = &loginvault.State{Salt: "salt-for-tests", LoginDay: "2026-10-12", LoginDayCount: 1} // the scheduled job, at 05:00

	r.driver.failWith(LoginNotSubmitted, "browser_or_network_before_submit")
	r.driver.failWith(LoginNotSubmitted, "browser_or_network_before_submit")
	r.want(r.ask(fp("old")), brokerapi.OutcomeFailed, brokerapi.CodeLoginNotSubmitted)
	if got := r.back.state.LoginDayCount; got != 1 {
		t.Fatalf("a login that submitted nothing was counted: %d", got)
	}

	r.clock = r.clock.Add(time.Hour)
	r.driver.failWith(LoginUnknown, "after_or_during_submit")
	r.want(r.ask(fp("old")), brokerapi.OutcomeFailed, brokerapi.CodeLoginFailed)
	r.clock = r.clock.Add(time.Hour)
	r.driver.succeed("s1")
	r.want(r.ask(fp("old")), brokerapi.OutcomeRefreshed, "")
	if got := r.back.state.LoginDayCount; got != 3 {
		t.Fatalf("count = %d, want 3 (the job's one, a failed one, a good one)", got)
	}
	r.driver.set("s1", ProbeDead)
	r.clock = r.clock.Add(time.Hour)
	r.want(r.ask(fp("s1")), brokerapi.OutcomeBudgetExhausted, "")
}

// Configuration can lower the budget. It cannot raise it.
func TestTheBudgetHasACompiledInCeiling(t *testing.T) {
	d := &fakeDriver{states: map[string]ProbeState{}}
	base := Config{
		Driver: d, Credentials: Credentials{Username: "u", Password: "p"},
		Connect: func(context.Context) (Store, error) { return nil, nil },
		NewSalt: func() (string, error) { return "s", nil },
	}
	for n, ok := range map[int]bool{0: true, 1: true, CeilingDailyLogins: true, CeilingDailyLogins + 1: false, 100: false, -1: false} {
		c := base
		c.DailyBudget = n
		b, err := New(c)
		if (err == nil) != ok {
			t.Errorf("budget %d: err = %v", n, err)
		}
		if n == 0 && err == nil {
			if got, loc := b.DailyBudget(); got != CeilingDailyLogins || loc != time.UTC {
				t.Errorf("default budget = %d in %v", got, loc)
			}
		}
	}
	if CeilingDailyLogins != 6 {
		t.Errorf("the ceiling is %d; it is 6 by the owner's decision and changes only with one", CeilingDailyLogins)
	}
	// The calendar is the driver's, else the configured blackout's.
	d.limits = &Constraints{Location: losAngeles}
	c := base
	c.Driver = constrainedDriver{d}
	if b, err := New(c); err != nil || func() string { _, l := b.DailyBudget(); return l.String() }() != "America/Los_Angeles" {
		t.Errorf("budget zone from the driver: %v", err)
	}
}

func TestBudgetExhaustionIsVisibleInTheMetrics(t *testing.T) {
	r := newRig(t, func(c *Config) { c.DailyBudget = 1 })
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.succeed("s1")
	r.ask(fp("old"))
	r.driver.set("s1", ProbeDead)
	r.clock = r.clock.Add(time.Hour)
	r.ask(fp("s1"))
	r.b.Wait()

	_, _, srv := newTestServer(t, r)
	body := scrape(t, srv)
	for _, want := range []string{
		`login_broker_refresh_total{code="",outcome="budget_exhausted",site="testsite"} 1`,
		`login_broker_logins_today{site="testsite"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func scrape(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", rec.Code)
	}
	return rec.Body.String()
}

// --- 9. the cooldown is in the record -------------------------------------------

func TestTheCooldownSurvivesARestart(t *testing.T) {
	r := newRig(t, nil)
	r.store("old", ProbeDead, 9*time.Hour)
	r.driver.failWith(LoginNotSubmitted, "signin_page_unreachable:network")
	r.driver.failWith(LoginNotSubmitted, "signin_page_unreachable:network")
	r.want(r.ask(fp("old")), brokerapi.OutcomeFailed, brokerapi.CodeLoginNotSubmitted)
	until := r.back.state.CooldownUntil
	if !until.Equal(r.clock.Add(DefaultCooldown)) {
		t.Fatalf("cooldown_until = %v", until)
	}

	r.start() // a new pod, seconds later
	r.clock = r.clock.Add(20 * time.Second)
	resp := r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeCooldown)
	if resp.RetryAfter == nil || !resp.RetryAfter.Equal(until) || r.driver.loginCalls != 2 {
		t.Errorf("retry_after %v, driver logins %d", resp.RetryAfter, r.driver.loginCalls)
	}

	// A cooldown years ahead (a hand edit, a broken clock) holds for one
	// cooldown, not for years.
	r.back.otherWriter(func(s *loginvault.State) { s.CooldownUntil = r.clock.Add(24 * 365 * time.Hour) })
	resp = r.ask(fp("old"))
	r.want(resp, brokerapi.OutcomeDeferred, brokerapi.CodeCooldown)
	if !resp.RetryAfter.Equal(r.clock.Add(DefaultCooldown)) {
		t.Errorf("retry_after = %v", resp.RetryAfter)
	}
}
