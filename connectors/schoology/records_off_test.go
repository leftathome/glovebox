package schoology

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	schoologylib "github.com/leftathome/schoology-go"
)

// recordsPollTime is the fixed clock of the connector-level record tests.
var recordsPollTime = time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC)

// recordsFixture is a fake Schoology for two kids: k1 (uid 1001) lists two
// assignments, k2 (uid 1002) one; every listed assignment has a clean
// detail page. Tests change the maps between polls.
type recordsFixture struct {
	mu       sync.Mutex
	rows     map[int64][]*schoologylib.Assignment
	details  map[int64]*schoologylib.AssignmentDetail
	listErr  map[int64]error
	feedErr  map[int64]error
	comments map[int64]*schoologylib.AssignmentComments
}

func newRecordsFixture() *recordsFixture {
	f := &recordsFixture{
		rows: map[int64][]*schoologylib.Assignment{
			1001: {listRow(9100000011, "EXAMPLE Lab Report", "EXAMPLE SCIENCE 7"), listRow(9100000012, "EXAMPLE Essay Draft", "EXAMPLE ENGLISH 7")},
			1002: {listRow(9100000021, "EXAMPLE Map Quiz", "EXAMPLE HISTORY 7")},
		},
		details: map[int64]*schoologylib.AssignmentDetail{},
		listErr: map[int64]error{}, feedErr: map[int64]error{},
		comments: map[int64]*schoologylib.AssignmentComments{},
	}
	f.rows[1001][1].Status = schoologylib.AssignmentStatusOverdue
	for _, rows := range f.rows {
		for _, r := range rows {
			f.details[r.ID] = regularDetail(r.ID, r.Title, r.CourseTitle)
		}
	}
	return f
}

func (f *recordsFixture) client() *fakeClient {
	return &fakeClient{
		OverdueSubmissionsFunc: func(_ context.Context, uid int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.listErr[uid]; err != nil {
				return nil, nil, err
			}
			var out []*schoologylib.Assignment
			for _, r := range f.rows[uid] {
				cp := *r
				out = append(out, &cp)
			}
			return out, nil, nil
		},
		FeedFunc: func(_ context.Context, uid int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return nil, nil, f.feedErr[uid]
		},
		InboxFunc: func(context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		AssignmentFunc: func(_ context.Context, id int64) (*schoologylib.AssignmentDetail, schoologylib.ParseErrors, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			d, ok := f.details[id]
			if !ok {
				return nil, nil, schoologylib.ErrNotFound
			}
			cp := *d
			if c, ok := f.comments[id]; ok {
				cp.Comments = *c
			}
			return &cp, nil, nil
		},
		CommentsFunc: func(context.Context, int64) (*schoologylib.AssignmentComments, error) {
			return &schoologylib.AssignmentComments{State: schoologylib.CommentsStateEmpty}, nil
		},
		ValidateFunc: func(context.Context) error { return nil },
	}
}

// recordsRun is one connector with its staging, checkpoint and, when on, a
// records sink: a real emitter against a fake journal, or a test's own.
type recordsRun struct {
	c          *SchoologyConnector
	fixture    *recordsFixture
	reader     *sdkmetric.ManualReader // the records meter
	logs       *syncBuffer
	stagingDir string
	cp         *memCheckpoint
	journal    *recordstest.Journal
	stateDir   string
	now        time.Time
}

// newRecordsRun builds the connector. on enables records in the config
// (and the real emitter unless sink is set); sink, when set, is the sink.
func newRecordsRun(t *testing.T, stateDir string, on bool, sink RecordSink) *recordsRun {
	t.Helper()
	run := &recordsRun{stateDir: stateDir, fixture: newRecordsFixture(), now: recordsPollTime}
	cfg := recordsConfig(t)
	cfg.Records = records.Config{}
	if on {
		run.journal = recordstest.New(t, "journal-token")
		run.journal.Registry = recordsRegistry(t)
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("journal-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Records = recordsBlock(run.journal.URL)
		cfg.Records.TokenFile = tokenFile
		ApplyDefaults(&cfg)
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	c, err := NewConnector(run.fixture.client(), cfg, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return run.now }
	c.sleep = func(context.Context, time.Duration) error { return nil }
	// The spool's and the journal's clocks a minute after the read.
	later := func() time.Time { return run.now.Add(time.Minute) }
	if run.journal != nil {
		run.journal.Now = later
	}
	run.reader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(run.reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	switch {
	case sink != nil:
		c.NewRecords = func(metric.Meter) (RecordSink, error) { return sink, nil }
	default:
		if factory := NewRecordsFactory(cfg.Records, stateDir, later); factory != nil {
			c.NewRecords = func(metric.Meter) (RecordSink, error) { return factory(provider.Meter("glovebox.records")) }
		}
	}
	run.logs = captureLogs(t)
	metrics, err := connector.NewMetrics("schoology-records-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	run.cp = newMemCheckpoint()
	run.stagingDir = t.TempDir()
	w, err := connector.NewStagingWriter(run.stagingDir, "schoology")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wire(connector.ConnectorContext{Backend: w, Matcher: connector.NewRuleMatcher(cfg.Rules), Metrics: metrics}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	run.c = c
	return run
}

// poll runs one scheduled poll and returns its error.
func (r *recordsRun) poll(t *testing.T) error {
	t.Helper()
	return r.c.pollNow(context.Background(), r.cp, "scheduled", -1)
}

func (r *recordsRun) items(t *testing.T) []stagedItem {
	t.Helper()
	return readStagedItems(t, r.stagingDir)
}

// recordsMetrics returns the records meter's int64 sums by name.
func (r *recordsRun) recordsMetrics(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] += 0
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					out[m.Name] += dp.Value
				}
			}
		}
	}
	return out
}

// diffItems compares two runs' items by metadata (without the staging
// timestamp) and body; "" when they are the same.
func diffItems(a, b []stagedItem) string {
	va, vb := comparableItems(a), comparableItems(b)
	if len(va) != len(vb) {
		return fmt.Sprintf("%d items vs %d", len(va), len(vb))
	}
	for i := range va {
		if va[i] != vb[i] {
			return fmt.Sprintf("item %d:\n%s\nvs\n%s", i, va[i], vb[i])
		}
	}
	return ""
}

// memCheckpoint is an in-memory Checkpoint whose keys a test can list.
type memCheckpoint struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemCheckpoint() *memCheckpoint { return &memCheckpoint{m: map[string]string{}} }

func (c *memCheckpoint) Load(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *memCheckpoint) Save(k, v string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
	return nil
}

func (c *memCheckpoint) Delete(k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
	return nil
}

func (c *memCheckpoint) snapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs routes the process-wide slog default to a buffer for the
// duration of the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// With records off nothing new exists: no sink, no spool directory, no
// records metric, no log line, no goroutine.
func TestRecordsOffChangesNothing(t *testing.T) {
	stateDir := t.TempDir()
	before := runtime.NumGoroutine()
	run := newRecordsRun(t, stateDir, false, nil)
	for i := 0; i < 2; i++ {
		if err := run.poll(t); err != nil {
			t.Fatal(err)
		}
	}
	if run.c.NewRecords != nil || run.c.records != nil {
		t.Fatal("records sink built with records off")
	}
	if fileExists(filepath.Join(stateDir, "spool")) {
		t.Fatal("spool created with records off")
	}
	if m := run.recordsMetrics(t); len(m) != 0 {
		t.Fatalf("records metrics with records off: %v", m)
	}
	if logs := strings.ToLower(run.logs.String()); strings.Contains(logs, "records") || strings.Contains(logs, "journal") {
		t.Fatalf("records log line with records off:\n%s", run.logs.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.c.startRecords(ctx)
	cancel()
	if runtime.NumGoroutine() > before+2 {
		t.Fatalf("goroutines %d -> %d", before, runtime.NumGoroutine())
	}
	if len(run.items(t)) != 6 { // 3 assignments, 3 details
		t.Fatalf("the fixture staged %d items, want 6", len(run.items(t)))
	}
}

// The items staged with records on are identical to those with records off
// (shadow emission changes no item and no checkpoint, over two polls with a
// status change in between), and what was spooled reaches the journal,
// valid and applied.
func TestRecordsOnStagesTheSameItems(t *testing.T) {
	off := newRecordsRun(t, t.TempDir(), false, nil)
	on := newRecordsRun(t, t.TempDir(), true, nil)
	for _, r := range []*recordsRun{off, on} {
		if err := r.poll(t); err != nil {
			t.Fatal(err)
		}
		r.fixture.rows[1001][0].Status = schoologylib.AssignmentStatusOverdue
		r.now = r.now.Add(21 * time.Hour)
		if err := r.poll(t); err != nil {
			t.Fatal(err)
		}
	}
	if diff := diffItems(off.items(t), on.items(t)); diff != "" {
		t.Fatalf("records changed the items: %s", diff)
	}
	if a, b := off.cp.snapshot(), on.cp.snapshot(); !reflect.DeepEqual(a, b) {
		t.Fatalf("records changed the checkpoint:\n%v\nvs\n%v", a, b)
	}
	if !fileExists(filepath.Join(on.stateDir, "spool")) {
		t.Fatal("no spool with records on")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	on.c.startRecords(ctx)
	// Per poll: a listing and a detail snapshot per kid.
	const want = 8
	deadline := time.Now().Add(10 * time.Second)
	for on.recordsMetrics(t)["glovebox_journal_spool_answers_total"] < want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	answers := on.journal.Answers()
	if len(answers) != want {
		t.Fatalf("%d answers, want %d; log:\n%s", len(answers), want, on.logs.String())
	}
	for _, a := range answers {
		if a.Code != 200 || a.Status != journalapi.StatusApplied {
			t.Fatalf("journal answered %+v", a)
		}
	}
	types := map[string]int{}
	for _, s := range on.journal.Received() {
		types[s.RecordType]++
	}
	if types[TypeLMSAssignment] != 4 || types[TypeLMSAssignmentDetail] != 4 {
		t.Fatalf("record types received %v", types)
	}
	if entries, _ := os.ReadDir(filepath.Join(on.stateDir, "spool")); len(entries) != 1 { // rejected/ only
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		t.Fatalf("spool not drained: %v", names)
	}
	raw, _ := json.Marshal(on.journal.Received())
	if strings.Contains(string(raw), "e_k1") || strings.Contains(on.logs.String(), "journal-token") {
		t.Fatal("a record carries an item-fixture subject, or the token was logged")
	}
}
