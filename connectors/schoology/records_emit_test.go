package schoology

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	schoologylib "github.com/leftathome/schoology-go"
)

// recorderSink records what the connector emits.
type recorderSink struct {
	mu    sync.Mutex
	snaps []journalapi.Snapshot
	calls int
	err   error
	panic bool
}

func (s *recorderSink) Emit(snaps []journalapi.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.panic {
		panic("EXAMPLE sink failure")
	}
	s.snaps = append(s.snaps, snaps...)
	return s.err
}
func (s *recorderSink) OnCut()              {}
func (s *recorderSink) Run(context.Context) {}

func (s *recorderSink) all() []journalapi.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]journalapi.Snapshot(nil), s.snaps...)
}

// bySubjectAndType indexes snapshots by data_subject and record type.
func bySubjectAndType(t *testing.T, snaps []journalapi.Snapshot) map[string]map[string]journalapi.Snapshot {
	t.Helper()
	out := map[string]map[string]journalapi.Snapshot{}
	for _, s := range snaps {
		subj := s.Scope[2][1]
		if out[subj] == nil {
			out[subj] = map[string]journalapi.Snapshot{}
		}
		if _, dup := out[subj][s.RecordType]; dup {
			t.Fatalf("two %s snapshots for %s in one poll", s.RecordType, subj)
		}
		out[subj][s.RecordType] = s
	}
	return out
}

func TestEmitEveryKidsSnapshotsWithItsOwnSubject(t *testing.T) {
	sink := &recorderSink{}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	if err := run.poll(t); err != nil {
		t.Fatal(err)
	}
	snaps := sink.all()
	validateAll(t, snaps)
	idx := bySubjectAndType(t, snaps)
	if len(snaps) != 4 || len(idx) != 2 {
		t.Fatalf("%d snapshots for %d subjects, want 4 for 2", len(snaps), len(idx))
	}
	wantKeys := map[string][]string{"e_child1": {"9100000011", "9100000012"}, "e_child2": {"9100000021"}}
	for subj, keys := range wantKeys {
		for _, ty := range []string{TypeLMSAssignment, TypeLMSAssignmentDetail} {
			s, ok := idx[subj][ty]
			if !ok {
				t.Fatalf("no %s snapshot for %s", ty, subj)
			}
			if got := keysOfSnap(s); strings.Join(got, ",") != strings.Join(keys, ",") {
				t.Errorf("%s %s keys %v, want %v", subj, ty, got, keys)
			}
			for _, r := range s.Records {
				if r.DataSubject != subj {
					t.Errorf("%s %s record %s carries subject %s", subj, ty, r.Key, r.DataSubject)
				}
			}
			if !s.ObservedAt.Equal(recordsPollTime) {
				t.Errorf("%s %s observed_at %v, want the poll's clock", subj, ty, s.ObservedAt)
			}
		}
	}
}

// The listing's observed_at is the time taken just before the kid's listing
// was requested.
func TestEmitListingObservedAtIsBeforeTheListing(t *testing.T) {
	sink := &recorderSink{}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	base := run.now
	ticks := 0
	run.c.now = func() time.Time { ticks++; return base.Add(time.Duration(ticks) * time.Second) }
	listedAt := map[int64]time.Time{}
	inner := run.c.client.(*fakeClient).OverdueSubmissionsFunc
	run.c.client.(*fakeClient).OverdueSubmissionsFunc = func(ctx context.Context, uid int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
		listedAt[uid] = base.Add(time.Duration(ticks) * time.Second)
		return inner(ctx, uid)
	}
	if err := run.poll(t); err != nil {
		t.Fatal(err)
	}
	idx := bySubjectAndType(t, sink.all())
	for subj, uid := range map[string]int64{"e_child1": 1001, "e_child2": 1002} {
		if got := idx[subj][TypeLMSAssignment].ObservedAt; !got.Equal(listedAt[uid]) {
			t.Errorf("%s listing observed_at %v, want %v (the clock read before its listing)", subj, got, listedAt[uid])
		}
	}
}

// A kid whose listing failed has no records this poll; the other kid's are
// sent.
func TestEmitNothingForAKidWhoseListingFailed(t *testing.T) {
	sink := &recorderSink{}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	run.fixture.listErr[1001] = schoologylib.NewParseError("GetOverdueSubmissions", "EXAMPLE layout change")
	if err := run.poll(t); err != nil {
		t.Fatal(err)
	}
	idx := bySubjectAndType(t, sink.all())
	if _, ok := idx["e_child1"]; ok {
		t.Fatalf("records for k1 after its listing failed: %v", idx["e_child1"])
	}
	if len(idx["e_child2"]) != 2 {
		t.Fatalf("k2's records: %v", idx["e_child2"])
	}
}

// A session refused on another surface skips the detail surface: only the
// listings are records.
func TestEmitOnlyListingsWhenTheSessionWasRefused(t *testing.T) {
	sink := &recorderSink{}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	run.fixture.feedErr[1002] = schoologylib.ErrSessionExpired
	if err := run.poll(t); !connector.IsPermanent(err) {
		t.Fatalf("poll err = %v, want the session-expired exit", err)
	}
	for _, s := range sink.all() {
		if s.RecordType != TypeLMSAssignment {
			t.Fatalf("a %s snapshot with the session refused", s.RecordType)
		}
	}
	if len(sink.all()) != 2 {
		t.Fatalf("%d listing snapshots, want 2", len(sink.all()))
	}
}

// Without the kid's rule there are no records for the surface, as there
// are no items.
func TestEmitNothingWithoutTheKidsRule(t *testing.T) {
	sink := &recorderSink{}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	run.c.wireMu.Lock()
	run.c.matcher = connector.NewRuleMatcher(append(recordRules()[2:4:4], recordRules()[4]))
	run.c.wireMu.Unlock()
	if err := run.poll(t); err != nil {
		t.Fatal(err)
	}
	for _, s := range sink.all() {
		if s.Scope[2][1] != "e_child2" {
			t.Fatalf("a snapshot for %s without its rule", s.Scope[2][1])
		}
	}
}

// A sink that fails or panics changes nothing staged.
func TestEmitFailuresLeaveTheItems(t *testing.T) {
	off := newRecordsRun(t, t.TempDir(), false, nil)
	if err := off.poll(t); err != nil {
		t.Fatal(err)
	}
	for name, sink := range map[string]*recorderSink{
		"error": {err: errors.New("EXAMPLE spool full")},
		"panic": {panic: true},
	} {
		t.Run(name, func(t *testing.T) {
			run := newRecordsRun(t, t.TempDir(), true, sink)
			if err := run.poll(t); err != nil {
				t.Fatalf("poll: %v", err)
			}
			if sink.calls != 4 {
				t.Fatalf("%d Emit calls, want 4", sink.calls)
			}
			if diff := diffItems(off.items(t), run.items(t)); diff != "" {
				t.Fatalf("items differ: %s", diff)
			}
		})
	}
}

// A spool that cannot be written (the state volume broke after start)
// leaves the items staged and is counted.
func TestEmitSpoolWriteFailureLeavesItems(t *testing.T) {
	off := newRecordsRun(t, t.TempDir(), false, nil)
	if err := off.poll(t); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	run := newRecordsRun(t, stateDir, true, nil)
	spool := filepath.Join(stateDir, "spool")
	if err := os.RemoveAll(spool); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spool, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if diff := diffItems(off.items(t), run.items(t)); diff != "" {
		t.Fatalf("items differ after a spool failure: %s", diff)
	}
	if n := run.recordsMetrics(t)["glovebox_journal_spool_write_errors_total"]; n != 4 {
		t.Fatalf("spool write errors counted %d, want 4", n)
	}
}

func TestNewRecordsFactory(t *testing.T) {
	off := recordsBlock("http://j:9095")
	off.Enabled = false
	if NewRecordsFactory(off, t.TempDir(), nil) != nil {
		t.Fatal("a factory with records off")
	}
	if f := NewRecordsFactory(recordsBlock("http://j:9095"), "", nil); f == nil {
		t.Fatal("no factory with records on")
	} else if _, err := f(nil); err == nil {
		t.Fatal("an empty state directory was accepted")
	}
}

// A config that asked for records and cannot have them fails Wire, so the
// process stops at start rather than run without its records.
func TestWireFailsWithoutItsSink(t *testing.T) {
	c, err := NewConnector(&fakeClient{}, recordsConfig(t), "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	c.NewRecords = func(metric.Meter) (RecordSink, error) { return nil, errors.New("EXAMPLE spool cannot be opened") }
	metrics, err := connector.NewMetrics("schoology-records-wire")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	w, _ := newTestWriter(t)
	err = c.Wire(connector.ConnectorContext{Backend: w, Matcher: connector.NewRuleMatcher(recordRules()), Metrics: metrics})
	if err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("Wire err = %v, want the records failure", err)
	}
}

// runSink signals each Run and returns when its context ends.
type runSink struct {
	recorderSink
	started chan struct{}
}

func (s *runSink) Run(ctx context.Context) {
	s.started <- struct{}{}
	<-ctx.Done()
}

// Watch starts the delivery loop; a restarted Watch starts it again only
// after the previous loop has returned, so one runs at a time.
func TestWatchStartsOneRecordsLoop(t *testing.T) {
	sink := &runSink{started: make(chan struct{}, 4)}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run.c.Watch(ctx, run.cp) }()
		select {
		case <-sink.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("Watch %d did not start the records loop", i+1)
		}
		cancel()
		<-done
	}
	select {
	case <-sink.started:
		t.Fatal("an extra records loop was started")
	default:
	}
}
