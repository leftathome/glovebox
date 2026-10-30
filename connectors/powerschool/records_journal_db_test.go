package powerschool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	pslib "github.com/leftathome/powerschool-go"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/httpapi"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/satoken"
	"github.com/leftathome/glovebox/internal/satoken/satokentest"
)

// The end-to-end test (Part B1 Task 23): the connector, with records on,
// against the real journal core -- its HTTP API, database, record types
// (this connector's own), roster and policy -- over three polls. Behind
// JOURNAL_TEST_DATABASE_URL like Part A's database tests:
//
//	JOURNAL_PG_HOST_NETWORK=1 scripts/journal-test-db.sh ./connectors/powerschool/ -run TestRecordsAgainstTheJournal
//
// The journal's own rig (internal/journal/httpapi rig_test.go) is internal
// to that package's tests and journaltest cannot hold an HTTP rig (httpapi's
// tests import journaltest), so the few lines that assemble the server are
// here, built from journaltest's fixtures.

// e2eConnectorToken is the bearer the connector's projected token file
// holds; the fake TokenReview maps it to the connector's ServiceAccount.
const e2eConnectorToken = "ps-projected-token"

// e2eGatewayToken is the reader's (openclaw's gateway).
const e2eGatewayToken = "gw-token"

// e2eJournal is the journal core under test and what it answered.
type e2eJournal struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	bodies  [][]byte // every POST /v1/records body, in order
	types   []string // each body's record_type
	answers []journalapi.Response
	codes   []int
}

func newE2EJournal(t *testing.T, now func() time.Time) *e2eJournal {
	t.Helper()
	db := journaltest.NewDB(t)
	reg, err := record.NewRegistry(RecordTypes())
	if err != nil {
		t.Fatal(err)
	}
	ros := journaltest.Roster(t)
	rosterNow := func() *roster.Roster { return ros }
	j := journal.New(db, journal.Options{Registry: reg, Guard: diff.DefaultGuard,
		Scanner: journaltest.NewFakeScanner("scanner-1"), Roster: rosterNow, Now: now})
	ctx := context.Background()
	if err := j.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	// The policy gitops gives the connector (docs/upgrading.md): every
	// PowerSchool type, its own source, roster subjects only.
	var all []string
	for _, ty := range RecordTypes() {
		all = append(all, ty.Name)
	}
	pol, err := policy.New(map[string]policy.Service{
		journaltest.SAPowerSchool: {WriteTypes: all, WriteScopes: map[string][]string{"source": {"powerschool"}}, WriteSubjects: policy.RosterSubjects},
		journaltest.SAGateway:     {Read: true, Readers: policy.Readers{Any: true}},
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	api := satokentest.New(t)
	for tok, sa := range map[string]string{e2eConnectorToken: journaltest.SAPowerSchool, e2eGatewayToken: journaltest.SAGateway} {
		ns, name, _ := strings.Cut(sa, "/")
		api.Allow(tok, "system:serviceaccount:"+ns+":"+name, journalapi.Audience)
	}
	auth, err := satoken.NewTokenReviewer(api.Access(), journalapi.Audience, pol.Callers())
	if err != nil {
		t.Fatal(err)
	}
	s, err := httpapi.New(httpapi.Deps{Journal: j, Registry: reg, Auth: auth, Policy: pol, Roster: rosterNow,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: now, Ready: func() bool { return true },
		RequestTimeout: 30 * time.Second, Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	e := &e2eJournal{t: t}
	h := s.Handler()
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != journalapi.PathRecords {
			h.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var a journalapi.Response
		_ = json.Unmarshal(rec.Body.Bytes(), &a)
		var snap struct {
			RecordType string `json:"record_type"`
		}
		_ = json.Unmarshal(body, &snap)
		e.mu.Lock()
		e.bodies = append(e.bodies, body)
		e.types = append(e.types, snap.RecordType)
		e.answers = append(e.answers, a)
		e.codes = append(e.codes, rec.Code)
		e.mu.Unlock()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// sent returns how many snapshots the journal has answered.
func (e *e2eJournal) sent() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.answers)
}

// answersFrom returns the codes, answers and record types from index i on.
func (e *e2eJournal) answersFrom(i int) ([]int, []journalapi.Response, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.codes[i:]...), append([]journalapi.Response(nil), e.answers[i:]...), append([]string(nil), e.types[i:]...)
}

// get reads path with query as the gateway, into out.
func (e *e2eJournal) get(path string, q url.Values, out any) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+e2eGatewayToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s?%s: %d %s", path, q.Encode(), resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		e.t.Fatal(err)
	}
}

// state returns the reader's rows (removed ones too).
func (e *e2eJournal) state(reader string) []journalapi.StateRow {
	e.t.Helper()
	var st journalapi.StateResponse
	e.get(journalapi.PathState, url.Values{"reader": {reader}, "include_removed": {"true"}}, &st)
	return st.Rows
}

// events returns every event the reader may see after seq after.
func (e *e2eJournal) events(reader string, after int64) []journalapi.Event {
	e.t.Helper()
	var out []journalapi.Event
	for {
		var ev journalapi.EventsResponse
		e.get(journalapi.PathEvents, url.Values{"reader": {reader}, "after": {itoa64(after)}, "limit": {"1000"}}, &ev)
		out = append(out, ev.Events...)
		if len(ev.Events) == 0 || ev.Next == after {
			return out
		}
		after = ev.Next
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// e2ePages are the portal's pages for one poll.
type e2ePages struct {
	attendance []pslib.AttendanceDay
	scored     bool // assignment 5001 graded 8/10 instead of missing
}

func TestRecordsAgainstTheJournal(t *testing.T) {
	var clock atomic.Pointer[time.Time]
	setNow := func(at time.Time) { clock.Store(&at) }
	now := func() time.Time { return *clock.Load() }
	setNow(pollTime)
	jrn := newE2EJournal(t, func() time.Time { return now().Add(time.Minute) })

	// The portal: pages chosen per poll.
	var pages atomic.Pointer[e2ePages]
	client := extraFixture("111111")
	client.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
		return attendanceFixture(client.Selected, pages.Load().attendance...), nil
	}
	client.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		a := sampleAssignment("5001", "Lab report")
		if pages.Load().scored {
			a.Score, a.Status = floatPtr(8), pslib.StatusGraded
		} else {
			a.Status, a.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}
		}
		return []*pslib.Assignment{a}, nil
	}

	// The connector, with records on and its token file.
	stateDir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(e2eConnectorToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := recentConfigOnRecordRules()
	cfg.Records = PowerSchoolRecords{Config: recordsBlock(jrn.srv.URL)}
	cfg.Records.TokenFile = tokenFile
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	c, err := NewConnector(client, cfg, "v0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	c.now = now
	provider := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	factory := NewRecordsFactory(cfg.Records, stateDir, now)
	c.NewRecords = func(metric.Meter) (RecordSink, error) { return factory(provider.Meter("glovebox.records")) }
	logs := captureLogs(t)
	metrics, err := connector.NewMetrics("powerschool-records-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	w, err := connector.NewStagingWriter(t.TempDir(), "powerschool")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wire(connector.ConnectorContext{Backend: w, Matcher: connector.NewRuleMatcher(cfg.Rules), Metrics: metrics}); err != nil {
		t.Fatal(err)
	}
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.startRecords(ctx)

	// poll runs one poll at at and waits until the spool has delivered
	// every snapshot; it returns the journal's answers to them.
	poll := func(at time.Time, p e2ePages) []string {
		t.Helper()
		setNow(at)
		pages.Store(&p)
		before := jrn.sent()
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatalf("pollNow: %v", err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for pendingSpoolFiles(t, stateDir) > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := pendingSpoolFiles(t, stateDir); n > 0 {
			t.Fatalf("%d snapshots still in the spool; log:\n%s", n, logs.String())
		}
		codes, answers, types := jrn.answersFrom(before)
		if len(answers) == 0 {
			t.Fatalf("nothing reached the journal; log:\n%s", logs.String())
		}
		for i, a := range answers {
			if codes[i] != http.StatusOK || a.Status != journalapi.StatusApplied {
				t.Fatalf("snapshot %d (%s) answered %d %+v", before+i, types[i], codes[i], a)
			}
		}
		return types
	}

	// Poll 1, 2026-10-20: the default grid (2 Oct late, 6 Oct unexcused
	// travel, 8 Oct unmarked) and a missing assignment.
	term1 := []pslib.AttendanceDay{
		codeDay(day(2026, 10, 2), "L"),
		{Date: day(2026, 10, 5), Kind: pslib.CellPresent},
		codeDay(day(2026, 10, 6), "VU"),
		{Date: day(2026, 10, 7), Kind: pslib.CellNoMeeting, Raw: "-"},
		{Date: day(2026, 10, 8), Kind: pslib.CellUnmarked, Raw: "."},
	}
	types := map[string]bool{}
	for _, ty := range poll(pollTime, e2ePages{attendance: term1}) {
		types[ty] = true
	}
	if len(types) != 7 {
		t.Fatalf("poll 1 wrote types %v, want all seven", types)
	}

	const markOct2, markOct6 = "example science 7a|1(A):2026-10-02", "example science 7a|1(A):2026-10-06"
	parentRows := jrn.state("e_parent1")
	for _, subject := range []string{"e_child1", "e_child2"} {
		for _, want := range []struct{ ty, key string }{
			{TypeAttendanceMark, markOct2}, {TypeAttendanceMark, markOct6},
			{TypeAttendanceDay, "2026-10-02"}, {TypeAttendanceDay, "2026-10-06"},
		} {
			if r := findRow(parentRows, want.ty, subject, want.key); r == nil || r.Removed {
				t.Fatalf("after poll 1, %s %s of %s: %+v", want.ty, want.key, subject, r)
			}
		}
		if n := countRows(parentRows, TypeAssignment, subject); n != 1 {
			t.Fatalf("after poll 1, %d assignment rows of %s, want 1", n, subject)
		}
	}
	day6 := findRow(parentRows, TypeAttendanceDay, "e_child1", "2026-10-06")
	if string(day6.Fields["classification"]) != `"full_day"` || string(day6.Fields["any_unexcused"]) != "true" {
		t.Fatalf("6 Oct is %s / %s, want full_day / true", day6.Fields["classification"], day6.Fields["any_unexcused"])
	}
	seq1 := lastSeq(jrn.events("e_parent1", 0))

	// A resend of a spooled file (the connector crashed after the journal
	// applied it, before deleting it) is answered duplicate, not applied
	// twice.
	jrn.mu.Lock()
	first := append([]byte(nil), jrn.bodies[0]...)
	jrn.mu.Unlock()
	code, again := resendAs(t, jrn.srv.URL, e2eConnectorToken, first)
	if code != http.StatusOK || again.Status != journalapi.StatusDuplicate {
		t.Fatalf("resend answered %d %+v, want 200 duplicate", code, again)
	}

	// Poll 2, the next day: 6 Oct's code corrected away and the
	// assignment scored.
	term1Corrected := append([]pslib.AttendanceDay(nil), term1...)
	term1Corrected[2] = pslib.AttendanceDay{Date: day(2026, 10, 6), Kind: pslib.CellPresent}
	poll(pollTime.Add(24*time.Hour), e2ePages{attendance: term1Corrected, scored: true})
	ev2 := jrn.events("e_parent1", seq1)
	for _, subject := range []string{"e_child1", "e_child2"} {
		if e := findEvent(ev2, TypeAssignment, subject, ""); e == nil || e.Kind != journalapi.KindUpdated || string(e.Changed["score"].New) != "8" {
			t.Fatalf("poll 2: assignment event of %s %+v, want updated with score 8", subject, e)
		}
		for _, want := range []struct{ ty, key string }{{TypeAttendanceMark, markOct6}, {TypeAttendanceDay, "2026-10-06"}} {
			if e := findEvent(ev2, want.ty, subject, want.key); e == nil || e.Kind != journalapi.KindRemoved {
				t.Fatalf("poll 2: %s %s of %s: %+v, want removed", want.ty, want.key, subject, e)
			}
		}
	}
	for _, e := range ev2 {
		if e.Kind == journalapi.KindRemoved && e.Key != markOct6 && e.Key != "2026-10-06" {
			t.Fatalf("poll 2 removed %s %s of %s", e.RecordType, e.Key, e.DataSubject)
		}
	}
	seq2 := lastSeq(ev2)
	if seq2 == 0 {
		seq2 = seq1
	}

	// Poll 3, 18 December, the last day of term: a full-day unexcused
	// absence today. Today is never settled, so no attendance_day yet; the
	// connector carries the day in its checkpoint (decision S16).
	const lastDay = "2026-12-18"
	decGrid := []pslib.AttendanceDay{
		{Date: day(2026, 12, 14), Kind: pslib.CellPresent},
		{Date: day(2026, 12, 15), Kind: pslib.CellPresent},
		{Date: day(2026, 12, 16), Kind: pslib.CellPresent},
		{Date: day(2026, 12, 17), Kind: pslib.CellPresent},
		codeDay(day(2026, 12, 18), "UA"),
	}
	poll(time.Date(2026, 12, 18, 23, 30, 0, 0, time.UTC), e2ePages{attendance: decGrid, scored: true})
	parentRows = jrn.state("e_parent1")
	for _, subject := range []string{"e_child1", "e_child2"} {
		if r := findRow(parentRows, TypeAttendanceDay, subject, lastDay); r != nil {
			t.Fatalf("the last day of term was sent before it settled: %+v", r)
		}
	}
	evDec := jrn.events("e_parent1", seq2)
	for _, e := range evDec {
		if e.Kind == journalapi.KindRemoved {
			t.Fatalf("the December poll removed %s %s of %s", e.RecordType, e.Key, e.DataSubject)
		}
	}
	if s := lastSeq(evDec); s > seq2 {
		seq2 = s
	}

	// Poll 4, January: a new term's grid (same school year). Its range
	// starts at the term's first date, so last term's marks and days stay;
	// the carried 18 December settles and is sent on its own, complete=false.
	term2 := []pslib.AttendanceDay{
		{Date: day(2027, 1, 4), Kind: pslib.CellPresent},
		codeDay(day(2027, 1, 5), "UA"),
		{Date: day(2027, 1, 6), Kind: pslib.CellPresent},
	}
	poll(time.Date(2027, 1, 12, 23, 30, 0, 0, time.UTC), e2ePages{attendance: term2, scored: true})
	ev3 := jrn.events("e_parent1", seq2)
	for _, e := range ev3 {
		if e.Kind == journalapi.KindRemoved {
			t.Fatalf("term rollover removed %s %s of %s", e.RecordType, e.Key, e.DataSubject)
		}
	}
	parentRows = jrn.state("e_parent1")
	for _, subject := range []string{"e_child1", "e_child2"} {
		for _, want := range []struct{ ty, key string }{
			{TypeAttendanceMark, markOct2}, {TypeAttendanceDay, "2026-10-02"},
			{TypeAttendanceMark, "example science 7a|1(A):2027-01-05"}, {TypeAttendanceDay, "2027-01-05"},
			{TypeAttendanceMark, "example science 7a|1(A):" + lastDay}, {TypeAttendanceDay, lastDay},
		} {
			if r := findRow(parentRows, want.ty, subject, want.key); r == nil || r.Removed {
				t.Fatalf("after the rollover, %s %s of %s: %+v", want.ty, want.key, subject, r)
			}
		}
	}
	wantCarried := func(when string) {
		t.Helper()
		rows := jrn.state("e_parent1")
		for _, subject := range []string{"e_child1", "e_child2"} {
			r := findRow(rows, TypeAttendanceDay, subject, lastDay)
			if r == nil || r.Removed || string(r.Fields["classification"]) != `"full_day"` || string(r.Fields["any_unexcused"]) != "true" {
				t.Fatalf("%s, the carried %s of %s: %+v", when, lastDay, subject, r)
			}
		}
	}
	wantCarried("after the first January poll")
	if e := findEvent(ev3, TypeAttendanceDay, "e_child1", lastDay); e == nil || e.Kind != journalapi.KindCreated {
		t.Fatalf("the carried day's event %+v, want created", e)
	}
	seq3 := lastSeq(ev3)

	// Poll 5, the next day: the new term's complete snapshot again. Its
	// range does not reach back to 18 December, which stays.
	poll(time.Date(2027, 1, 13, 23, 30, 0, 0, time.UTC), e2ePages{attendance: term2, scored: true})
	for _, e := range jrn.events("e_parent1", seq3) {
		if e.Kind == journalapi.KindRemoved {
			t.Fatalf("the second January poll removed %s %s of %s", e.RecordType, e.Key, e.DataSubject)
		}
	}
	wantCarried("after the second January poll")
	parentRows = jrn.state("e_parent1")

	// Visibility: a child reads only its own rows, and never a term
	// comment (audience guardians); the guardian reads both children's.
	childRows := jrn.state("e_child1")
	if len(childRows) == 0 {
		t.Fatal("e_child1 reads nothing")
	}
	for _, r := range childRows {
		if r.DataSubject != "e_child1" || !strings.Contains(r.Scope, "data_subject=e_child1") {
			t.Fatalf("e_child1 reads a row of %s (scope %s)", r.DataSubject, r.Scope)
		}
		if r.RecordType == TypeTermComment {
			t.Fatalf("e_child1 reads a term comment: %+v", r)
		}
	}
	if countRows(parentRows, TypeTermComment, "e_child1") == 0 || countRows(parentRows, TypeTermComment, "e_child2") == 0 {
		t.Fatal("the guardian reads no term comments")
	}
	for _, r := range parentRows {
		if !strings.Contains(r.Scope, "data_subject="+r.DataSubject) {
			t.Fatalf("row of %s in scope %s", r.DataSubject, r.Scope)
		}
		if r.RecordType == TypeTermComment && strings.Contains(string(r.Fields["comment"]), kidOf[map[string]string{"e_child1": "111112", "e_child2": "111111"}[r.DataSubject]]+" ") {
			t.Fatalf("a term comment of %s carries the other kid's text: %s", r.DataSubject, r.Fields["comment"])
		}
	}
	for _, e := range jrn.events("e_child1", 0) {
		if e.DataSubject != "e_child1" || e.RecordType == TypeTermComment {
			t.Fatalf("e_child1 sees event %s %s of %s", e.RecordType, e.Key, e.DataSubject)
		}
	}
	if strings.Contains(logs.String(), e2eConnectorToken) {
		t.Fatal("the token reached the log")
	}
}

// pendingSpoolFiles counts the files waiting in the spool (rejected/ is a
// directory and not counted).
func pendingSpoolFiles(t *testing.T, stateDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func resendAs(t *testing.T, base, token string, body []byte) (int, journalapi.Response) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+journalapi.PathRecords, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a journalapi.Response
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &a)
	return resp.StatusCode, a
}

func findRow(rows []journalapi.StateRow, ty, subject, key string) *journalapi.StateRow {
	for i, r := range rows {
		if r.RecordType == ty && r.DataSubject == subject && r.Key == key {
			return &rows[i]
		}
	}
	return nil
}

func countRows(rows []journalapi.StateRow, ty, subject string) int {
	n := 0
	for _, r := range rows {
		if r.RecordType == ty && r.DataSubject == subject && !r.Removed {
			n++
		}
	}
	return n
}

// findEvent returns the last event of type ty about subject with key ("":
// any key).
func findEvent(evs []journalapi.Event, ty, subject, key string) *journalapi.Event {
	var out *journalapi.Event
	for i, e := range evs {
		if e.RecordType == ty && e.DataSubject == subject && (key == "" || e.Key == key) {
			out = &evs[i]
		}
	}
	return out
}

func lastSeq(evs []journalapi.Event) int64 {
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].Seq
}
