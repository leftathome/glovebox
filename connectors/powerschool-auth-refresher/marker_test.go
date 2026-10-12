package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/leftathome/glovebox/internal/loginvault"
	pslib "github.com/leftathome/powerschool-go"
)

// The login state record is shared with the login broker when one is
// deployed for the same account. These tests are about the two of them --
// or two runs of this job -- not logging in over each other.

// A fresh attempt marker is somebody else's login in flight. This run does
// nothing: no browser, nothing sent to the portal, nothing written, and it
// is not a failure of the job.
func TestRunDoesNotLogInBesideALoginInFlight(t *testing.T) {
	h := newHarness(t, morning(t))
	started := h.clock.Add(-90 * time.Second)
	h.store.state = &loginState{Salt: testSalt, AttemptStartedAt: started, UnconfirmedCount: 1}

	if code := h.run(); code != exitSuccess {
		t.Fatalf("exit %d, want 0", code)
	}
	if h.portalContacts() != 0 {
		t.Fatalf("the portal was contacted %d times beside a login in flight", h.portalContacts())
	}
	if h.store.stateWrites != 0 || h.store.writes != 0 {
		t.Errorf("state writes %d, session writes %d", h.store.stateWrites, h.store.writes)
	}
	if st := h.store.state; !st.AttemptStartedAt.Equal(started) || st.UnconfirmedCount != 1 || st.Blocked {
		t.Errorf("the in-flight attempt was judged: %+v", st)
	}
	if !strings.Contains(h.terminations[0], "outcome=login_in_progress") {
		t.Errorf("termination message: %s", h.terminations[0])
	}
}

// A stale marker is a login that died with its process. It is counted as an
// unconfirmed login -- the second in a row sets the latch -- and as one of
// the day's logins.
func TestRunCountsAStaleAttemptAsAnUnconfirmedLogin(t *testing.T) {
	h := newHarness(t, morning(t))
	died := h.clock.Add(-3 * time.Hour)
	h.store.state = &loginState{Salt: testSalt, AttemptStartedAt: died, UnconfirmedCount: 1,
		CredentialFingerprint: credentialFingerprint(testSalt, testUsername, testPassword)}

	if code := h.run(); code != exitLoginBlocked {
		t.Fatalf("exit %d, want %d (the latch)", code, exitLoginBlocked)
	}
	st := h.store.state
	if !st.Blocked || st.BlockedReason != blockedLoginUnconfirmed || st.UnconfirmedCount != 2 ||
		!st.AttemptStartedAt.IsZero() || st.LoginDayCount != 1 {
		t.Errorf("state = %+v", st)
	}
	if h.logins != 0 {
		t.Error("a login followed two logins without a session")
	}

	// One stale attempt alone is a strike, not a latch: the run goes on.
	h2 := newHarness(t, morning(t))
	h2.store.state = &loginState{Salt: testSalt, AttemptStartedAt: died}
	if code := h2.run(); code != exitSuccess || h2.logins != 1 {
		t.Fatalf("exit %d, logins %d", code, h2.logins)
	}
	// ... and the confirmed login that followed cleared the count.
	if st := h2.store.state; st.UnconfirmedCount != 0 || st.Blocked || st.LoginDayCount != 2 {
		t.Errorf("state after the login = %+v", st)
	}
}

// The marker is in the record while the browser runs, and gone on every
// way out -- whatever became of the login.
func TestRunWritesTheAttemptMarkerAroundTheLogin(t *testing.T) {
	for name, c := range map[string]struct {
		login     error
		wantCount int // logins counted for the day
	}{
		"confirmed":         {nil, 1},
		"rejected":          {&loginError{kind: loginRejected, err: errors.New("rejected")}, 1},
		"unknown":           {&loginError{kind: loginFailedUnknown, err: errors.New("timeout")}, 1},
		"before the submit": {&loginError{kind: loginFailedBeforeSubmit, err: errors.New("no browser")}, 0},
	} {
		h := newHarness(t, morning(t))
		h.loginResults = []error{c.login, c.login}
		start := h.clock.UTC()
		var during []loginState
		h.onLogin = func() { during = append(during, *h.store.state) }
		h.run()
		if len(during) == 0 || !during[0].AttemptStartedAt.Equal(start) {
			t.Errorf("%s: no attempt marker in the record during the login: %+v", name, during)
		}
		st := h.store.state
		if !st.AttemptStartedAt.IsZero() {
			t.Errorf("%s: the marker was left behind: %+v", name, st)
		}
		if st.LoginDayCount != c.wantCount || (c.wantCount > 0 && st.LoginDay != "2026-10-12") {
			t.Errorf("%s: day count = %q %d, want %d", name, st.LoginDay, st.LoginDayCount, c.wantCount)
		}
	}
}

// The marker is written check-and-set. If the record changed after this
// run read it -- the broker started a login of its own -- the write is
// refused and this run does not log in.
//
// For a scheduled run that means NO LOGIN WAS MADE in its window, and that
// must be visible: the run exits 8, so the Job shows as failed. It is safe
// to fail it only because Kubernetes never runs a failed login Job again
// (backoffLimit 0, restartPolicy Never; pinned by
// scripts/test-chart-powerschool-login.sh).
func TestRunDoesNotLogInWhenTheRecordChangedUnderIt(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.writeStateErrs = []error{&vaultapi.ResponseError{HTTPMethod: "PUT", StatusCode: 400,
		Errors: []string{"check-and-set parameter did not match the current version"}}}

	if code := h.run(); code != exitLoginStateConflict || exitLoginStateConflict != 8 {
		t.Fatalf("exit %d, want %d", code, exitLoginStateConflict)
	}
	if h.logins != 0 || h.signInChecks > 1 {
		t.Fatal("logged in on a record somebody else had changed")
	}
	if !strings.Contains(h.terminations[0], "outcome=login_state_conflict") || !strings.Contains(h.terminations[0], "no login was made") {
		t.Errorf("termination message: %s", h.terminations[0])
	}
	if !strings.Contains(h.logs.String(), `"outcome":"login_state_conflict"`) || !strings.Contains(h.logs.String(), `"level":"ERROR"`) {
		t.Error("the conflict is not an ERROR line with its own outcome")
	}
}

// Vault stores the marker and answers 500. The retry is refused -- against
// this run's own write. The run recognises it, logs in once, and clears the
// marker; it does not walk away from a marker it wrote.
func TestRunRecognisesItsOwnMarkerAfterAnUnacknowledgedWrite(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.applyStateThenFail = map[int]bool{1: true}

	if code := h.run(); code != exitSuccess {
		t.Fatalf("exit %d: %s", code, h.terminations[0])
	}
	st := h.store.state
	if h.logins != 1 || !st.AttemptStartedAt.IsZero() || st.LoginDayCount != 1 || st.UnconfirmedCount != 0 {
		t.Fatalf("logins %d, state %+v", h.logins, st)
	}
}

// And the outcome: an unconfirmed login whose record Vault stored but did
// not acknowledge is one unconfirmed login, not two (two would latch).
func TestRunCountsAnUnacknowledgedOutcomeOnce(t *testing.T) {
	h := newHarness(t, morning(t))
	h.probeResults = []error{pslib.ErrSessionExpired} // the new session is refused
	h.store.applyStateThenFail = map[int]bool{2: true}

	if code := h.run(); code != exitLoginUnconfirmed {
		t.Fatalf("exit %d: %s", code, h.terminations[0])
	}
	if st := h.store.state; st.UnconfirmedCount != 1 || st.Blocked || st.LoginDayCount != 1 {
		t.Fatalf("state = %+v", st)
	}
}

// What the broker budgets against: this job's logins are counted into the
// same per-day count, on the Pacific calendar.
func TestRunCountsItsLoginsForTheSharedDailyBudget(t *testing.T) {
	h := newHarness(t, morning(t))
	h.store.state = &loginState{Salt: testSalt, LoginDay: "2026-10-12", LoginDayCount: 2}
	if code := h.run(); code != exitSuccess {
		t.Fatalf("exit %d", code)
	}
	if st := h.store.state; st.LoginDay != "2026-10-12" || st.LoginDayCount != 3 {
		t.Errorf("count = %q %d, want 3", st.LoginDay, st.LoginDayCount)
	}
	if got := loginvault.DayOf(h.clock, h.cfg.Blackout.loc); got != "2026-10-12" {
		t.Errorf("local day = %s", got)
	}
}
