package powerschool

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// emitterWait bounds the waits for the records emitter to deliver or drain
// the spool: real HTTP plus a spool delete with a directory fsync per answer,
// sent back to back (glovebox-062i; connector/records uses the same bound,
// asyncWait, since dcbb003). The bound costs nothing when the test passes --
// each wait ends the moment its condition holds -- and only decides how long
// a stuck emitter takes to fail. The schoology copy of this test failed in CI
// while image builds saturated the node's disk; this one fails the same way
// with every fsync delayed by 1.5s. Waits on the fake sink in
// records_emit_test.go do no I/O and stay short.
const emitterWait = 2 * time.Minute

// recentConfigOnRecordRules is recentConfig on the record rules, records
// off: the baseline the "on" runs are compared with.
func recentConfigOnRecordRules() Config {
	cfg := recentConfig()
	cfg.Rules = recordRules()
	return cfg
}

// itemView is what a staged item is apart from its id and its times.
type itemView struct {
	Meta map[string]any
	Body string
}

func viewOf(it stagedItem) itemView {
	raw, _ := json.Marshal(it.Meta)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for k := range m {
		if k == "item_id" || k == "id" || strings.Contains(k, "time") || strings.HasSuffix(k, "_at") {
			delete(m, k)
		}
	}
	return itemView{Meta: m, Body: it.Body}
}

// diffItems compares two polls' items by metadata (without ids and
// times) and body; "" when they are the same.
func diffItems(a, b []stagedItem) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d items vs %d", len(a), len(b))
	}
	// Sorted by their whole content: two kids' items can share a subject.
	views := func(items []stagedItem) []string {
		var out []string
		for _, it := range items {
			raw, _ := json.Marshal(viewOf(it))
			out = append(out, string(raw))
		}
		sort.Strings(out)
		return out
	}
	va, vb := views(a), views(b)
	for i := range va {
		if va[i] != vb[i] {
			return fmt.Sprintf("item %d: %s\nvs %s", i, va[i], vb[i])
		}
	}
	return ""
}

// diffCheckpoints compares two checkpoints key by key, ignoring the poll
// time.
func diffCheckpoints(t *testing.T, a, b connector.Checkpoint) string {
	t.Helper()
	ka, kb := checkpointMap(t, a), checkpointMap(t, b)
	if !reflect.DeepEqual(ka, kb) {
		return fmt.Sprintf("%v\nvs %v", ka, kb)
	}
	return ""
}

func checkpointMap(t *testing.T, cp connector.Checkpoint) map[string]string {
	t.Helper()
	m, ok := cp.(*memCheckpoint)
	if !ok {
		t.Fatalf("checkpoint %T is not a memCheckpoint", cp)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.m {
		// Records' own state (decision S16) is not the items' checkpoint.
		if k == lastPollKey || strings.HasPrefix(k, "meta:last_read:") || strings.HasPrefix(k, "meta:records_") {
			continue
		}
		out[k] = v
	}
	return out
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// recordsRun is one poll with the full fixture and, when on, a real
// records emitter against a fake journal.
type recordsRun struct {
	c        *PowerSchoolConnector
	reader   *sdkmetric.ManualReader // the records meter
	tel      *metricRecorder         // the connector's powerschool_* metrics
	logs     *syncBuffer
	items    []stagedItem
	cp       connector.Checkpoint
	journal  *recordstest.Journal
	stateDir string
}

func pollOnceWithFixtures(t *testing.T, stateDir string, on bool) *recordsRun {
	t.Helper()
	run := &recordsRun{stateDir: stateDir}
	cfg := recentConfigOnRecordRules()
	if on {
		run.journal = recordstest.New(t, "journal-token")
		run.journal.Registry = recordsRegistry(t)
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("journal-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Records = PowerSchoolRecords{Config: recordsBlock(run.journal.URL)}
		cfg.Records.TokenFile = tokenFile
		ApplyDefaults(&cfg)
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	c, err := NewConnector(extraFixture("111111"), cfg, "v0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return pollTime }
	// The spool's and the journal's clocks a minute after the read, so
	// max_age and the future-skew check see the fixture's time, not today.
	later := func() time.Time { return pollTime.Add(time.Minute) }
	if run.journal != nil {
		run.journal.Now = later
	}
	// The records meter is read back through its own reader: the
	// framework's /metrics shares the process's default registry between
	// every test's providers.
	run.reader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(run.reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if factory := NewRecordsFactory(cfg.Records, stateDir, later); factory != nil {
		c.NewRecords = func(metric.Meter) (RecordSink, error) { return factory(provider.Meter("glovebox.records")) }
	}
	run.logs = captureLogs(t)
	metrics, err := connector.NewMetrics("powerschool-records-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	cp := newMemCheckpoint()
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "powerschool")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wire(connector.ConnectorContext{Backend: w, Matcher: connector.NewRuleMatcher(cfg.Rules), Metrics: metrics}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	run.tel = recordMetrics(t, c)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	run.c, run.cp, run.items = c, cp, readItems(t, stagingDir)
	return run
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

// With records off nothing new exists: no spool directory, no metric, no
// log line, no goroutine, and the staged items and the checkpoint are what
// they were.
func TestRecordsOffChangesNothing(t *testing.T) {
	stateDir := t.TempDir()
	before := runtime.NumGoroutine()
	run := pollOnceWithFixtures(t, stateDir, false)
	if run.c.NewRecords != nil || run.c.records != nil {
		t.Fatal("records sink built with records off")
	}
	if fileExists(filepath.Join(stateDir, "spool")) {
		t.Fatal("spool created with records off")
	}
	if m := run.recordsMetrics(t); len(m) != 0 {
		t.Fatalf("records metrics with records off: %v", m)
	}
	for k := range run.tel.counters() {
		if strings.HasPrefix(k, "powerschool_record_") {
			t.Fatalf("records metric %s with records off", k)
		}
	}
	if strings.Contains(strings.ToLower(run.logs.String()), "record") {
		t.Fatalf("records log line with records off:\n%s", run.logs.String())
	}
	// No records state in the checkpoint (carried attendance days, S16).
	cp := run.cp.(*memCheckpoint)
	cp.mu.Lock()
	for k := range cp.m {
		if strings.HasPrefix(k, "meta:records_") {
			t.Errorf("records state %q written with records off", k)
		}
	}
	cp.mu.Unlock()
	// Watch starts nothing extra.
	ctx, cancel := context.WithCancel(context.Background())
	run.c.startRecords(ctx)
	cancel()
	if runtime.NumGoroutine() > before+2 {
		t.Fatalf("goroutines %d -> %d", before, runtime.NumGoroutine())
	}
	if len(run.items) == 0 {
		t.Fatal("the fixture staged nothing")
	}
}

// The items staged with records on are identical to those with records
// off: shadow emission changes no item and no checkpoint. And what was
// spooled reaches the journal, valid and accepted.
func TestRecordsOnStagesTheSameItems(t *testing.T) {
	off := pollOnceWithFixtures(t, t.TempDir(), false)
	on := pollOnceWithFixtures(t, t.TempDir(), true)
	if diff := diffItems(off.items, on.items); diff != "" {
		t.Fatalf("records changed the items: %s", diff)
	}
	if diff := diffCheckpoints(t, off.cp, on.cp); diff != "" {
		t.Fatalf("records changed the checkpoint: %s", diff)
	}
	if !fileExists(filepath.Join(on.stateDir, "spool")) {
		t.Fatal("no spool with records on")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	on.c.startRecords(ctx)
	deadline := time.Now().Add(emitterWait)
	// 8 snapshots per kid: one per course surface, one per school year of
	// attendance (marks, days), the history, and two comment terms.
	for on.recordsMetrics(t)["glovebox_journal_spool_answers_total"] < 16 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	answers := on.journal.Answers()
	if len(answers) == 0 {
		t.Fatalf("nothing reached the journal; log:\n%s", on.logs.String())
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
	if len(types) != 7 {
		t.Fatalf("record types received %v", types)
	}
	if n := on.recordsMetrics(t)["glovebox_journal_spool_answers_total"]; n != int64(len(answers)) || n != 16 {
		t.Fatalf("spool answers counted %d, journal answered %d, want 16", n, len(answers))
	}
	if entries, _ := os.ReadDir(filepath.Join(on.stateDir, "spool")); len(entries) != 1 { // rejected/ only
		t.Fatalf("spool not drained: %d entries", len(entries))
	}
}

// A spool that cannot be written (the state volume broke after start)
// leaves the items staged and is counted.
func TestRecordsSpoolWriteFailureLeavesItems(t *testing.T) {
	stateDir := t.TempDir()
	off := pollOnceWithFixtures(t, t.TempDir(), false)
	run := pollOnceWithFixtures(t, stateDir, true)
	spool := filepath.Join(stateDir, "spool")
	if err := os.RemoveAll(spool); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spool, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second poll through the same connector, on a fresh checkpoint and
	// staging directory: everything is staged again.
	cp := newMemCheckpoint()
	stagingDir := t.TempDir()
	w, _ := connector.NewStagingWriter(stagingDir, "powerschool")
	run.c.wireMu.Lock()
	run.c.writer = w
	run.c.wireMu.Unlock()
	if err := run.c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if diff := diffItems(off.items, readItems(t, stagingDir)); diff != "" {
		t.Fatalf("items differ after a spool failure: %s", diff)
	}
	if n := run.recordsMetrics(t)["glovebox_journal_spool_write_errors_total"]; n != 16 {
		t.Fatalf("spool write errors counted %d, want 16 (8 snapshots per kid)", n)
	}
}
