package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func seed(r *rig) {
	r.do("POST", journalapi.PathRecords, "ps", assignments(1, "e_child1", "powerschool", T0.Add(-time.Hour), one("k1", "e_child1", `{"title": "A"}`)), nil)
	r.do("POST", journalapi.PathRecords, "ps", assignments(2, "e_child2", "powerschool", T0.Add(-time.Hour), one("k2", "e_child2", `{"title": "B"}`)), nil)
}

func TestReadsAsAReader(t *testing.T) {
	r := newRig(t)
	seed(r)
	var st journalapi.StateResponse
	if code := r.do("GET", journalapi.PathState+"?reader=e_child1", "gw", nil, &st); code != 200 || len(st.Rows) != 1 || st.Rows[0].DataSubject != "e_child1" {
		t.Fatalf("%d %+v", code, st)
	}
	if st.RosterSHA256 == "" || st.RosterSHA256 != r.ros.Load().SHA256() {
		t.Errorf("roster hash %q", st.RosterSHA256)
	}
	if code := r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child2", "dash", nil, &st); code != 200 || len(st.Rows) != 1 {
		t.Errorf("dashboard as a guardian: %d %+v", code, st)
	}
	var ev journalapi.EventsResponse
	if code := r.do("GET", journalapi.PathEvents+"?reader=e_parent1&after=0&limit=1", "gw", nil, &ev); code != 200 || len(ev.Events) != 1 || ev.Next != ev.Events[0].Seq {
		t.Errorf("events: %d %+v", code, ev)
	}
	next := ev.Next
	if r.do("GET", journalapi.PathEvents+"?reader=e_parent1&after="+itoa(next), "gw", nil, &ev); len(ev.Events) != 1 || ev.Events[0].Seq <= next {
		t.Errorf("cursor: %+v", ev)
	}
	if r.do("GET", journalapi.PathEvents+"?reader=e_parent1&after=999999", "gw", nil, &ev); len(ev.Events) != 0 || ev.Next != 999999 {
		t.Errorf("an empty page keeps the cursor: %+v", ev)
	}
	var counts journalapi.CountsResponse
	if code := r.do("GET", journalapi.PathCounts+"?reader=e_parent1", "dash", nil, &counts); code != 200 || len(counts.Counts) != 2 {
		t.Errorf("counts: %d %+v", code, counts)
	}
	var h journalapi.Healthz
	if code := r.do("GET", journalapi.PathHealthz, "ps", nil, &h); code != 200 || h.RosterSHA256 == "" || h.SchemaVersion < 1 || h.Status != journalapi.HealthOK {
		t.Errorf("healthz: %d %+v", code, h)
	}
	if h.RosterSHA256 != r.ros.Load().SHA256() {
		t.Errorf("healthz roster hash %q", h.RosterSHA256)
	}
}

func TestReadRefusals(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		token, path string
		status      int
		reason      string
	}{
		"no reader":              {"gw", journalapi.PathState, 400, journalapi.ReasonBadRequest},
		"empty reader":           {"gw", journalapi.PathEvents + "?reader=", 400, journalapi.ReasonBadRequest},
		"grants in the query":    {"gw", journalapi.PathState + "?reader=e_child1&grants=e_child2:guardians", 400, journalapi.ReasonBadRequest},
		"audience in the query":  {"gw", journalapi.PathEvents + "?reader=e_child1&audience=public", 400, journalapi.ReasonBadRequest},
		"two readers":            {"gw", journalapi.PathState + "?reader=e_child1&reader=e_parent1", 400, journalapi.ReasonBadRequest},
		"reader not in roster":   {"gw", journalapi.PathState + "?reader=e_nobody", 403, journalapi.ReasonPolicy},
		"reader outside readers": {"dash", journalapi.PathState + "?reader=e_child1", 403, journalapi.ReasonPolicy},
		"writer reads":           {"ps", journalapi.PathState + "?reader=e_parent1", 403, journalapi.ReasonPolicy},
		"counts not allowed":     {"ps", journalapi.PathCounts + "?reader=e_parent1", 403, journalapi.ReasonPolicy},
		"bad min_importance":     {"gw", journalapi.PathEvents + "?reader=e_parent1&min_importance=urgent", 400, journalapi.ReasonBadRequest},
		"bad limit":              {"gw", journalapi.PathEvents + "?reader=e_parent1&limit=0", 400, journalapi.ReasonBadRequest},
		"limit over the cap":     {"gw", journalapi.PathEvents + "?reader=e_parent1&limit=1001", 400, journalapi.ReasonBadRequest},
		"limit not a number":     {"gw", journalapi.PathEvents + "?reader=e_parent1&limit=ten", 400, journalapi.ReasonBadRequest},
		"bad after":              {"gw", journalapi.PathEvents + "?reader=e_parent1&after=-1", 400, journalapi.ReasonBadRequest},
		"after not a number":     {"gw", journalapi.PathEvents + "?reader=e_parent1&after=x", 400, journalapi.ReasonBadRequest},
		"bad include_removed":    {"gw", journalapi.PathState + "?reader=e_parent1&include_removed=maybe", 400, journalapi.ReasonBadRequest},
		"bad since":              {"gw", journalapi.PathCounts + "?reader=e_parent1&since=yesterday", 400, journalapi.ReasonBadRequest},
		"unknown counts param":   {"gw", journalapi.PathCounts + "?reader=e_parent1&limit=5", 400, journalapi.ReasonBadRequest},
		"unknown state param":    {"gw", journalapi.PathState + "?reader=e_parent1&after=5", 400, journalapi.ReasonBadRequest},
		"healthz with a query":   {"gw", journalapi.PathHealthz + "?reader=e_parent1", 400, journalapi.ReasonBadRequest},
		"malformed query":        {"gw", journalapi.PathState + "?reader=%zz", 400, journalapi.ReasonBadRequest},
	} {
		var resp journalapi.Response
		if code := r.do("GET", c.path, c.token, nil, &resp); code != c.status || resp.Reason != c.reason {
			t.Errorf("%s: %d %+v", name, code, resp)
		}
	}
	// The handler checks after and limit itself (the journal's ErrBadQuery
	// is only a backstop, with a generic detail).
	for path, detail := range map[string]string{
		journalapi.PathEvents + "?reader=e_parent1&after=-1":  "after is a sequence number >= 0",
		journalapi.PathEvents + "?reader=e_parent1&limit=0":   "limit is 1 to 1000",
		journalapi.PathEvents + "?reader=e_parent1&limit=-5":  "limit is 1 to 1000",
		journalapi.PathEvents + "?reader=e_parent1&after=-10": "after is a sequence number >= 0",
	} {
		var resp journalapi.Response
		if code := r.do("GET", path, "gw", nil, &resp); code != 400 || resp.Detail != detail {
			t.Errorf("%s: %d %+v", path, code, resp)
		}
	}
	// The limit cap itself is admitted.
	var ev journalapi.EventsResponse
	if code := r.do("GET", journalapi.PathEvents+"?reader=e_parent1&limit=1000&min_importance=high", "gw", nil, &ev); code != 200 {
		t.Errorf("limit 1000: %d", code)
	}
}

// Which readers a service may present is its policy's readers (4.9), and
// whether it may read at all (read) or only count (counts). Both ways, for
// every read endpoint.
func TestReaderPresentationFollowsPolicy(t *testing.T) {
	r := newRig(t)
	seed(r)
	cases := []struct {
		token, path string
		status      int
	}{
		// readers "*": the gateway may present any roster member...
		{"gw", journalapi.PathState + "?reader=e_child1", 200},
		{"gw", journalapi.PathState + "?reader=e_child2", 200},
		{"gw", journalapi.PathEvents + "?reader=e_guest1", 200},
		{"gw", journalapi.PathCounts + "?reader=e_child2", 200},
		// ...but no one outside the roster.
		{"gw", journalapi.PathEvents + "?reader=e_nobody", 403},
		{"gw", journalapi.PathCounts + "?reader=e_nobody", 403},
		// readers [e_parent1, e_parent2]: the dashboard reads only as them.
		{"dash", journalapi.PathState + "?reader=e_parent2", 200},
		{"dash", journalapi.PathEvents + "?reader=e_parent1", 200},
		{"dash", journalapi.PathCounts + "?reader=e_parent2", 200},
		{"dash", journalapi.PathState + "?reader=e_child1", 403},
		{"dash", journalapi.PathEvents + "?reader=e_child2", 403},
		{"dash", journalapi.PathCounts + "?reader=e_child1", 403},
		{"dash", journalapi.PathState + "?reader=e_guest1", 403},
		// read without counts.
		{"ro", journalapi.PathState + "?reader=e_parent1", 200},
		{"ro", journalapi.PathEvents + "?reader=e_parent1", 200},
		{"ro", journalapi.PathCounts + "?reader=e_parent1", 403},
		{"ro", journalapi.PathState + "?reader=e_parent2", 403},
		{"ro", journalapi.PathEvents + "?reader=e_parent2", 403},
		// counts without read.
		{"co", journalapi.PathCounts + "?reader=e_parent1", 200},
		{"co", journalapi.PathState + "?reader=e_parent1", 403},
		{"co", journalapi.PathEvents + "?reader=e_parent1", 403},
		{"co", journalapi.PathCounts + "?reader=e_parent2", 403},
		// no read rights at all.
		{"ps", journalapi.PathEvents + "?reader=e_parent1", 403},
		{"nw", journalapi.PathState + "?reader=e_parent2", 403},
		{"nw", journalapi.PathCounts + "?reader=e_parent2", 403},
		{"admin", journalapi.PathState + "?reader=e_parent1", 403},
	}
	for _, c := range cases {
		code, raw := r.send("GET", c.path, c.token, "", nil)
		if code != c.status {
			t.Errorf("%s %s: %d %s", c.token, c.path, code, raw)
		}
		if code == 403 {
			var rr journalapi.Response
			_ = json.Unmarshal(raw, &rr)
			if rr.Reason != journalapi.ReasonPolicy || strings.Contains(string(raw), "e_") {
				t.Errorf("%s %s: %s", c.token, c.path, raw)
			}
		}
	}
}

// The presented reader is the one whose view comes back, never a wider
// one: each child sees only its own rows, a guardian both, a guest none.
func TestReaderGetsOnlyItsView(t *testing.T) {
	r := newRig(t)
	seed(r)
	subjects := func(token, reader string) map[string]int {
		var st journalapi.StateResponse
		if code := r.do("GET", journalapi.PathState+"?reader="+reader, token, nil, &st); code != 200 {
			t.Fatalf("%s as %s: %d", token, reader, code)
		}
		out := map[string]int{}
		for _, row := range st.Rows {
			out[row.DataSubject]++
		}
		var ev journalapi.EventsResponse
		r.do("GET", journalapi.PathEvents+"?reader="+reader, token, nil, &ev)
		for _, e := range ev.Events {
			out["event:"+e.DataSubject]++
		}
		var cn journalapi.CountsResponse
		r.do("GET", journalapi.PathCounts+"?reader="+reader, token, nil, &cn)
		for _, c := range cn.Counts {
			out["count:"+c.DataSubject] += c.Live
		}
		return out
	}
	want := map[string]map[string]int{
		"e_child1":  {"e_child1": 1, "event:e_child1": 1, "count:e_child1": 1},
		"e_child2":  {"e_child2": 1, "event:e_child2": 1, "count:e_child2": 1},
		"e_parent1": {"e_child1": 1, "e_child2": 1, "event:e_child1": 1, "event:e_child2": 1, "count:e_child1": 1, "count:e_child2": 1},
		"e_guest1":  {},
	}
	for reader, w := range want {
		got := subjects("gw", reader)
		if len(got) != len(w) {
			t.Errorf("%s: %v", reader, got)
			continue
		}
		for k, n := range w {
			if got[k] != n {
				t.Errorf("%s: %v", reader, got)
			}
		}
	}
}

// What the journal withholds stays withheld: a guardians-only record is not
// in a child's view, and a text value that failed its scan is masked in
// state and events for everyone.
func TestWithheldStaysWithheld(t *testing.T) {
	r := newRig(t)
	snap := journalapi.Snapshot{SnapshotID: uid(9), RecordType: "term_comment", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "comments"}, {"data_subject", "e_child1"}},
		Complete: true, ObservedAt: T0.Add(-time.Hour),
		Records: []journalapi.Record{one("t1", "e_child1", `{"comment": "INJECT SECRETTEXT"}`)}}
	if code, resp := post(r, "ps", snap); code != 200 {
		t.Fatalf("%d %+v", code, resp)
	}
	code, raw := r.send("GET", journalapi.PathState+"?reader=e_child1", "gw", "", nil)
	if code != 200 || !strings.Contains(string(raw), `"rows":[]`) {
		t.Errorf("a child reads a guardians-only record: %d %s", code, raw)
	}
	for _, path := range []string{journalapi.PathState + "?reader=e_parent1", journalapi.PathEvents + "?reader=e_parent1"} {
		code, raw := r.send("GET", path, "gw", "", nil)
		if code != 200 || strings.Contains(string(raw), "SECRETTEXT") || !strings.Contains(string(raw), journalapi.WithheldPrefix) {
			t.Errorf("%s: %d %s", path, code, raw)
		}
	}
}

// Lists are [], never null, and Count.Events is {}.
func TestEmptyListsAreArrays(t *testing.T) {
	r := newRig(t)
	for path, want := range map[string]string{
		journalapi.PathState + "?reader=e_guest1":  `"rows":[]`,
		journalapi.PathEvents + "?reader=e_guest1": `"events":[]`,
		journalapi.PathCounts + "?reader=e_guest1": `"counts":[]`,
	} {
		code, raw := r.send("GET", path, "gw", "", nil)
		if code != 200 || !strings.Contains(string(raw), want) || strings.Contains(string(raw), "null") {
			t.Errorf("%s: %d %s", path, code, raw)
		}
	}
}

func TestNonNil(t *testing.T) {
	if got := nonNil[int](nil); got == nil || len(got) != 0 {
		t.Errorf("nil: %#v", got)
	}
	in := []int{1, 2}
	if got := nonNil(in); len(got) != 2 || &got[0] != &in[0] {
		t.Errorf("kept: %#v", got)
	}
	raw, _ := json.Marshal(journalapi.EventsResponse{Events: nonNil[journalapi.Event](nil)})
	if !strings.Contains(string(raw), `"events":[]`) {
		t.Errorf("%s", raw)
	}
}

// The cursor is the last returned seq, or the request's after for an empty
// page (it never goes back, and never jumps past what was not returned).
func TestNextCursor(t *testing.T) {
	if got := nextCursor(41, nil); got != 41 {
		t.Errorf("empty page: %d", got)
	}
	if got := nextCursor(41, []journalapi.Event{{Seq: 45}, {Seq: 50}}); got != 50 {
		t.Errorf("page: %d", got)
	}
	if got := nextCursor(0, []journalapi.Event{{Seq: 7}}); got != 7 {
		t.Errorf("one: %d", got)
	}
}

// Paging with the returned cursor visits every visible event once, in
// order, whatever the page size; filters do not stall it.
func TestEventsPaging(t *testing.T) {
	r := newRig(t)
	for i := 1; i <= 5; i++ {
		r.do("POST", journalapi.PathRecords, "ps", assignments(10+i, "e_child1", "powerschool", T0.Add(-time.Duration(10-i)*time.Minute),
			one("k1", "e_child1", `{"title": "v`+itoa(int64(i))+`"}`)), nil)
	}
	var all journalapi.EventsResponse
	r.do("GET", journalapi.PathEvents+"?reader=e_child1", "gw", nil, &all)
	if len(all.Events) != 5 || all.Next != all.Events[4].Seq {
		t.Fatalf("all: %+v", all)
	}
	var seen []int64
	cursor := int64(0)
	for i := 0; i < 10; i++ {
		var ev journalapi.EventsResponse
		if code := r.do("GET", journalapi.PathEvents+"?reader=e_child1&limit=2&after="+itoa(cursor), "gw", nil, &ev); code != 200 {
			t.Fatalf("page: %d", code)
		}
		if len(ev.Events) == 0 {
			if ev.Next != cursor {
				t.Errorf("an empty page moved the cursor %d -> %d", cursor, ev.Next)
			}
			break
		}
		for _, e := range ev.Events {
			seen = append(seen, e.Seq)
		}
		if ev.Next != ev.Events[len(ev.Events)-1].Seq {
			t.Errorf("next %d, last %d", ev.Next, ev.Events[len(ev.Events)-1].Seq)
		}
		cursor = ev.Next
	}
	if len(seen) != 5 {
		t.Errorf("seen %v", seen)
	}
	for i, e := range all.Events {
		if i < len(seen) && seen[i] != e.Seq {
			t.Errorf("seen %v", seen)
		}
	}
	// A filtered page that matches nothing keeps the cursor, too.
	var ev journalapi.EventsResponse
	if r.do("GET", journalapi.PathEvents+"?reader=e_child1&type=attendance_day&after=3", "gw", nil, &ev); len(ev.Events) != 0 || ev.Next != 3 {
		t.Errorf("filtered: %+v", ev)
	}
	// No after starts at the beginning.
	if r.do("GET", journalapi.PathEvents+"?reader=e_child1&limit=1", "gw", nil, &ev); len(ev.Events) != 1 || ev.Events[0].Seq != all.Events[0].Seq {
		t.Errorf("no after: %+v", ev)
	}
}

// The read filters reach the journal.
func TestReadFilters(t *testing.T) {
	r := newRig(t)
	seed(r)
	var st journalapi.StateResponse
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=e_child1", "gw", nil, &st); len(st.Rows) != 1 || st.Rows[0].DataSubject != "e_child1" {
		t.Errorf("subject: %+v", st)
	}
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&type=attendance_day", "gw", nil, &st); len(st.Rows) != 0 {
		t.Errorf("type: %+v", st)
	}
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&type=assignment&scope="+st2scope(r), "gw", nil, &st); len(st.Rows) != 1 {
		t.Errorf("scope: %+v", st)
	}
	// The subject filter narrows the reader's view, never replaces the
	// reader: a child filtering on its sibling gets nothing of the sibling's.
	for _, path := range []string{journalapi.PathState, journalapi.PathEvents, journalapi.PathCounts} {
		code, raw := r.send("GET", path+"?reader=e_child1&subject=e_child2", "gw", "", nil)
		if code != 200 || strings.Contains(string(raw), "e_child2") {
			t.Errorf("%s: subject is not the reader: %d %s", path, code, raw)
		}
	}
	// subject= (present, empty) is the household's rows, not "every subject".
	if r.do("GET", journalapi.PathState+"?reader=e_parent1&subject=", "gw", nil, &st); len(st.Rows) != 0 {
		t.Errorf("empty subject: %+v", st)
	}
	// A removed record is shown only with include_removed=true.
	r.do("POST", journalapi.PathRecords, "ps", assignments(3, "e_child1", "powerschool", T0.Add(-30*time.Minute)), nil)
	if r.do("GET", journalapi.PathState+"?reader=e_child1", "gw", nil, &st); len(st.Rows) != 0 {
		t.Errorf("removed shown: %+v", st)
	}
	if r.do("GET", journalapi.PathState+"?reader=e_child1&include_removed=true", "gw", nil, &st); len(st.Rows) != 1 || !st.Rows[0].Removed {
		t.Errorf("include_removed: %+v", st)
	}
	var ev journalapi.EventsResponse
	if r.do("GET", journalapi.PathEvents+"?reader=e_parent1&subject=e_child2", "gw", nil, &ev); len(ev.Events) != 1 || ev.Events[0].DataSubject != "e_child2" {
		t.Errorf("events subject: %+v", ev)
	}
	if r.do("GET", journalapi.PathEvents+"?reader=e_parent1&min_importance=high", "gw", nil, &ev); len(ev.Events) != 0 {
		t.Errorf("min_importance: %+v", ev)
	}
	var cn journalapi.CountsResponse
	if r.do("GET", journalapi.PathCounts+"?reader=e_parent1&subject=e_child2", "gw", nil, &cn); len(cn.Counts) != 1 || cn.Counts[0].DataSubject != "e_child2" {
		t.Errorf("counts subject: %+v", cn)
	}
	// since defaults to seven days before now and is echoed back.
	if r.do("GET", journalapi.PathCounts+"?reader=e_parent1", "gw", nil, &cn); !cn.Since.Equal(T0.Add(-7 * 24 * time.Hour)) {
		t.Errorf("default since: %s", cn.Since)
	}
	cn = journalapi.CountsResponse{} // json.Unmarshal would merge into the old maps
	if r.do("GET", journalapi.PathCounts+"?reader=e_parent1&since=2026-10-16T11:45:00Z", "gw", nil, &cn); !cn.Since.Equal(T0.Add(-15 * time.Minute)) {
		t.Errorf("since: %s", cn.Since)
	}
	events := 0
	for _, c := range cn.Counts {
		for _, n := range c.Events {
			events += n
		}
	}
	// Every event was observed before T0-15m, so none counts since then.
	if events != 0 {
		t.Errorf("events since T0-15m: %+v", cn)
	}
}

func st2scope(r *rig) string {
	var st journalapi.StateResponse
	r.do("GET", journalapi.PathState+"?reader=e_child1", "gw", nil, &st)
	if len(st.Rows) != 1 {
		r.t.Fatalf("scope lookup: %+v", st)
	}
	return url.QueryEscape(st.Rows[0].Scope)
}

// The roster and the erasure ban come from the journal: an erased member
// reads nothing (403), and without a roster nobody does (503).
func TestReadJournalRefusals(t *testing.T) {
	r := newRig(t)
	seed(r)
	if _, err := r.j.Erase(context.Background(), journalapiAdmin, "e_child1"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{journalapi.PathState, journalapi.PathEvents, journalapi.PathCounts} {
		var rr journalapi.Response
		if code := r.do("GET", path+"?reader=e_child1", "gw", nil, &rr); code != 403 || rr.Reason != journalapi.ReasonPolicy {
			t.Errorf("%s as an erased member: %d %+v", path, code, rr)
		}
		if code := r.do("GET", path+"?reader=e_child2", "gw", nil, nil); code != 200 {
			t.Errorf("%s as another member: %d", path, code)
		}
	}
	r.ros.Store(nil)
	for _, path := range []string{journalapi.PathState, journalapi.PathEvents, journalapi.PathCounts} {
		var rr journalapi.Response
		if code := r.do("GET", path+"?reader=e_child2", "gw", nil, &rr); code != 503 || rr.Reason != journalapi.ReasonUnavailable {
			t.Errorf("%s without a roster: %d %+v", path, code, rr)
		}
	}
	var h journalapi.Healthz
	if code := r.do("GET", journalapi.PathHealthz, "gw", nil, &h); code != 200 || h.RosterSHA256 != "" {
		t.Errorf("healthz without a roster: %d %+v", code, h)
	}
}

// A database failure is a 500 that says nothing about it, on every read.
func TestReadDatabaseFailureIs500(t *testing.T) {
	r := newAuthRig(t)
	for _, path := range []string{journalapi.PathState, journalapi.PathEvents, journalapi.PathCounts} {
		code, raw := r.send("GET", path+"?reader=e_parent1", "gw", "", nil)
		var rr journalapi.Response
		_ = json.Unmarshal(raw, &rr)
		if code != 500 || rr.Reason != journalapi.ReasonInternal || rr.Detail != "" {
			t.Errorf("%s: %d %s", path, code, raw)
		}
	}
	// The real healthz (the auth rig serves a stub on its server).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", journalapi.PathHealthz, nil)
	req.Header.Set("Authorization", "Bearer gw")
	r.s.Handler().ServeHTTP(rec, req)
	if rr := decodeRefusal(t, rec); rec.Code != 500 || rr.Reason != journalapi.ReasonInternal || rr.Detail != "" {
		t.Errorf("healthz: %d %+v", rec.Code, rr)
	}
}

// No read refusal echoes a value or a parameter name it was sent.
func TestReadRefusalsDoNotEcho(t *testing.T) {
	r := newAuthRig(t)
	for _, path := range []string{
		journalapi.PathState + "?reader=e_parent1&SECRETNAME=1",
		journalapi.PathState + "?SECRETNAME=1&SECRETNAME=2",
		journalapi.PathState + "?reader=e_SECRETVALUE&reader=e_parent1",
		journalapi.PathEvents + "?reader=e_parent1&limit=SECRETVALUE",
		journalapi.PathEvents + "?reader=e_parent1&after=SECRETVALUE",
		journalapi.PathEvents + "?reader=e_parent1&min_importance=SECRETVALUE",
		journalapi.PathState + "?reader=e_parent1&include_removed=SECRETVALUE",
		journalapi.PathCounts + "?reader=e_parent1&since=SECRETVALUE",
		journalapi.PathState + "?reader=e_SECRETVALUE",
	} {
		code, raw := r.send("GET", path, "dash", "", nil)
		if code/100 != 4 || strings.Contains(string(raw), "SECRET") {
			t.Errorf("%s: %d %s", path, code, raw)
		}
	}
}

// api_requests_total names the route, never the path or the query.
func TestReadsAreCountedByRouteName(t *testing.T) {
	r := newAuthRig(t)
	r.send("GET", journalapi.PathState+"?reader=e_SECRETVALUE", "dash", "", nil)
	r.send("GET", journalapi.PathEvents+"?reader=e_SECRETVALUE", "dash", "", nil)
	r.send("GET", journalapi.PathCounts+"?reader=e_SECRETVALUE", "dash", "", nil)
	r.send("POST", journalapi.PathNotes, "dash", "application/json", "{}")
	m := r.scrape()
	for _, want := range []string{`code="403",endpoint="state"`, `code="403",endpoint="events"`, `code="403",endpoint="counts"`, `code="403",endpoint="notes"`} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(m, "SECRETVALUE") || strings.Contains(m, "/v1/") {
		t.Error("a path or query reached a label")
	}
}
