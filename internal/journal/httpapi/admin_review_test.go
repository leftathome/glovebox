package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// Tests of the Task 27 security review.

// panicReader panics on the first Read, as a broken body reader would.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("body reader exploded") }

// I-1: a handler that panics is audited as a panic, never as 200, and the
// caller still gets route's 500.
func TestAdminPanicIsAuditedAsAPanic(t *testing.T) {
	r := newAuthRig(t)
	for _, c := range []struct{ path, endpoint string }{
		{journalapi.HeldPath(uid(2), journalapi.ActionConfirm), "admin_held"},
		{journalapi.PathAdminErase, "admin_erase"},
		{journalapi.PathAdminRescan, "admin_rescan"},
	} {
		req := httptest.NewRequest("POST", c.path, panicReader{})
		req.Header.Set("Authorization", "Bearer admin")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.s.Handler().ServeHTTP(rec, req)
		if rec.Code != 500 || decodeRefusal(t, rec).Reason != journalapi.ReasonInternal {
			t.Errorf("%s: %d %s", c.endpoint, rec.Code, rec.Body.String())
		}
		logs := r.logs.String()
		if !regexp.MustCompile(`level=ERROR msg="journal admin" caller=` + journaltest.SAAdmin + ` endpoint=` + c.endpoint + ` .*outcome=panic\n`).MatchString(logs) {
			t.Errorf("%s: no panic audit line:\n%s", c.endpoint, logs)
		}
		if regexp.MustCompile(`msg="journal admin" caller=\S+ endpoint=` + c.endpoint + ` .*code=`).MatchString(logs) {
			t.Errorf("%s: the panic was audited with a code:\n%s", c.endpoint, logs)
		}
	}
	if r.s.rescanning.Load() {
		t.Errorf("a panicking rescan request took the guard")
	}
}

// T-1: the admin routes answer POST only; any other method, lowercase post
// included, is 404 and changes nothing.
func TestAdminMethodsArePinned(t *testing.T) {
	r := newRig(t)
	seed(r)
	id := holdOne(r)
	for _, path := range []string{journalapi.HeldPath(id, journalapi.ActionConfirm), journalapi.HeldPath(id, journalapi.ActionDiscard),
		journalapi.PathAdminErase, journalapi.PathAdminRescan} {
		for _, m := range []string{"PUT", "GET", "post", "Post", "DELETE", "PATCH"} {
			var resp journalapi.Response
			if code := r.do(m, path, "admin", `{"data_subject": "e_child1", "confirm": "e_child1"}`, &resp); code != 404 || resp.Reason != journalapi.ReasonNotFound {
				t.Errorf("%s %s: %d %+v", m, path, code, resp)
			}
		}
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
		t.Errorf("held snapshot changed: %s", st)
	}
	if n := r.erasures(); n != 0 {
		t.Errorf("erased: %d", n)
	}
	if r.s.rescanning.Load() || strings.Contains(r.logs.String(), "journal rescan started") || strings.Contains(r.logs.String(), `msg="journal admin"`) {
		t.Errorf("an admin handler ran")
	}
}

// T-2: the operator check comes before any parsing: a non-operator with a
// bad path, a query string or an oversized body is 403, on every endpoint.
func TestOperatorCheckComesFirst(t *testing.T) {
	r := newAuthRig(t)
	big := strings.Repeat(" ", 1<<20)
	for _, c := range []struct {
		path string
		body any
	}{
		{journalapi.PathAdminHeld + "not-a-uuid:confirm", nil},
		{journalapi.PathAdminHeld + uid(2) + ":approve", nil},
		{journalapi.HeldPath(uid(2), journalapi.ActionConfirm) + "?x=1", nil},
		{journalapi.HeldPath(uid(2), journalapi.ActionConfirm), big},
		{journalapi.PathAdminErase + "?x=1", nil},
		{journalapi.PathAdminErase, big},
		{journalapi.PathAdminErase, `{"data_subject": "a/b"}`},
		{journalapi.PathAdminRescan + "?all=true", nil},
		{journalapi.PathAdminRescan, big},
		{journalapi.PathAdminRescan, `{"all": "x"}`},
	} {
		for _, tok := range []string{"gw", "ps", "dash"} {
			var resp journalapi.Response
			if code := r.do("POST", c.path, tok, c.body, &resp); code != 403 || resp.Reason != journalapi.ReasonPolicy {
				t.Errorf("%s %s: %d %+v", tok, c.path, code, resp)
			}
		}
	}
	if strings.Contains(r.logs.String(), `msg="journal admin"`) {
		t.Errorf("a refused request was audited as an operator request")
	}
}

// T-3: erase runs under the request's context: a request cancelled while
// the erase waits erases nothing.
func TestEraseFollowsTheRequestContext(t *testing.T) {
	r := newRig(t)
	seed(r)
	// Hold the member's author lock, the first lock Erase takes.
	tx, err := r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "glovebox_journal:author:e_child1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", r.srv.URL+journalapi.PathAdminErase,
		strings.NewReader(`{"data_subject": "e_child1", "confirm": "e_child1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	waitFor(t, "the erase to wait for the author lock", func() bool {
		var n int
		_ = r.db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n)
		return n > 0
	})
	cancel()
	<-done
	waitFor(t, "the erase handler to return", func() bool {
		return regexp.MustCompile(`msg="journal admin" caller=\S+ endpoint=admin_erase data_subject=e_child1 code=500`).MatchString(r.logs.String())
	})
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := r.erasures(); n != 0 {
		t.Errorf("a cancelled erase erased: %d", n)
	}
	var st journalapi.StateResponse
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child1", "gw", nil, &st); len(st.Rows) != 1 {
		t.Errorf("rows went: %+v", st)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// T-4: a 413 closes the connection (the rest of the body is never read).
func TestAdminTooLargeClosesTheConnection(t *testing.T) {
	r := newAuthRig(t)
	for _, path := range []string{journalapi.HeldPath(uid(2), journalapi.ActionConfirm), journalapi.PathAdminErase, journalapi.PathAdminRescan} {
		req, _ := http.NewRequest("POST", r.srv.URL+path, strings.NewReader(`{}`+strings.Repeat(" ", 8<<10)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 413 || !resp.Close {
			t.Errorf("%s: %d, close %v", path, resp.StatusCode, resp.Close)
		}
	}
}

// T-4: on held, the query string is refused before the path is parsed.
func TestHeldQueryCheckComesBeforeThePath(t *testing.T) {
	r := newAuthRig(t)
	var resp journalapi.Response
	if code := r.do("POST", journalapi.PathAdminHeld+"not-a-uuid:approve?x=1", "admin", nil, &resp); code != 400 || resp.Detail != "POST /v1/admin/held takes no query parameters" {
		t.Errorf("%d %+v", code, resp)
	}
	if !strings.Contains(r.logs.String(), `endpoint=admin_held snapshot_id="(not a snapshot id)" action="(not an action)" code=400`) {
		t.Errorf("audit:\n%s", r.logs.String())
	}
}

// S-1: a panic inside the rescan goroutine is logged with the caller, frees
// the guard, and the server goes on.
func TestRescanPanicIsRecovered(t *testing.T) {
	r := newRig(t)
	r.scan.panicVersion.Store(true)
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", nil, nil); code != 202 {
		t.Fatal(code)
	}
	r.waitRescan()
	waited := make(chan struct{})
	go func() { r.s.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after a panicked rescan")
	}
	if !regexp.MustCompile(`level=ERROR msg="journal rescan panicked" caller=` + journaltest.SAAdmin + ` panic="scanner version unavailable"\n`).MatchString(r.logs.String()) {
		t.Errorf("no panic line:\n%s", r.logs.String())
	}
	r.scan.panicVersion.Store(false)
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", nil, nil); code != 202 {
		t.Fatalf("after a panic: %d", code)
	}
	r.waitRescan()
	if !strings.Contains(r.logs.String(), "journal rescan finished") {
		t.Errorf("the next rescan did not run")
	}
}

// S-2: Wait returns only once the background rescan has returned.
func TestWaitDrainsTheRescan(t *testing.T) {
	r := newRig(t)
	r.s.Wait() // nothing running
	r.do("POST", journalapi.PathRecords, "ps", assignments(5, "e_child1", "powerschool", T0.Add(-time.Minute), one("k1", "e_child1", `{"title": "A", "comment": "fine"}`)), nil)
	entered, release := r.scan.hold()
	defer release()
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, nil); code != 202 {
		t.Fatal(code)
	}
	<-entered
	waited := make(chan struct{})
	go func() { r.s.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned while the rescan ran")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return")
	}
	if r.s.rescanning.Load() || !strings.Contains(r.logs.String(), "journal rescan finished") {
		t.Errorf("Wait returned before the rescan had")
	}
}

// S-3: Deps.Context is required.
func TestNewNeedsAContext(t *testing.T) {
	r := newAuthRig(t)
	d := r.s.d
	if _, err := New(d); err != nil {
		t.Fatalf("complete deps: %v", err)
	}
	d.Context = nil
	if _, err := New(d); err == nil {
		t.Error("New accepted no context")
	}
}

// S-5: a snapshot alone (no rows) is data: its member is not unknown.
func TestEraseOfASnapshotOnlyMemberIsNotUnknown(t *testing.T) {
	r := newRig(t)
	var resp journalapi.Response
	empty := assignments(7, "e_child2", "powerschool", T0.Add(-time.Hour))
	empty.Complete = false
	if code := r.do("POST", journalapi.PathRecords, "ps", empty, &resp); code != 200 {
		t.Fatalf("%d %+v", code, resp)
	}
	ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(`{
	  "people": {"e_parent1": {"agent": "agent-parent1", "guardian_of": ["e_child1"]}, "e_child1": {"agent": "agent-child1"}},
	  "household_members": ["e_parent1", "e_child1"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	r.ros.Store(ros)
	var er journalapi.EraseResponse
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child2"), &er); code != 200 || er.Snapshots != 1 || er.Current != 0 || er.Events != 0 || er.SubjectUnknown {
		t.Errorf("%d %+v", code, er)
	}
}

// S-5: an erase of an id that is neither on the roster nor holds any data
// still erases (a ban), but says so and logs a warning.
func TestEraseHintsUnknownSubjects(t *testing.T) {
	r := newRig(t)
	seed(r)
	var er journalapi.EraseResponse
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_parent2"), &er); code != 200 || er.SubjectUnknown {
		t.Errorf("on the roster, no data: %d %+v", code, er)
	}
	// e_child2 leaves the roster but its data stays: not unknown.
	ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(`{
	  "people": {
	    "e_parent1": {"agent": "agent-parent1", "guardian_of": ["e_child1"]},
	    "e_parent2": {"agent": "agent-parent2", "guardian_of": ["e_child1"]},
	    "e_child1":  {"agent": "agent-child1"}
	  },
	  "household_members": ["e_parent1", "e_parent2", "e_child1"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	r.ros.Store(ros)
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child2"), &er); code != 200 || er.SubjectUnknown || er.Current != 1 {
		t.Errorf("off the roster with data: %d %+v", code, er)
	}
	if strings.Contains(r.logs.String(), "unknown member") {
		t.Errorf("warned about a known member:\n%s", r.logs.String())
	}
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_typo"), &er); code != 200 || !er.SubjectUnknown || er.DataSubject != "e_typo" {
		t.Errorf("unknown: %d %+v", code, er)
	}
	if n := r.erasures(); n != 3 {
		t.Errorf("erasure rows: %d", n)
	}
	if !regexp.MustCompile(`level=WARN msg="journal erase of an unknown member: not on the roster and no data held" caller=` + journaltest.SAAdmin + ` data_subject=e_typo\n`).MatchString(r.logs.String()) {
		t.Errorf("no warning:\n%s", r.logs.String())
	}
}
