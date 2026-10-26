package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// holdOne stores snapshot uid(102) of e_child1 as held (it would remove 4 of
// 5 live keys) and returns its id.
func holdOne(r *rig) string {
	r.t.Helper()
	var recs []journalapi.Record
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		recs = append(recs, one(k, "e_child1", `{"title": "A"}`))
	}
	r.do("POST", journalapi.PathRecords, "ps", assignments(101, "e_child1", "powerschool", T0.Add(-30*time.Minute), recs...), nil)
	var resp journalapi.Response
	if code := r.do("POST", journalapi.PathRecords, "ps", assignments(102, "e_child1", "powerschool", T0.Add(-20*time.Minute), recs[:1]...), &resp); code != 200 || resp.Status != journalapi.StatusHeld {
		r.t.Fatalf("not held: %d %+v", code, resp)
	}
	return uid(102)
}

func (r *rig) snapshotStatus(id string) string {
	r.t.Helper()
	var st string
	if err := r.db.QueryRow(`SELECT status FROM snapshot WHERE id = $1::uuid`, id).Scan(&st); err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *rig) erasures() int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM erasure`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func eraseBody(subject string) journalapi.EraseRequest {
	return journalapi.EraseRequest{DataSubject: subject, Confirm: subject}
}

// waitRescan waits until no rescan runs.
func (r *rig) waitRescan() {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.s.rescanning.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.s.rescanning.Load() {
		r.t.Fatal("the rescan did not finish")
	}
}

func TestHeldConfirmAndDiscard(t *testing.T) {
	r := newRig(t)
	id := holdOne(r)
	var resp journalapi.Response
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "gw", nil, &resp); code != 403 || resp.Reason != journalapi.ReasonPolicy {
		t.Errorf("non-operator confirm: %d %+v", code, resp)
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
		t.Fatalf("a refused confirm changed the snapshot: %s", st)
	}
	resp = journalapi.Response{}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", nil, &resp); code != 200 || resp.Status != journalapi.StatusApplied || resp.Stats == nil || resp.Stats.Removed != 4 {
		t.Errorf("confirm: %d %+v", code, resp)
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusApplied {
		t.Errorf("after confirm: %s", st)
	}
	resp = journalapi.Response{}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionDiscard), "admin", nil, &resp); code != 409 || resp.Reason != journalapi.ReasonConflict {
		t.Errorf("discard after confirm: %d %+v", code, resp)
	}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", nil, &resp); code != 409 || resp.Reason != journalapi.ReasonConflict {
		t.Errorf("confirm after confirm: %d %+v", code, resp)
	}
	for _, action := range []string{journalapi.ActionConfirm, journalapi.ActionDiscard} {
		resp = journalapi.Response{}
		if code := r.do("POST", journalapi.HeldPath(uid(77), action), "admin", nil, &resp); code != 404 || resp.Reason != journalapi.ReasonNotFound {
			t.Errorf("unknown %s: %d %+v", action, code, resp)
		}
	}
}

func TestHeldDiscard(t *testing.T) {
	r := newRig(t)
	id := holdOne(r)
	var resp journalapi.Response
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionDiscard), "dash", nil, &resp); code != 403 || resp.Reason != journalapi.ReasonPolicy {
		t.Errorf("non-operator discard: %d %+v", code, resp)
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
		t.Fatalf("a refused discard changed the snapshot: %s", st)
	}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionDiscard), "admin", nil, &resp); code != 200 || resp.Status != journalapi.StatusDiscarded || resp.Stats != nil {
		t.Errorf("discard: %d %+v", code, resp)
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusDiscarded {
		t.Errorf("after discard: %s", st)
	}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", nil, &resp); code != 409 || resp.Reason != journalapi.ReasonConflict {
		t.Errorf("confirm after discard: %d %+v", code, resp)
	}
	var st journalapi.StateResponse
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child1", "gw", nil, &st); len(st.Rows) != 5 {
		t.Errorf("a discarded snapshot removed rows: %d", len(st.Rows))
	}
}

// A confirm re-checks the writer under the current policy (Task 17): a
// writer that lost the right gets nothing applied, the snapshot stays held.
func TestHeldConfirmRechecksTheWriter(t *testing.T) {
	r := newRig(t)
	id := holdOne(r)
	for name, svc := range map[string]*policy.Service{
		"type withdrawn": {WriteTypes: []string{"attendance_day"}, WriteSubjects: policy.RosterSubjects},
		"no policy":      nil,
	} {
		services := testServices()
		if svc == nil {
			delete(services, journaltest.SAPowerSchool)
		} else {
			services[journaltest.SAPowerSchool] = *svc
		}
		pol, err := policy.New(services, journaltest.Registry(t))
		if err != nil {
			t.Fatal(err)
		}
		r.s.d.Policy = pol
		var resp journalapi.Response
		if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", nil, &resp); code != 403 || resp.Reason != journalapi.ReasonPolicy || !strings.Contains(resp.Detail, "discard") {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
		if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
			t.Errorf("%s: %s", name, st)
		}
	}
}

func TestWriterMayWrite(t *testing.T) {
	r := newAuthRig(t)
	reg := journaltest.Registry(t)
	assignment, _ := reg.Active("assignment")
	sc, rej := record.ParseScope([]journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"}, {"data_subject", "e_child1"}, {"course", "c1"}})
	if rej != nil {
		t.Fatal(rej)
	}
	if rej := r.s.writerMayWrite(journaltest.SAPowerSchool, assignment, sc); rej != nil {
		t.Errorf("the writer may write: %+v", rej)
	}
	if rej := r.s.writerMayWrite("openclaw/nobody", assignment, sc); rej == nil || !strings.Contains(rej.Detail, "no longer has a journal policy") {
		t.Errorf("no policy: %+v", rej)
	}
	for _, w := range []string{journaltest.SASchoology, journaltest.SAAdmin, "openclaw/nobody"} {
		if rej := r.s.writerMayWrite(w, assignment, sc); rej == nil || rej.Status != 403 || rej.Reason != journalapi.ReasonPolicy {
			t.Errorf("%s: %+v", w, rej)
		}
	}
}

// Erase deletes the held snapshots of the subject: a later confirm finds
// nothing.
func TestHeldOfAnErasedSubjectIsGone(t *testing.T) {
	r := newRig(t)
	id := holdOne(r)
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child1"), nil); code != 200 {
		t.Fatal(code)
	}
	var resp journalapi.Response
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", nil, &resp); code != 404 || resp.Reason != journalapi.ReasonNotFound {
		t.Errorf("%d %+v", code, resp)
	}
}

// The snapshot id and the action are parsed before anything reaches the
// database: the auth rig answers 500 to a request that does.
func TestHeldPathIsParsedFirst(t *testing.T) {
	r := newAuthRig(t)
	for _, target := range []string{
		"not-a-uuid:confirm", uid(2) + ":approve", uid(2) + ":", uid(2), ":confirm", uid(2) + ":confirm:x",
		uid(2) + ":Confirm", "urn:uuid:" + uid(2) + ":confirm", "00000000-0000-4000-8000-00000000000:confirm",
	} {
		var resp journalapi.Response
		if code := r.do("POST", journalapi.PathAdminHeld+target, "admin", nil, &resp); code != 400 || resp.Reason != journalapi.ReasonBadRequest {
			t.Errorf("%q: %d %+v", target, code, resp)
		}
		if strings.Contains(resp.Detail, "not-a-uuid") || strings.Contains(resp.Detail, "approve") {
			t.Errorf("%q: echoed: %q", target, resp.Detail)
		}
	}
	// A well-formed request does reach the database (500 here): the parse
	// is not refusing everything.
	if code := r.do("POST", journalapi.HeldPath(uid(2), journalapi.ActionConfirm), "admin", nil, nil); code != 500 {
		t.Errorf("well-formed: %d", code)
	}
	if code := r.do("POST", journalapi.HeldPath(uid(2), journalapi.ActionDiscard), "admin", nil, nil); code != 500 {
		t.Errorf("well-formed discard: %d", code)
	}
}

func TestHeldTakesNoBody(t *testing.T) {
	r := newRig(t)
	id := holdOne(r)
	var resp journalapi.Response
	for _, body := range []string{`{"force": true}`, `[]`, `x`, `{} {}`} {
		if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", body, &resp); code != 400 || resp.Reason != journalapi.ReasonBadRequest || strings.Contains(resp.Detail, "force") {
			t.Errorf("%s: %d %+v", body, code, resp)
		}
	}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", strings.Repeat(" ", adminEnvelopeBytes+1), &resp); code != 413 || resp.Reason != journalapi.ReasonTooLarge {
		t.Errorf("oversized: %d %+v", code, resp)
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
		t.Fatalf("a refused request changed the snapshot: %s", st)
	}
	if code := r.do("POST", journalapi.HeldPath(id, journalapi.ActionDiscard), "admin", `{}`, &resp); code != 200 || resp.Status != journalapi.StatusDiscarded {
		t.Errorf("an empty object: %d %+v", code, resp)
	}
}

func TestEraseThenReplayAnswers410(t *testing.T) {
	r := newRig(t)
	seed(r)
	var er journalapi.EraseResponse
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child1"), &er); code != 200 || er.Current != 1 || er.Events != 1 || er.Snapshots != 1 || er.DataSubject != "e_child1" || er.ErasedAt.IsZero() {
		t.Fatalf("erase %d %+v", code, er)
	}
	var resp journalapi.Response
	code := r.do("POST", journalapi.PathRecords, "ps", assignments(9, "e_child1", "powerschool", T0.Add(-time.Hour)), &resp)
	if code != 410 || resp.Reason != journalapi.ReasonErasedSubject {
		t.Errorf("replay: %d %+v", code, resp)
	}
	var st journalapi.StateResponse
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child1", "gw", nil, &st); len(st.Rows) != 0 {
		t.Errorf("rows remain: %+v", st)
	}
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child2", "gw", nil, &st); len(st.Rows) != 1 {
		t.Errorf("another subject's rows went: %+v", st)
	}
	resp = journalapi.Response{}
	if code := r.do("POST", journalapi.PathAdminErase, "admin", `{"data_subject": "", "confirm": ""}`, &resp); code != 400 || resp.Reason != journalapi.ReasonBadRequest {
		t.Errorf("empty subject: %d %+v", code, resp)
	}
	if code := r.do("POST", journalapi.PathAdminErase, "gw", eraseBody("e_child2"), &resp); code != 403 || resp.Reason != journalapi.ReasonPolicy {
		t.Errorf("non-operator erase: %d", code)
	}
	if n := r.erasures(); n != 1 {
		t.Errorf("erasure rows: %d", n)
	}
	var actor string
	if err := r.db.QueryRow(`SELECT actor FROM erasure`).Scan(&actor); err != nil || actor != journaltest.SAAdmin {
		t.Errorf("actor %q %v", actor, err)
	}
	// A second erase is allowed (the ban stays) and answers 200.
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child1"), &er); code != 200 || er.Current != 0 {
		t.Errorf("second erase: %d %+v", code, er)
	}
	// A member not on the roster is erased too: the ban covers data that
	// has not arrived.
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_gone"), &er); code != 200 || er.DataSubject != "e_gone" {
		t.Errorf("off the roster: %d %+v", code, er)
	}
}

// Erase is irreversible: the body must repeat the member id in confirm,
// exactly. Anything else erases nothing.
func TestEraseNeedsConfirmation(t *testing.T) {
	r := newRig(t)
	seed(r)
	for name, body := range map[string]string{
		"no confirm":      `{"data_subject": "e_child1"}`,
		"empty confirm":   `{"data_subject": "e_child1", "confirm": ""}`,
		"other member":    `{"data_subject": "e_child1", "confirm": "e_child2"}`,
		"case":            `{"data_subject": "e_child1", "confirm": "E_child1"}`,
		"space":           `{"data_subject": "e_child1", "confirm": "e_child1 "}`,
		"prefix":          `{"data_subject": "e_child1", "confirm": "e_child"}`,
		"confirm is true": `{"data_subject": "e_child1", "confirm": true}`,
		"confirm only":    `{"confirm": "e_child1"}`,
	} {
		var resp journalapi.Response
		if code := r.do("POST", journalapi.PathAdminErase, "admin", body, &resp); code != 400 || resp.Reason != journalapi.ReasonBadRequest {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
	if n := r.erasures(); n != 0 {
		t.Fatalf("an unconfirmed erase erased: %d", n)
	}
	var st journalapi.StateResponse
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child1", "gw", nil, &st); len(st.Rows) != 1 {
		t.Errorf("rows went: %+v", st)
	}
}

// The member id follows roster.ValidID; anything else (the household "",
// a kept note's pseudonym, a path) is refused before the database.
func TestEraseRefusesNonMemberIDs(t *testing.T) {
	r := newAuthRig(t)
	for _, id := range []string{"", "erased:0123456789abcdef0123456789abcdef:1", "e child", "a/b", ".", "..", "e_child1\n", "\u00e9"} {
		var resp journalapi.Response
		if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody(id), &resp); code != 400 || resp.Reason != journalapi.ReasonBadRequest {
			t.Errorf("%q: %d %+v", id, code, resp)
		}
	}
	// The id is checked before the confirmation, whatever confirm says.
	var resp journalapi.Response
	if code := r.do("POST", journalapi.PathAdminErase, "admin", `{"data_subject": "a/b", "confirm": "x"}`, &resp); code != 400 || resp.Detail != "data_subject is not a member id" {
		t.Errorf("unconfirmed bad id: %d %+v", code, resp)
	}
	resp = journalapi.Response{}
	if code := r.do("POST", journalapi.PathAdminErase, "admin", ` `, &resp); code != 400 || !strings.Contains(resp.Detail, `"confirm"`) {
		t.Errorf("no body: %d %+v", code, resp)
	}
	// A valid, confirmed id does reach the database (500 on this rig).
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child1"), nil); code != 500 {
		t.Errorf("valid id: %d", code)
	}
}

func TestEraseWithoutARosterIs503(t *testing.T) {
	r := newRig(t)
	seed(r)
	r.ros.Store(nil)
	var resp journalapi.Response
	if code := r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child1"), &resp); code != 503 || resp.Reason != journalapi.ReasonUnavailable {
		t.Errorf("%d %+v", code, resp)
	}
	if n := r.erasures(); n != 0 {
		t.Errorf("erased without a roster: %d", n)
	}
}

func TestEraseErrMapping(t *testing.T) {
	r := newAuthRig(t)
	for name, c := range map[string]struct {
		err    error
		status int
		reason string
	}{
		"no subject":    {fmt.Errorf("x: %w", journal.ErrNoSubject), 400, journalapi.ReasonBadRequest},
		"no roster":     {journal.ErrNoRoster, 503, journalapi.ReasonUnavailable},
		"anything else": {errors.New("pq: secret detail"), 500, journalapi.ReasonInternal},
		"cancelled":     {context.Canceled, 500, journalapi.ReasonInternal},
	} {
		rec := httptest.NewRecorder()
		r.s.eraseErr(rec, c.err)
		resp := decodeRefusal(t, rec)
		if rec.Code != c.status || resp.Reason != c.reason || resp.Status != journalapi.StatusRejected {
			t.Errorf("%s: %d %+v", name, rec.Code, resp)
		}
		if strings.Contains(resp.Detail, "secret") {
			t.Errorf("%s: the error leaked: %q", name, resp.Detail)
		}
	}
}

// Only a service with admin: true reaches an admin endpoint; every other
// service with a policy is 403 policy and nothing happens; a service with
// no policy is 403 forbidden; no token is 401.
func TestAdminEndpointsAreOperatorOnly(t *testing.T) {
	r := newRig(t)
	seed(r)
	id := holdOne(r)
	type req struct{ path, body string }
	reqs := []req{
		{journalapi.HeldPath(id, journalapi.ActionConfirm), ""},
		{journalapi.HeldPath(id, journalapi.ActionDiscard), ""},
		{journalapi.PathAdminErase, `{"data_subject": "e_child2", "confirm": "e_child2"}`},
		{journalapi.PathAdminRescan, `{"all": true}`},
	}
	send := func(tok string, q req) (int, journalapi.Response) {
		var resp journalapi.Response
		var body any
		if q.body != "" {
			body = q.body
		}
		code := r.do("POST", q.path, tok, body, &resp)
		return code, resp
	}
	for tok, sa := range tokens {
		svc, ok := testServices()[sa]
		if ok && svc.Admin {
			continue
		}
		for _, q := range reqs {
			code, resp := send(tok, q)
			want, reason := 403, journalapi.ReasonPolicy
			if !ok {
				reason = journalapi.ReasonForbidden
			}
			if code != want || resp.Reason != reason {
				t.Errorf("%s %s: %d %+v", tok, q.path, code, resp)
			}
		}
	}
	for _, q := range reqs {
		if code, _ := send("", q); code != 401 {
			t.Errorf("no token %s: %d", q.path, code)
		}
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
		t.Errorf("held snapshot changed: %s", st)
	}
	if n := r.erasures(); n != 0 {
		t.Errorf("erased: %d", n)
	}
	if r.s.rescanning.Load() || strings.Contains(r.logs.String(), "journal rescan started") {
		t.Errorf("a rescan started")
	}
	// The policy decides, not the service's name: the gateway with
	// admin: true is an operator, and the admin service without it is not.
	services := testServices()
	gw := services[journaltest.SAGateway]
	gw.Admin = true
	services[journaltest.SAGateway] = gw
	services[journaltest.SAAdmin] = policy.Service{Read: true, Readers: policy.Readers{IDs: []string{"e_parent1"}}}
	pol, err := policy.New(services, journaltest.Registry(t))
	if err != nil {
		t.Fatal(err)
	}
	r.s.d.Policy = pol
	for _, q := range reqs[2:] {
		if code, resp := send("admin", q); code != 403 || resp.Reason != journalapi.ReasonPolicy {
			t.Errorf("admin without admin: true, %s: %d %+v", q.path, code, resp)
		}
	}
	if code, resp := send("gw", reqs[0]); code != 200 || resp.Status != journalapi.StatusApplied {
		t.Errorf("gateway with admin: true confirms: %d %+v", code, resp)
	}
	if code, _ := send("gw", reqs[2]); code != 200 {
		t.Errorf("gateway with admin: true erases: %d", code)
	}
	if code, resp := send("gw", reqs[3]); code != 202 || resp.Status != journalapi.StatusStarted {
		t.Errorf("gateway with admin: true rescans: %d %+v", code, resp)
	}
	r.waitRescan()
}

// Every admin POST refuses a query string (4.9), before the body is read
// and before the database.
func TestAdminPostsRefuseQueryStrings(t *testing.T) {
	r := newRig(t)
	seed(r)
	id := holdOne(r)
	for _, q := range []string{"?x=1", "?data_subject=e_child1", "?all=true", "?reader=e_parent1", "?=", "?a"} {
		for _, c := range []struct{ path, body string }{
			{journalapi.HeldPath(id, journalapi.ActionConfirm), ""},
			{journalapi.PathAdminErase, `{"data_subject": "e_child1", "confirm": "e_child1"}`},
			{journalapi.PathAdminRescan, ""},
		} {
			var resp journalapi.Response
			var body any
			if c.body != "" {
				body = c.body
			}
			if code := r.do("POST", c.path+q, "admin", body, &resp); code != 400 || resp.Reason != journalapi.ReasonBadRequest || strings.Contains(resp.Detail, "data_subject") || strings.Contains(resp.Detail, "reader") {
				t.Errorf("%s%s: %d %+v", c.path, q, code, resp)
			}
		}
	}
	// Even with a body over the cap, the query string is refused first.
	if code := r.do("POST", journalapi.PathAdminErase+"?x=1", "admin", strings.Repeat(" ", 1<<20), nil); code != 400 {
		t.Errorf("oversized with a query: %d", code)
	}
	if st := r.snapshotStatus(id); st != journalapi.StatusHeld {
		t.Errorf("held snapshot changed: %s", st)
	}
	if n := r.erasures(); n != 0 {
		t.Errorf("erased: %d", n)
	}
	if r.s.rescanning.Load() || strings.Contains(r.logs.String(), "journal rescan started") {
		t.Errorf("a rescan started")
	}
}

// The admin body caps are derived from the request types: every key a
// request has plus a small margin, and every string at the widest scope
// value with every byte escaped.
func TestAdminBodyCapsAreDerived(t *testing.T) {
	erase := adminBodyCap(journalapi.EraseRequest{})
	wantErase := int64(adminEnvelopeBytes +
		escapedBytes*len("data_subject") + 4 + escapedBytes*record.MaxScopeValueBytes + 2 +
		escapedBytes*len("confirm") + 4 + escapedBytes*record.MaxScopeValueBytes + 2)
	if erase.bytes != wantErase || erase.keys != (strictjson.Limits{MaxObjectKeys: 2 + adminKeyMargin, MaxTotalKeys: 2 + adminKeyMargin}) {
		t.Errorf("erase: %+v, want %d bytes", erase, wantErase)
	}
	rescan := adminBodyCap(journalapi.RescanRequest{})
	if want := int64(adminEnvelopeBytes + escapedBytes*len("all") + 4 + len("false")); rescan.bytes != want || rescan.keys != (strictjson.Limits{MaxObjectKeys: 1 + adminKeyMargin, MaxTotalKeys: 1 + adminKeyMargin}) {
		t.Errorf("rescan: %+v, want %d bytes", rescan, want)
	}
	held := adminBodyCap(struct{}{})
	if held.bytes != adminEnvelopeBytes || held.keys != (strictjson.Limits{MaxObjectKeys: adminKeyMargin, MaxTotalKeys: adminKeyMargin}) {
		t.Errorf("held: %+v", held)
	}
	if erase.bytes > 4<<10 {
		t.Errorf("the erase cap is not small: %d", erase.bytes)
	}
	defer func() {
		if recover() == nil {
			t.Errorf("a field type with no width was capped")
		}
	}()
	adminBodyCap(struct {
		N int `json:"n"`
	}{})
}

func escapeAll(s string) string {
	var b strings.Builder
	for _, c := range s {
		fmt.Fprintf(&b, `\u%04x`, c)
	}
	return b.String()
}

func TestEraseBodyCaps(t *testing.T) {
	r := newRig(t)
	seed(r)
	// The widest id the journal can hold (a scope value), every byte
	// escaped, in both fields: it fits.
	id := "e_" + strings.Repeat("x", record.MaxScopeValueBytes-2)
	widest := `{"data_subject": "` + escapeAll(id) + `", "confirm": "` + escapeAll(id) + `"}`
	var er journalapi.EraseResponse
	if code := r.do("POST", journalapi.PathAdminErase, "admin", widest, &er); code != 200 || er.DataSubject != id {
		t.Errorf("widest: %d", code)
	}
	cap := adminBodyCap(journalapi.EraseRequest{}).bytes
	var resp journalapi.Response
	over := `{"data_subject": "e_child1", "confirm": "e_child1"}` + strings.Repeat(" ", int(cap))
	if code := r.do("POST", journalapi.PathAdminErase, "admin", over, &resp); code != 413 || resp.Reason != journalapi.ReasonTooLarge {
		t.Errorf("over the byte cap: %d %+v", code, resp)
	}
	at := `{"data_subject": "e_child1", "confirm": "e_child1"}`
	at += strings.Repeat(" ", int(cap)-len(at))
	for name, c := range map[string]struct {
		body   string
		code   int
		reason string
	}{
		"too many keys":  {`{"data_subject": "e_child1", "confirm": "e_child1", "a": 1, "b": 2, "c": 3}`, 413, journalapi.ReasonTooLarge},
		"unknown key":    {`{"data_subject": "e_child1", "confirm": "e_child1", "SECRETKEY": 1}`, 400, journalapi.ReasonBadRequest},
		"duplicate key":  {`{"data_subject": "e_child2", "data_subject": "e_child1", "confirm": "e_child1"}`, 400, journalapi.ReasonBadRequest},
		"case variant":   {`{"Data_Subject": "e_child1", "confirm": "e_child1"}`, 400, journalapi.ReasonBadRequest},
		"trailing data":  {`{"data_subject": "e_child1", "confirm": "e_child1"} {}`, 400, journalapi.ReasonBadRequest},
		"not an object":  {`["e_child1"]`, 400, journalapi.ReasonBadRequest},
		"null":           {`null`, 400, journalapi.ReasonBadRequest},
		"nested":         {`{"data_subject": {"id": "e_child1"}, "confirm": "e_child1"}`, 400, journalapi.ReasonBadRequest},
		"empty":          {` `, 400, journalapi.ReasonBadRequest},
		"at the cap, ok": {at, 200, ""},
	} {
		resp = journalapi.Response{}
		code := r.do("POST", journalapi.PathAdminErase, "admin", c.body, &resp)
		if code != c.code || resp.Reason != c.reason {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
		if strings.Contains(resp.Detail, "SECRETKEY") {
			t.Errorf("%s: echoed a key", name)
		}
	}
	if code, _ := r.send("POST", journalapi.PathAdminErase, "admin", "text/plain", `{"data_subject": "e_child2", "confirm": "e_child2"}`); code != 400 {
		t.Errorf("text/plain: %d", code)
	}
	if code, _ := r.send("POST", journalapi.PathAdminErase, "admin", "", `{"data_subject": "e_child2", "confirm": "e_child2"}`); code != 400 {
		t.Errorf("no Content-Type: %d", code)
	}
	var st journalapi.StateResponse
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child2", "gw", nil, &st); len(st.Rows) != 1 {
		t.Errorf("a refused erase erased e_child2")
	}
}

func TestRescanBody(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		body   string
		code   int
		reason string
	}{
		"unknown key":   {`{"all": true, "SECRETKEY": 1}`, 400, journalapi.ReasonBadRequest},
		"not a bool":    {`{"all": "yes"}`, 400, journalapi.ReasonBadRequest},
		"case variant":  {`{"ALL": true}`, 400, journalapi.ReasonBadRequest},
		"too many keys": {`{"all": true, "a": 1, "b": 2, "c": 3}`, 413, journalapi.ReasonTooLarge},
		"over the cap":  {`{"all": true}` + strings.Repeat(" ", int(adminBodyCap(journalapi.RescanRequest{}).bytes)), 413, journalapi.ReasonTooLarge},
	} {
		var resp journalapi.Response
		if code := r.do("POST", journalapi.PathAdminRescan, "admin", c.body, &resp); code != c.code || resp.Reason != c.reason || strings.Contains(resp.Detail, "SECRETKEY") {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
	if code, _ := r.send("POST", journalapi.PathAdminRescan, "admin", "text/plain", `{"all": true}`); code != 400 {
		t.Errorf("text/plain: %d", code)
	}
	if r.s.rescanning.Load() || strings.Contains(r.logs.String(), "journal rescan started") {
		t.Errorf("a refused rescan started")
	}
	for _, body := range []any{nil, `{}`, `{"all": false}`, `{"all": true}`} {
		var resp journalapi.Response
		if code := r.do("POST", journalapi.PathAdminRescan, "admin", body, &resp); code != 202 || resp.Status != journalapi.StatusStarted {
			t.Errorf("%v: %d %+v", body, code, resp)
		}
		r.waitRescan()
	}
	if got := strings.Count(r.logs.String(), "journal rescan finished"); got != 4 {
		t.Errorf("finished %d rescans", got)
	}
	if !strings.Contains(r.logs.String(), "all=true") || !strings.Contains(r.logs.String(), "all=false") {
		t.Errorf("all not passed through:\n%s", r.logs.String())
	}
}

// Refusal details echo nothing the caller sent.
func TestAdminRefusalsDoNotEchoValues(t *testing.T) {
	r := newAuthRig(t)
	for _, body := range []string{
		`{"data_subject": "SECRETNAME", "confirm": "SECRETOTHER"}`,
		`{"data_subject": "SECRET NAME", "confirm": "SECRET NAME"}`,
		`{"SECRETKEY": "SECRETNAME"}`,
	} {
		code, raw := r.send("POST", journalapi.PathAdminErase, "admin", "application/json", body)
		if code != 400 || strings.Contains(string(raw), "SECRET") {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
	code, raw := r.send("POST", journalapi.PathAdminHeld+"SECRETID:SECRETACTION", "admin", "", nil)
	if code != 400 || strings.Contains(string(raw), "SECRET") {
		t.Errorf("held: %d %s", code, raw)
	}
	code, raw = r.send("POST", journalapi.PathAdminRescan+"?SECRETKEY=SECRETNAME", "admin", "", nil)
	if code != 400 || strings.Contains(string(raw), "SECRET") {
		t.Errorf("rescan: %d %s", code, raw)
	}
	// The audit log names a well-formed member id an operator sent
	// (SECRETNAME here); nothing else the caller chose reaches it.
	for _, v := range []string{"SECRETOTHER", "SECRET NAME", "SECRETKEY", "SECRETID", "SECRETACTION"} {
		if strings.Contains(r.logs.String(), v) {
			t.Errorf("%s reached the log:\n%s", v, r.logs.String())
		}
	}
}

var auditLine = regexp.MustCompile(`(?m)^.*msg="journal admin".*$`)

// Every request past the operator check logs one audit line with exactly
// the caller, the route name, the request's subject (an erase's member id,
// a held snapshot's id and action, a rescan's all) and the answer's status.
// A refused non-operator logs a warning with caller and route only.
func TestAdminAuditLog(t *testing.T) {
	r := newRig(t)
	seed(r)
	id := holdOne(r)
	r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child2"), nil)
	r.do("POST", journalapi.PathAdminErase, "admin", `{"data_subject": "e_child1", "confirm": "e_child2"}`, nil)
	r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("not ok"), nil)
	r.do("POST", journalapi.HeldPath(id, journalapi.ActionDiscard), "admin", nil, nil)
	r.do("POST", journalapi.PathAdminHeld+"nope:confirm", "admin", nil, nil)
	r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, nil)
	r.waitRescan()
	r.do("POST", journalapi.PathAdminErase, "gw", eraseBody("e_child1"), nil)
	lines := auditLine.FindAllString(r.logs.String(), -1)
	want := []string{
		`caller=` + journaltest.SAAdmin + ` endpoint=admin_erase data_subject=e_child2 code=200`,
		`caller=` + journaltest.SAAdmin + ` endpoint=admin_erase data_subject=e_child1 code=400`,
		`caller=` + journaltest.SAAdmin + ` endpoint=admin_erase data_subject="(not a member id)" code=400`,
		`caller=` + journaltest.SAAdmin + ` endpoint=admin_held snapshot_id=` + id + ` action=discard code=200`,
		`caller=` + journaltest.SAAdmin + ` endpoint=admin_held snapshot_id="(not a snapshot id)" action="(not an action)" code=400`,
		`caller=` + journaltest.SAAdmin + ` endpoint=admin_rescan all=true code=202`,
	}
	if len(lines) != len(want) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for i, w := range want {
		if !strings.HasSuffix(lines[i], `level=INFO msg="journal admin" `+w) {
			t.Errorf("line %d: %s\nwant suffix %s", i, lines[i], w)
		}
	}
	logs := r.logs.String()
	if !regexp.MustCompile(`level=WARN msg="journal admin refused" caller=` + journaltest.SAGateway + ` endpoint=admin_erase\n`).MatchString(logs) {
		t.Errorf("no refusal warning:\n%s", logs)
	}
	if !regexp.MustCompile(`msg="journal rescan started" caller=`+journaltest.SAAdmin+` all=true\n`).MatchString(logs) ||
		!regexp.MustCompile(`msg="journal rescan finished" caller=`+journaltest.SAAdmin+` current_rows=\d+ event_rows=\d+ released=\d+ withheld=\d+\n`).MatchString(logs) {
		t.Errorf("rescan lines:\n%s", logs)
	}
	if strings.Contains(logs, `"title"`) || strings.Contains(logs, "not ok") {
		t.Errorf("a value reached the log:\n%s", logs)
	}
}

func TestRescanRunsOnceAtATime(t *testing.T) {
	r := newRig(t)
	seed(r)
	r.do("POST", journalapi.PathRecords, "ps", assignments(5, "e_child1", "powerschool", T0.Add(-time.Minute), one("k1", "e_child1", `{"title": "A", "comment": "fine"}`)), nil)
	var resp journalapi.Response
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, &resp); code != 202 || resp.Status != journalapi.StatusStarted {
		t.Fatalf("%d %+v", code, resp)
	}
	r.waitRescan()
	entered, release := r.scan.hold()
	defer release()
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, &resp); code != 202 {
		t.Fatalf("%d %+v", code, resp)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the rescan never scanned")
	}
	// While it runs, every other rescan is 409, concurrent ones included.
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp journalapi.Response
			codes[i] = r.do("POST", journalapi.PathAdminRescan, "admin", nil, &resp)
			if resp.Reason != journalapi.ReasonConflict {
				codes[i] = -codes[i]
			}
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 409 {
			t.Errorf("concurrent rescan %d: %d", i, c)
		}
	}
	release()
	r.waitRescan()
	if got := strings.Count(r.logs.String(), "journal rescan started"); got != 2 {
		t.Errorf("%d rescans started, want 2", got)
	}
	if got := strings.Count(r.logs.String(), "journal rescan finished"); got != 2 {
		t.Errorf("%d rescans finished, want 2:\n%s", got, r.logs.String())
	}
	// Free again.
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", nil, &resp); code != 202 {
		t.Errorf("after: %d", code)
	}
	r.waitRescan()
	r.s.rescanning.Store(true) // as if one were running
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", nil, &resp); code != 409 || resp.Reason != journalapi.ReasonConflict {
		t.Errorf("second rescan: %d", code)
	}
	r.s.rescanning.Store(false)
}

// The rescan runs in the background under Deps.Context: neither the
// request's timeout nor its end cancels it, it holds no in-flight slot
// once answered, and cancelling Deps.Context stops it and frees the guard.
func TestRescanIsDetachedFromTheRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRig(t, func(d *Deps) {
		d.RequestTimeout = 100 * time.Millisecond
		d.MaxInFlight = 1
		d.Context = ctx
	})
	r.do("POST", journalapi.PathRecords, "ps", assignments(5, "e_child1", "powerschool", T0.Add(-time.Minute), one("k1", "e_child1", `{"title": "A", "comment": "fine"}`)), nil)
	calls := r.scan.Calls()
	entered, release := r.scan.hold()
	defer release()
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, nil); code != 202 {
		t.Fatal(code)
	}
	<-entered
	// Past the request timeout, with the request long answered.
	time.Sleep(300 * time.Millisecond)
	// The one in-flight slot is free: the background rescan does not hold it.
	if code := r.do("GET", journalapi.PathHealthz, "gw", nil, nil); code != 200 {
		t.Errorf("healthz while a rescan runs: %d", code)
	}
	release()
	r.waitRescan()
	logs := r.logs.String()
	if !strings.Contains(logs, "journal rescan finished") || strings.Contains(logs, "journal rescan failed") {
		t.Fatalf("the rescan did not finish:\n%s", logs)
	}
	if r.scan.Calls() <= calls {
		t.Errorf("nothing was scanned")
	}

	// Deps.Context ends a running rescan, and the guard is free after.
	entered, release2 := r.scan.hold()
	defer release2()
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, nil); code != 202 {
		t.Fatal(code)
	}
	<-entered
	cancel()
	release2()
	r.waitRescan()
	if !strings.Contains(r.logs.String(), "journal rescan failed") {
		t.Errorf("no failure logged:\n%s", r.logs.String())
	}
}

// api_requests_total names the admin routes, never a path or an id.
func TestAdminRequestsAreCountedByRouteName(t *testing.T) {
	r := newRig(t)
	seed(r)
	id := holdOne(r)
	r.do("POST", journalapi.HeldPath(id, journalapi.ActionConfirm), "admin", nil, nil)
	r.do("POST", journalapi.HeldPath(uid(88), journalapi.ActionConfirm), "admin", nil, nil)
	r.do("POST", journalapi.PathAdminErase, "admin", eraseBody("e_child2"), nil)
	r.do("POST", journalapi.PathAdminErase, "gw", eraseBody("e_child2"), nil)
	r.do("POST", journalapi.PathAdminRescan, "admin", nil, nil)
	r.waitRescan()
	m := r.scrape()
	for _, want := range []string{`code="200",endpoint="admin_held"`, `code="404",endpoint="admin_held"`,
		`code="200",endpoint="admin_erase"`, `code="403",endpoint="admin_erase"`, `code="202",endpoint="admin_rescan"`} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(m, id) || strings.Contains(m, uid(88)) || strings.Contains(m, "e_child2") {
		t.Errorf("an id reached a label")
	}
}

// Without all, a rescan skips values the current scanner version judged;
// with all, or after a scanner upgrade, it judges them again.
func TestRescanAllOrOnlyOtherVersions(t *testing.T) {
	r := newRig(t)
	r.do("POST", journalapi.PathRecords, "ps", assignments(5, "e_child1", "powerschool", T0.Add(-time.Minute), one("k1", "e_child1", `{"title": "A", "comment": "fine"}`)), nil)
	calls := r.scan.Calls()
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", nil, nil); code != 202 {
		t.Fatal(code)
	}
	r.waitRescan()
	if got := r.scan.Calls(); got != calls {
		t.Errorf("a rescan without all scanned %d values judged by this version", got-calls)
	}
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, nil); code != 202 {
		t.Fatal(code)
	}
	r.waitRescan()
	if got := r.scan.Calls(); got <= calls {
		t.Errorf("a rescan with all scanned nothing")
	}
	calls = r.scan.Calls()
	r.scan.SetVersion("scanner-2")
	if code := r.do("POST", journalapi.PathAdminRescan, "admin", nil, nil); code != 202 {
		t.Fatal(code)
	}
	r.waitRescan()
	if got := r.scan.Calls(); got <= calls {
		t.Errorf("a rescan after an upgrade scanned nothing")
	}
}

// Many rescans asked for at once from idle: exactly one starts.
func TestConcurrentRescansStartOnce(t *testing.T) {
	r := newRig(t)
	r.do("POST", journalapi.PathRecords, "ps", assignments(5, "e_child1", "powerschool", T0.Add(-time.Minute), one("k1", "e_child1", `{"title": "A", "comment": "fine"}`)), nil)
	for round := 0; round < 5; round++ {
		entered, release := r.scan.hold()
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, DefaultMaxInFlight)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes[i] = r.do("POST", journalapi.PathAdminRescan, "admin", `{"all": true}`, nil)
			}()
		}
		close(start)
		wg.Wait()
		started := 0
		for _, c := range codes {
			switch c {
			case 202:
				started++
			case 409:
			default:
				t.Errorf("round %d: %d", round, c)
			}
		}
		if started != 1 {
			t.Errorf("round %d: %d rescans started", round, started)
		}
		if started > 0 {
			<-entered
		}
		release()
		r.waitRescan()
	}
}
