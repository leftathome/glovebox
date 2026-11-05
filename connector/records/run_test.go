package records

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

type emitterRig struct {
	j      *recordstest.Journal
	clock  *fakeClock
	cfg    Config
	dir    string
	m      *testMetrics
	passed chan PassResult
}

func newEmitterRig(t *testing.T) *emitterRig {
	t.Helper()
	r := &emitterRig{clock: newClock(at(15, 0)), dir: t.TempDir(), m: newTestMetrics(t)}
	r.j = recordstest.New(t, "tok-1")
	r.j.Registry = testRegistry(t)
	r.j.Now = r.clock.Now
	tok := filepath.Join(t.TempDir(), "token")
	mustWrite(t, tok, "tok-1\n")
	r.cfg = Config{Enabled: true, JournalURL: r.j.URL, TokenFile: tok}
	return r
}

// emitter builds an Emitter on the rig's directory (or on mfs).
func (r *emitterRig) emitter(t *testing.T, mfs fsys, transport http.RoundTripper) *Emitter {
	t.Helper()
	r.passed = make(chan PassResult, 16)
	e, err := New(r.cfg, Options{StateDir: r.dir, Connector: "test", Log: quietLog(), Now: r.clock.Now,
		fs: mfs, transport: transport, passed: r.passed})
	if err != nil {
		t.Fatal(err)
	}
	e.m = r.m.Metrics
	e.spool.m, e.sender.m = r.m.Metrics, r.m.Metrics
	e.sender.jitter = func() float64 { return 0 }
	return e
}

// asyncWait bounds every wait in these tests for something the Run loop
// does on its own goroutine: a drain pass, Run returning after cancel, its
// goroutines exiting. Each wait ends as soon as the event happens, so the
// bound costs nothing when the test passes; it only decides how long a
// genuinely stuck loop takes to fail. It is generous on purpose: a pass is
// real file I/O (fsyncs) and real HTTP under the race detector, and with
// `go test -race -p 4 ./...` on a CPU-starved CI pod a first pass took
// longer than the 5s these waits used to allow (TestRestartReplaysInOrder
// failed "no drain pass" on a tag pipeline). Well inside go test's
// default 10m timeout, so a stuck loop still fails here, by name.
const asyncWait = 2 * time.Minute

// start runs e in the background; the returned stop waits for Run to
// return.
func start(t *testing.T, e *Emitter) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	var once atomic.Bool
	stop = func() {
		if once.Swap(true) {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(asyncWait):
			t.Fatal("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

// nextPass waits for the loop's next pass.
func (r *emitterRig) nextPass(t *testing.T) PassResult {
	t.Helper()
	select {
	case res := <-r.passed:
		return res
	case <-time.After(asyncWait):
		t.Fatal("no drain pass")
	}
	return PassResult{}
}

// countingMeter counts instrument registrations.
type countingMeter struct {
	noop.Meter
	n *atomic.Int32
}

func (c countingMeter) Int64Counter(name string, o ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	c.n.Add(1)
	return c.Meter.Int64Counter(name, o...)
}
func (c countingMeter) Int64Gauge(name string, o ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	c.n.Add(1)
	return c.Meter.Int64Gauge(name, o...)
}
func (c countingMeter) Float64Gauge(name string, o ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	c.n.Add(1)
	return c.Meter.Float64Gauge(name, o...)
}

// Off by default: a disabled config creates nothing at all.
func TestDisabledCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	var instruments atomic.Int32
	lb, log := newLogBuffer()
	before := runtime.NumGoroutine()
	for _, cfg := range []Config{{}, {Enabled: false, JournalURL: "ftp://junk", TokenFile: "rel", Spool: SpoolConfig{MaxBytes: 1}}} {
		e, err := New(cfg, Options{StateDir: dir, Connector: "test", Meter: countingMeter{n: &instruments}, Log: log})
		if err != nil || e != nil {
			t.Fatalf("disabled: emitter %v err %v", e, err)
		}
		if err := e.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 0))}); err != nil {
			t.Fatal(err)
		}
		e.OnCut()
		e.Run(context.Background()) // returns at once
	}
	if des, _ := os.ReadDir(dir); len(des) != 0 {
		t.Fatalf("state dir has %d entries", len(des))
	}
	if instruments.Load() != 0 {
		t.Fatalf("%d instruments registered", instruments.Load())
	}
	if lb.String() != "" {
		t.Fatalf("logged: %s", lb.String())
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
	// The same options with the feature on do create all of it.
	tok := filepath.Join(t.TempDir(), "token")
	mustWrite(t, tok, "t")
	e, err := New(Config{Enabled: true, JournalURL: "http://j:9095", TokenFile: tok},
		Options{StateDir: dir, Connector: "test", Meter: countingMeter{n: &instruments}, Log: log})
	if err != nil || e == nil {
		t.Fatal(err)
	}
	if !fileExists(rejectedDir(dir)) || instruments.Load() != 8 {
		t.Fatalf("enabled: spool %v instruments %d", fileExists(rejectedDir(dir)), instruments.Load())
	}
}

func TestNewRefusesAnInvalidEnabledConfig(t *testing.T) {
	if _, err := New(Config{Enabled: true, JournalURL: "ftp://j", TokenFile: "/t"}, Options{StateDir: t.TempDir()}); err == nil {
		t.Fatal("bad url accepted")
	}
	if _, err := New(Config{Enabled: true, JournalURL: "http://j", TokenFile: "/t"}, Options{}); err == nil {
		t.Fatal("empty state dir accepted")
	}
}

func TestRunDeliversWhatEmitWrites(t *testing.T) {
	r := newEmitterRig(t)
	e := r.emitter(t, nil, nil)
	start(t, e)
	r.nextPass(t) // the first pass, at once
	if err := e.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 0)), testSnapshot(t, "e_child1", "c2", at(13, 1))}); err != nil {
		t.Fatal(err)
	}
	res := r.nextPass(t)
	if res.Sent != 2 || res.Remaining != 0 || len(r.j.Received()) != 2 {
		t.Fatalf("%+v received %d", res, len(r.j.Received()))
	}
	if n := listNames(t, spoolDir(r.dir)); len(n) != 0 {
		t.Fatalf("spool %v", n)
	}
}

func TestRunRetriesAfterBackoff(t *testing.T) {
	r := newEmitterRig(t)
	e := r.emitter(t, nil, nil)
	r.j.SetReady(false)
	_ = e.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 0))})
	start(t, e)
	res := r.nextPass(t)
	if res.Sent != 0 || !res.NextDue.Equal(r.clock.Now().Add(30*time.Second)) {
		t.Fatalf("%+v", res)
	}
	r.j.SetReady(true)
	e.wake() // too early: nothing is sent
	if res := r.nextPass(t); res.Sent != 0 || len(r.j.Answers()) != 1 {
		t.Fatalf("sent during backoff: %+v", res)
	}
	r.clock.Advance(30 * time.Second)
	e.wake()
	if res := r.nextPass(t); res.Sent != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	r := newEmitterRig(t)
	e := r.emitter(t, nil, nil)
	before := runtime.NumGoroutine()
	stop := start(t, e)
	r.nextPass(t)
	stop()
	r.j.CloseClientConnections()
	deadline := time.Now().Add(asyncWait)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d after Run returned", before, after)
	}
}

func TestEmitFailureIsReportedNotFatal(t *testing.T) {
	r := newEmitterRig(t)
	clock := r.clock
	mfs := newMemFS(clock.Now)
	r.dir = "/state"
	e := r.emitter(t, mfs, nil)
	start(t, e)
	r.nextPass(t)
	mfs.setFailOnce(1) // the temp file cannot be created
	err := e.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 0))})
	if err == nil || strings.Contains(err.Error(), "/state") {
		t.Fatalf("err %v", err)
	}
	if r.m.counter(t, metricWriteErrors) != 1 {
		t.Fatal("not counted")
	}
	r.nextPass(t)
	if err := e.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 1))}); err != nil {
		t.Fatal(err)
	}
	if res := r.nextPass(t); res.Sent != 1 {
		t.Fatalf("%+v", res)
	}
}

type panicOnce struct {
	done atomic.Bool
	base http.RoundTripper
}

func (p *panicOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	if !p.done.Swap(true) {
		panic("transport bug")
	}
	return p.base.RoundTrip(req)
}

func TestRunRecoversFromAPanickingPass(t *testing.T) {
	r := newEmitterRig(t)
	e := r.emitter(t, nil, &panicOnce{base: http.DefaultTransport})
	_ = e.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 0))})
	start(t, e)
	res := r.nextPass(t)
	if !res.NextDue.Equal(r.clock.Now().Add(30 * time.Second)) {
		t.Fatalf("%+v", res)
	}
	r.clock.Advance(30 * time.Second)
	e.wake()
	if res := r.nextPass(t); res.Sent != 1 {
		t.Fatalf("the loop did not survive the panic: %+v", res)
	}
}

func TestOnCutCounts(t *testing.T) {
	r := newEmitterRig(t)
	e := r.emitter(t, nil, nil)
	NewFields(e.OnCut).Text("t", strings.Repeat("a", 70<<10))
	if r.m.counter(t, metricCut) != 1 {
		t.Fatal("not counted")
	}
}
