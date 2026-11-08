package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
	"github.com/leftathome/glovebox/internal/satoken"
	"github.com/leftathome/glovebox/internal/satoken/satokentest"
)

// The golden answers (Part C Task 4): what the real API answers a reader,
// written to docs/journal-api-examples/ so a consumer that cannot import
// journalapi (openclaw) pins its decoder to them. Behind
// JOURNAL_TEST_DATABASE_URL like the other database tests:
//
//	JOURNAL_PG_HOST_NETWORK=1 scripts/journal-test-db.sh ./internal/journal/httpapi/ -run TestGoldenAnswers -p 1
//
// With -update the files are rewritten; without, any difference fails.
//
// The journal here runs the connectors' real record types (the chart's
// journal-record-types files, which is what the core loads), the fixture
// roster, the spec's two attendance rules and the gateway's policy as gitops
// gives it (read, counts, readers "*"). The clock is fixed, so times are
// stable; event sequence numbers are renumbered 1, 2, ... in the order the
// journal assigned them (a database sequence may skip), and "next" with
// them. Every file stays a valid wire body.

var updateGolden = flag.Bool("update", false, "rewrite docs/journal-api-examples/ from the real API")

const goldenDir = "../../../docs/journal-api-examples"

const (
	goldenGatewayToken = "gw-golden"
	goldenPSToken      = "ps-golden"
	goldenSGToken      = "sg-golden"
)

// goldenTime is the first poll; the second is a day later.
var goldenTime = time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)

type goldenJournal struct {
	t     *testing.T
	srv   *httptest.Server
	now   atomic.Pointer[time.Time]
	ready atomic.Bool
	snaps int
}

func loadChartTypes(t *testing.T) []record.Type {
	t.Helper()
	var all []record.Type
	for _, f := range []string{"powerschool.json", "schoology.json"} {
		raw, err := os.ReadFile(filepath.Join("../../../charts/glovebox/journal-record-types", f))
		if err != nil {
			t.Fatal(err)
		}
		var types []record.Type
		if err := json.Unmarshal(raw, &types); err != nil {
			t.Fatal(err)
		}
		all = append(all, types...)
	}
	return all
}

func newGoldenJournal(t *testing.T) *goldenJournal {
	t.Helper()
	g := &goldenJournal{t: t}
	g.setNow(goldenTime)
	g.ready.Store(true)
	now := func() time.Time { return *g.now.Load() }
	types := loadChartTypes(t)
	reg, err := record.NewRegistry(types)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Compile(journaltest.Rules(), reg)
	if err != nil {
		t.Fatal(err)
	}
	var ps, sg []string
	for _, ty := range types {
		if ty.Owner == "powerschool" {
			ps = append(ps, ty.Name)
		} else {
			sg = append(sg, ty.Name)
		}
	}
	pol, err := policy.New(map[string]policy.Service{
		journaltest.SAGateway:     {Read: true, Counts: true, Readers: policy.Readers{Any: true}},
		journaltest.SAPowerSchool: {WriteTypes: ps, WriteScopes: map[string][]string{"source": {"powerschool"}}, WriteSubjects: policy.RosterSubjects},
		journaltest.SASchoology:   {WriteTypes: sg, WriteScopes: map[string][]string{"source": {"schoology"}}, WriteSubjects: policy.RosterSubjects},
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	ros := journaltest.Roster(t)
	rosterNow := func() *roster.Roster { return ros }
	db := journaltest.NewDB(t)
	j := journal.New(db, journal.Options{Registry: reg, Rules: rs, Guard: diff.DefaultGuard,
		Scanner: journaltest.NewFakeScanner("scanner-1"), Roster: rosterNow, Now: now})
	ctx := context.Background()
	if err := j.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	api := satokentest.New(t)
	for tok, sa := range map[string]string{goldenGatewayToken: journaltest.SAGateway,
		goldenPSToken: journaltest.SAPowerSchool, goldenSGToken: journaltest.SASchoology} {
		ns, name, _ := strings.Cut(sa, "/")
		api.Allow(tok, "system:serviceaccount:"+ns+":"+name, journalapi.Audience)
	}
	auth, err := satoken.NewTokenReviewer(api.Access(), journalapi.Audience, pol.Callers())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Deps{Journal: j, Registry: reg, Auth: auth, Policy: pol, Roster: rosterNow,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: now, Ready: g.ready.Load,
		RequestTimeout: 30 * time.Second, Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	g.srv = httptest.NewServer(s.Handler())
	t.Cleanup(g.srv.Close)
	return g
}

func (g *goldenJournal) setNow(t time.Time) { g.now.Store(&t) }

// send answers the raw body of one request.
func (g *goldenJournal) send(method, path, token string, body any) (int, []byte) {
	g.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			g.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, g.srv.URL+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// post sends one snapshot and requires it applied.
func (g *goldenJournal) post(token, recordType string, scope []journalapi.ScopePart, complete bool,
	rng *journalapi.CompleteRange, observed time.Time, recs ...journalapi.Record) {
	g.t.Helper()
	g.snaps++
	snap := journalapi.Snapshot{SnapshotID: fmt.Sprintf("00000000-0000-4000-9000-%012d", g.snaps),
		RecordType: recordType, SchemaVersion: 1, Scope: scope, Complete: complete, CompleteRange: rng,
		ObservedAt: observed, Records: recs}
	if snap.Records == nil {
		snap.Records = []journalapi.Record{}
	}
	code, raw := g.send("POST", journalapi.PathRecords, token, snap)
	var resp journalapi.Response
	if err := json.Unmarshal(raw, &resp); err != nil || code != 200 || resp.Status != journalapi.StatusApplied {
		g.t.Fatalf("%s %v: %d %s", recordType, scope, code, raw)
	}
}

func rec(key, subject string, audience []string, fields string) journalapi.Record {
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fields), &f); err != nil {
		panic(err)
	}
	return journalapi.Record{Key: key, DataSubject: subject, Audience: audience, Fields: f}
}

// goldenScenario is two polls of both connectors for both children: a
// course mark, an assignment scored on the second poll, an attendance mark
// removed, a full-day absence (a rule with notify advice), a Schoology
// assignment that leaves the listing, a text the scanner withholds, and a
// term comment whose audience widens from guardians to the subject.
func goldenScenario(g *goldenJournal) {
	sg := []string{"subject", "guardians"}
	t1 := goldenTime.Add(-10 * time.Minute)
	t2 := t1.Add(24 * time.Hour)
	year := records.Part("school_year", "2026-27")
	marks := &journalapi.CompleteRange{Field: "date", From: "2026-10-13", To: "2026-10-14"}
	days := &journalapi.CompleteRange{Field: "date", From: "2026-10-13", To: "2026-10-14"}

	// Poll 1.
	for _, c := range []struct{ subject, course string }{{"e_child1", "c1"}, {"e_child2", "c2"}} {
		g.post(goldenPSToken, "course_mark", records.Scope("powerschool", "grades", c.subject, records.Part("course", c.course)), true, nil, t1,
			rec(c.course, c.subject, sg, `{"course": "EXAMPLE SCIENCE 7", "period": "3", "teacher": "EXAMPLE TEACHER",
				"q1_letter": "B+", "q1_percent": 88.5, "q2_letter": null, "q2_percent": null}`))
	}
	g.post(goldenPSToken, "assignment", records.Scope("powerschool", "assignments", "e_child1", records.Part("course", "c1")), true, nil, t1,
		rec("c1:98765", "e_child1", sg, `{"course": "EXAMPLE SCIENCE 7", "category": "Homework", "title": "Worksheet 3",
			"due": "2026-10-20", "score": null, "max_score": 10, "percent": null, "letter": null, "missing": false,
			"late": false, "incomplete": false, "exempt": false, "collected": true, "absent": false,
			"teacher_comment": null, "description": "INJECT example text the scanner withholds", "category_description": null}`))
	g.post(goldenPSToken, "attendance_mark", records.Scope("powerschool", "attendance_marks", "e_child1", year), true, marks, t1,
		rec("c1:2026-10-13", "e_child1", sg, `{"date": "2026-10-13", "course": "EXAMPLE SCIENCE 7", "period": "3",
			"code": "T", "code_description": "Tardy", "kind": "tardy", "excused": false}`),
		rec("c1:2026-10-14", "e_child1", sg, `{"date": "2026-10-14", "course": "EXAMPLE SCIENCE 7", "period": "3",
			"code": "A", "code_description": "Absent", "kind": "absent", "excused": false}`))
	g.post(goldenPSToken, "attendance_day", records.Scope("powerschool", "attendance_days", "e_child1", year), true, days, t1,
		rec("2026-10-14", "e_child1", sg, `{"date": "2026-10-14", "periods_met": 6, "periods_unmarked": 0,
			"absent_unexcused": 6, "absent_excused": 0, "late": 0, "left_early": 0, "authorized": 0, "unknown_codes": 0,
			"any_unexcused": true, "classification": "full_day"}`))
	g.post(goldenPSToken, "term_comment", records.Scope("powerschool", "term_comments", "e_child1", records.Part("term", "Q1")), true, nil, t1,
		rec("c1:Q1", "e_child1", []string{"guardians"}, `{"course": "EXAMPLE SCIENCE 7", "term": "Q1", "comment": "Example comment, first version."}`))
	g.post(goldenSGToken, "lms_assignment", records.Scope("schoology", "assignments", "e_child1"), false, nil, t1,
		rec("a1001", "e_child1", sg, `{"title": "Lab report", "course": "EXAMPLE SCIENCE 7", "status": "upcoming",
			"link": "https://lms.example/assignment/a1001", "due": "2026-10-21T06:59:00Z"}`),
		rec("a1002", "e_child1", sg, `{"title": "Reading log", "course": "EXAMPLE SCIENCE 7", "status": "overdue",
			"link": "https://lms.example/assignment/a1002", "due": "2026-10-12T06:59:00Z"}`))
	g.post(goldenSGToken, "lms_assignment_detail", records.Scope("schoology", "assignment_details", "e_child1"), false, nil, t1,
		rec("a1001", "e_child1", sg, `{"description": "Write up the experiment.", "comments": null}`))

	// Poll 2, a day later.
	g.setNow(t2.Add(10 * time.Minute))
	g.post(goldenPSToken, "assignment", records.Scope("powerschool", "assignments", "e_child1", records.Part("course", "c1")), true, nil, t2,
		rec("c1:98765", "e_child1", sg, `{"course": "EXAMPLE SCIENCE 7", "category": "Homework", "title": "Worksheet 3",
			"due": "2026-10-20", "score": 9, "max_score": 10, "percent": 90, "letter": "A-", "missing": false,
			"late": false, "incomplete": false, "exempt": false, "collected": true, "absent": false,
			"teacher_comment": null, "description": "INJECT example text the scanner withholds", "category_description": null}`))
	g.post(goldenPSToken, "attendance_mark", records.Scope("powerschool", "attendance_marks", "e_child1", year), true, marks, t2,
		rec("c1:2026-10-14", "e_child1", sg, `{"date": "2026-10-14", "course": "EXAMPLE SCIENCE 7", "period": "3",
			"code": "A", "code_description": "Absent", "kind": "absent", "excused": false}`))
	g.post(goldenPSToken, "term_comment", records.Scope("powerschool", "term_comments", "e_child1", records.Part("term", "Q1")), true, nil, t2,
		rec("c1:Q1", "e_child1", sg, `{"course": "EXAMPLE SCIENCE 7", "term": "Q1", "comment": "Example comment, second version."}`))
	g.post(goldenSGToken, "lms_assignment", records.Scope("schoology", "assignments", "e_child1"), false, nil, t2,
		rec("a1001", "e_child1", sg, `{"title": "Lab report", "course": "EXAMPLE SCIENCE 7", "status": "submitted",
			"link": "https://lms.example/assignment/a1001", "due": "2026-10-21T06:59:00Z"}`))
	g.setNow(t2.Add(20 * time.Minute))
}

// seqMap renumbers event sequence numbers 1, 2, ... in ascending order.
type seqMap map[int64]int64

func (m seqMap) learn(events []any) {
	var seqs []int64
	for k := range m {
		seqs = append(seqs, k)
	}
	for _, e := range events {
		n, _ := e.(map[string]any)["seq"].(json.Number).Int64()
		if _, ok := m[n]; !ok {
			seqs = append(seqs, n)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		m[s] = int64(i + 1)
	}
}

func (m seqMap) of(t *testing.T, v any) json.Number {
	t.Helper()
	n, err := v.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return "0"
	}
	r, ok := m[n]
	if !ok {
		t.Fatalf("seq %d was never seen in an event", n)
	}
	return json.Number(strconv.FormatInt(r, 10))
}

func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	utcTimes(t, v)
	return v
}

// timeKeys are the answers' time fields.
var timeKeys = map[string]bool{"observed_at": true, "prev_observed_at": true, "first_seen_at": true,
	"last_seen_at": true, "confirmed_at": true, "since": true}

// utcTimes rewrites every time field in UTC. NOTE (Part C Task 4): times
// read back from the database carry the test process's local offset (a
// developer machine's TZ); the deployed core runs in UTC and answers "Z"
// times, as the API documents. The files hold the UTC form so they do not
// depend on where the test runs.
func utcTimes(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && timeKeys[k] {
				ts, err := time.Parse(time.RFC3339Nano, s)
				if err != nil {
					t.Fatalf("%s: %v", k, err)
				}
				x[k] = ts.UTC().Format(time.RFC3339Nano)
				continue
			}
			utcTimes(t, e)
		}
	case []any:
		for _, e := range x {
			utcTimes(t, e)
		}
	}
}

// golden compares (or with -update writes) one example file.
func golden(t *testing.T, name string, v any) {
	t.Helper()
	want, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join(goldenDir, name)
	if *updateGolden {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (generate it with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the real API's answer; if the change is intended, regenerate with -update and tell the consumers (openclaw pins these):\n%s", name, want)
	}
}

func TestGoldenAnswers(t *testing.T) {
	g := newGoldenJournal(t)
	goldenScenario(g)
	readers := []string{"e_parent1", "e_child1"}

	get := func(path string, wantCode int) any {
		t.Helper()
		code, raw := g.send("GET", path, goldenGatewayToken, nil)
		if code != wantCode {
			t.Fatalf("GET %s: %d %s", path, code, raw)
		}
		return decodeAny(t, raw)
	}

	// Events first: they name every seq the files will show.
	seqs := seqMap{}
	events := map[string]map[string]any{}
	for _, r := range readers {
		ev := get(journalapi.PathEvents+"?reader="+r+"&limit=1000", 200).(map[string]any)
		seqs.learn(ev["events"].([]any))
		events[r] = ev
	}
	for _, r := range readers {
		ev := events[r]
		last := ev["next"]
		for _, e := range ev["events"].([]any) {
			e.(map[string]any)["seq"] = seqs.of(t, e.(map[string]any)["seq"])
		}
		ev["next"] = seqs.of(t, last)
		golden(t, "events-"+r+".json", ev)
		// The page after the last: empty, next unchanged.
		n, _ := last.(json.Number).Int64()
		empty := get(journalapi.PathEvents+"?reader="+r+"&after="+strconv.FormatInt(n, 10), 200).(map[string]any)
		if len(empty["events"].([]any)) != 0 {
			t.Fatalf("%s: the page after next is not empty", r)
		}
		empty["next"] = seqs.of(t, empty["next"])
		if r == "e_parent1" {
			golden(t, "events-empty.json", empty)
		}
	}

	for _, r := range readers {
		golden(t, "state-"+r+".json", get(journalapi.PathState+"?reader="+r, 200))
	}
	golden(t, "state-e_parent1-include-removed.json", get(journalapi.PathState+"?reader=e_parent1&include_removed=true", 200))
	golden(t, "counts-e_parent1.json", get(journalapi.PathCounts+"?reader=e_parent1", 200))
	golden(t, "healthz.json", get(journalapi.PathHealthz, 200))
	// The roster's bytes beside the answers, so a consumer can check that
	// its own hash of the file equals roster_sha256.
	rosterPath := filepath.Join(goldenDir, "household.json")
	if *updateGolden {
		if err := os.WriteFile(rosterPath, []byte(journaltest.HouseholdV1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(rosterPath); err != nil || string(raw) != journaltest.HouseholdV1 {
		t.Errorf("household.json is not journaltest.HouseholdV1 (%v); regenerate with -update", err)
	}

	golden(t, "refusal-policy.json", get(journalapi.PathState+"?reader=e_nobody", 403))
	golden(t, "refusal-bad-request.json", get(journalapi.PathState+"?reader=e_child1&audience=guardians", 400))
	code, raw := g.send("GET", journalapi.PathHealthz, "", nil)
	if code != 401 {
		t.Fatalf("no token: %d %s", code, raw)
	}
	golden(t, "refusal-unauthenticated.json", decodeAny(t, raw))
	g.ready.Store(false)
	golden(t, "refusal-unavailable.json", get(journalapi.PathHealthz, 503))
	g.ready.Store(true)

	// The examples carry what the scenario is for (a guard against a
	// scenario edit that silently drops a shape).
	all, _ := json.Marshal(map[string]any{"p": events["e_parent1"], "c": events["e_child1"]})
	for _, want := range []string{`"kind":"removed"`, `"kind":"updated"`, `"last_fields"`, `"prev_observed_at"`,
		`"notify"`, `"group_key"`, `"importance":"high"`, journalapi.WithheldPrefix + "failed content scan",
		journalapi.WithheldPrefix + "different audience"} {
		if !strings.Contains(string(all), want) {
			t.Errorf("no event carries %s", want)
		}
	}
	st, _ := json.Marshal(get(journalapi.PathState+"?reader=e_parent1", 200))
	if !strings.Contains(string(st), `"in_latest_snapshot":false`) {
		t.Errorf("no row has left its listing")
	}
	cst, _ := json.Marshal(get(journalapi.PathState+"?reader=e_child1", 200))
	if strings.Contains(string(cst), `"e_child2"`) {
		t.Errorf("e_child1's state names e_child2")
	}
}
