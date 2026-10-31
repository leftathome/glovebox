package powerschool

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// recorderSink is a RecordSink that keeps what it is given, one batch per
// Emit call (one call per kid turn).
type recorderSink struct {
	mu      sync.Mutex
	batches [][]journalapi.Snapshot
	cuts    int
	err     error
	runs    int
}

func (r *recorderSink) Emit(s []journalapi.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, s)
	return r.err
}

func (r *recorderSink) OnCut() { r.mu.Lock(); r.cuts++; r.mu.Unlock() }

func (r *recorderSink) Run(ctx context.Context) {
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()
	<-ctx.Done()
}

func (r *recorderSink) all() []journalapi.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []journalapi.Snapshot
	for _, b := range r.batches {
		out = append(out, b...)
	}
	return out
}

// pollTime is the fixed clock of the emit tests, after every fixture date.
var pollTime = time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)

// recordsConfig is recentConfig on the record rules, records enabled.
func recordsConfig() Config {
	cfg := recentConfig()
	cfg.Rules = recordRules()
	cfg.Records = PowerSchoolRecords{Config: recordsBlock("http://journal.example:9095")}
	ApplyDefaults(&cfg)
	return cfg
}

// pollWithSink runs one poll of client with sink as the record sink.
func pollWithSink(t *testing.T, client *fakeClient, cfg Config, sink RecordSink) ([]stagedItem, connector.Checkpoint, *PowerSchoolConnector) {
	t.Helper()
	cp := newMemCheckpoint()
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "powerschool")
	if err != nil {
		t.Fatal(err)
	}
	var mk func() (RecordSink, error)
	if sink != nil {
		mk = func() (RecordSink, error) { return sink, nil }
	}
	c := newRecordsConnector(t, client, cfg, w, mk)
	captureLogs(t)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	return readItems(t, stagingDir), cp, c
}

// newRecordsConnector builds and wires a connector whose NewRecords
// returns what mk returns, with the fixed clock pollTime.
func newRecordsConnector(t *testing.T, client PowerSchoolClient, cfg Config, backend connector.StagingBackend,
	mk func() (RecordSink, error)) *PowerSchoolConnector {
	t.Helper()
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	c, err := NewConnector(client, cfg, "v0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return pollTime }
	if mk != nil {
		c.NewRecords = func(metric.Meter) (RecordSink, error) { return mk() }
	}
	metrics, err := connector.NewMetrics("powerschool-records-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	if err := c.Wire(connector.ConnectorContext{Backend: backend, Matcher: connector.NewRuleMatcher(cfg.Rules), Metrics: metrics}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	return c
}

func snapsOfType(snaps []journalapi.Snapshot, ty string) []journalapi.Snapshot {
	var out []journalapi.Snapshot
	for _, s := range snaps {
		if s.RecordType == ty {
			out = append(out, s)
		}
	}
	return out
}

func subjectOfScope(s journalapi.Snapshot) string {
	for _, p := range s.Scope {
		if p[0] == "data_subject" {
			return p[1]
		}
	}
	return ""
}

func TestEmitEverySurfaceForEveryKid(t *testing.T) {
	sink := &recorderSink{}
	pollWithSink(t, extraFixture("111111"), recordsConfig(), sink)
	if len(sink.batches) != 2 {
		t.Fatalf("%d Emit calls, want one per kid", len(sink.batches))
	}
	for i, want := range []string{"e_child1", "e_child2"} {
		batch := sink.batches[i]
		validateAll(t, batch)
		types := map[string]bool{}
		for _, s := range batch {
			types[s.RecordType] = true
			if subjectOfScope(s) != want {
				t.Errorf("kid %d's turn emitted a %s snapshot about %q", i+1, s.RecordType, subjectOfScope(s))
			}
			for _, r := range s.Records {
				if r.DataSubject != want {
					t.Errorf("kid %d's turn emitted a %s record about %q", i+1, s.RecordType, r.DataSubject)
				}
			}
			if !s.ObservedAt.Equal(pollTime) {
				t.Errorf("%s observed_at %v, want the read time %v", s.RecordType, s.ObservedAt, pollTime)
			}
			if s.RecordType == TypeTermComment {
				for _, r := range s.Records {
					if !reflect.DeepEqual(r.Audience, []string{"guardians"}) {
						t.Errorf("term_comment audience %v", r.Audience)
					}
				}
			}
		}
		if len(types) != 7 {
			t.Errorf("kid %d: record types %v, want all 7", i+1, types)
		}
	}
}

// A kid whose turn did not verify gets neither items nor records.
func TestEmitOnlyForAVerifiedTurn(t *testing.T) {
	client := extraFixture("111111")
	lookup := client.AssignmentsFunc
	client.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		if studentID == "111111" {
			// The session moves to the other child during k1's reads.
			defer func() { client.Selected = "111112" }()
		}
		return lookup(ctx, studentID, g)
	}
	cfg := recordsConfig()
	sink := &recorderSink{}
	items, _, _ := pollWithSink(t, client, cfg, sink)
	for _, it := range items {
		if it.Meta.Tags["kid"] == "k1" {
			t.Fatalf("k1 item staged: %s", it.Meta.Subject)
		}
	}
	for _, s := range sink.all() {
		if subjectOfScope(s) != "e_child2" {
			t.Fatalf("a %s snapshot about %q after k1's turn failed", s.RecordType, subjectOfScope(s))
		}
	}
	if len(sink.batches) != 1 {
		t.Fatalf("%d Emit calls", len(sink.batches))
	}
}

func TestEmitFollowsTheSurfaceSwitches(t *testing.T) {
	cfg := recordsConfig()
	cfg.Surfaces.Grades = boolPtr(false)
	cfg.Surfaces.TeacherComments = boolPtr(false)
	sink := &recorderSink{}
	pollWithSink(t, extraFixture("111111"), cfg, sink)
	all := sink.all()
	if n := len(snapsOfType(all, TypeCourseMark)) + len(snapsOfType(all, TypeTermComment)); n != 0 {
		t.Fatalf("%d snapshots for switched-off surfaces", n)
	}
	if len(snapsOfType(all, TypeAttendanceTotal)) == 0 || len(snapsOfType(all, TypeAssignment)) == 0 {
		t.Fatal("the surfaces left on emitted nothing")
	}
}

// A surface without a rule for the kid emits no records, as it stages no
// items.
func TestEmitNeedsTheSurfaceRule(t *testing.T) {
	cfg := recordsConfig()
	sink := &recorderSink{}
	cp, _ := connector.NewCheckpoint(t.TempDir())
	w, _ := connector.NewStagingWriter(t.TempDir(), "powerschool")
	c := newRecordsConnector(t, extraFixture("111111"), cfg, w, func() (RecordSink, error) { return sink, nil })
	var rules []connector.Rule
	for _, r := range cfg.Rules {
		if r.Match != "powerschool:k1:term_grade" {
			rules = append(rules, r)
		}
	}
	c.matcher = connector.NewRuleMatcher(rules)
	captureLogs(t)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	for _, s := range snapsOfType(sink.all(), TypeTermGrade) {
		if subjectOfScope(s) == "e_child1" {
			t.Fatal("term_grade records for k1 without k1's term_grade rule")
		}
	}
	if len(snapsOfType(sink.all(), TypeTermGrade)) != 1 {
		t.Fatal("k2's term_grade records are missing")
	}
}

// An Emit error is logged; the items and the checkpoint are what they
// would have been.
func TestEmitErrorLeavesItemsAlone(t *testing.T) {
	offItems, offCP, _ := pollWithSink(t, extraFixture("111111"), recentConfigOnRecordRules(), nil)
	sink := &recorderSink{err: errors.New("spool write failed")}
	onItems, onCP, _ := pollWithSink(t, extraFixture("111111"), recordsConfig(), sink)
	if diff := diffItems(offItems, onItems); diff != "" {
		t.Fatalf("items differ: %s", diff)
	}
	if diff := diffCheckpoints(t, offCP, onCP); diff != "" {
		t.Fatalf("checkpoints differ: %s", diff)
	}
}

// A builder that panics is recovered: the poll and the items carry on.
func TestEmitRecoversABuilderPanic(t *testing.T) {
	sink := &panicSink{}
	items, _, _ := pollWithSink(t, extraFixture("111111"), recordsConfig(), sink)
	if len(items) == 0 {
		t.Fatal("no items after a records panic")
	}
	if sink.calls != 2 {
		t.Fatalf("%d Emit calls", sink.calls)
	}
}

func TestRecordsFactory(t *testing.T) {
	if NewRecordsFactory(PowerSchoolRecords{}, t.TempDir(), nil) != nil {
		t.Fatal("a factory for records that are off")
	}
	cfg := PowerSchoolRecords{Config: recordsBlock("http://journal.example:9095")}
	if _, err := NewRecordsFactory(cfg, "relative/state", nil)(nil); err == nil {
		t.Fatal("a spool on a relative state directory")
	}
	if _, err := NewRecordsFactory(cfg, "", nil)(nil); err == nil {
		t.Fatal("a spool without a state directory")
	}
	dir := t.TempDir()
	sink, err := NewRecordsFactory(cfg, dir, nil)(nil)
	if err != nil || sink == nil {
		t.Fatalf("sink %v, err %v", sink, err)
	}
	if !fileExists(dir + "/spool") {
		t.Fatal("no spool directory")
	}
}

// A sink that cannot be built fails Wire: the process does not start
// without the records it was configured for.
func TestWireFailsWhenRecordsCannotStart(t *testing.T) {
	c, err := NewConnector(extraFixture("111111"), recordsConfig(), "v0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	c.NewRecords = func(metric.Meter) (RecordSink, error) { return nil, errors.New("no spool") }
	metrics, err := connector.NewMetrics("powerschool-records-wire-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	w, _ := connector.NewStagingWriter(t.TempDir(), "powerschool")
	if err := c.Wire(connector.ConnectorContext{Backend: w, Matcher: connector.NewRuleMatcher(recordRules()), Metrics: metrics}); err == nil {
		t.Fatal("Wire succeeded without its records sink")
	}
}

// Each Watch runs the delivery loop for its own life; a restarted Watch
// starts it again once the previous loop has returned, never two at once.
func TestWatchRunsOneDeliveryLoop(t *testing.T) {
	sink := &recorderSink{}
	w, _ := connector.NewStagingWriter(t.TempDir(), "powerschool")
	c := newRecordsConnector(t, extraFixture("111111"), recordsConfig(), w, func() (RecordSink, error) { return sink, nil })
	ctx1, cancel1 := context.WithCancel(context.Background())
	c.startRecords(ctx1)
	cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	c.startRecords(ctx2)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		runs := sink.runs
		sink.mu.Unlock()
		if runs == 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the second Watch did not start the loop")
}

// The configured settling time reaches the attendance_day builder: Monday
// has an unmarked cell and only Tuesday follows it on the grid, so it is
// settled after one school day but not after three.
func TestEmitUsesTheConfiguredSettleTime(t *testing.T) {
	for _, tc := range []struct {
		settle int
		want   int // attendance_day snapshots, one per kid when Monday settles
	}{{1, 2}, {3, 0}} {
		client := extraFixture("111111")
		client.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
			p := gridPage([]time.Time{mon, tue}, []string{"UA", ""}, []string{".", ""})
			p.Student = pageStudentOf(client.Selected)
			return p, nil
		}
		cfg := recordsConfig()
		cfg.Records.AttendanceDaySettleSchoolDays = tc.settle
		sink := &recorderSink{}
		pollWithSink(t, client, cfg, sink)
		days := snapsOfType(sink.all(), TypeAttendanceDay)
		if len(days) != tc.want {
			t.Fatalf("settle %d: %d attendance_day snapshots, want %d", tc.settle, len(days), tc.want)
		}
		for _, s := range days {
			wantRange(t, s, "2026-10-05", "2026-10-06")
		}
	}
}

type panicSink struct{ calls int }

func (p *panicSink) Emit([]journalapi.Snapshot) error { p.calls++; panic("synthetic records failure") }
func (p *panicSink) OnCut()                           {}
func (p *panicSink) Run(ctx context.Context)          { <-ctx.Done() }
