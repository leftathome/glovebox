package records

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// testRegistry has one source type "t1" (owner "src") with a string field
// "a" and a date field "d".
func testRegistry(t testing.TB) *record.Registry {
	t.Helper()
	reg, err := record.NewRegistry([]record.Type{{
		Name: "t1", SchemaVersion: 1, Owner: "src",
		Fields:          map[string]record.FieldDef{"a": {Type: record.String}, "d": {Type: record.Date}},
		DefaultAudience: []string{"guardians"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// at is 2026-10-16 h:m UTC.
func at(h, m int) time.Time { return time.Date(2026, 10, 16, h, m, 0, 0, time.UTC) }

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testSpoolConfig() SpoolConfig {
	c := Config{}
	c.ApplyDefaults()
	return c.Spool
}

// quietLog discards; logs are checked where they matter (privacy_test.go).
func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openTestSpool(t testing.TB, dir string) *Spool {
	t.Helper()
	sp, err := OpenSpool(dir, testSpoolConfig(), nil, quietLog(), fixedNow(at(15, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// testSnapshot is a one-record t1 snapshot of scope (src, s, subject,
// course=course).
func testSnapshot(t testing.TB, subject, course string, observed time.Time) journalapi.Snapshot {
	t.Helper()
	f := NewFields(nil)
	f.String("a", "x")
	f.Date("d", observed)
	return NewSnapshot("t1", 1, Scope("src", "s", subject, Part("course", course)), true, nil, observed,
		[]journalapi.Record{{Key: "k1", DataSubject: subject, Fields: f.Map()}})
}

func listNames(t testing.TB, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if !de.IsDir() {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

func mustWrite(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSnapshot(t testing.TB, path string) journalapi.Snapshot {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s journalapi.Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func spoolDir(stateDir string) string    { return filepath.Join(stateDir, spoolDirName) }
func rejectedDir(stateDir string) string { return filepath.Join(stateDir, spoolDirName, rejectedDirName) }

// testMetrics is a Metrics read back through a ManualReader.
type testMetrics struct {
	*Metrics
	reader *sdkmetric.ManualReader
}

func newTestMetrics(t testing.TB) *testMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider.Meter("glovebox.records"), "test")
	if err != nil {
		t.Fatal(err)
	}
	return &testMetrics{Metrics: m, reader: reader}
}

func (tm *testMetrics) collect(t testing.TB) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tm.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

// value sums the data points of name whose attributes include every
// key=value pair in kv.
func (tm *testMetrics) value(t testing.TB, name string, kv ...string) float64 {
	t.Helper()
	match := func(set attribute.Set) bool {
		for i := 0; i+1 < len(kv); i += 2 {
			v, ok := set.Value(attribute.Key(kv[i]))
			if !ok || v.Emit() != kv[i+1] {
				return false
			}
		}
		return true
	}
	total := 0.0
	for _, m := range tm.collect(t) {
		if m.Name != name {
			continue
		}
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, p := range d.DataPoints {
				if match(p.Attributes) {
					total += float64(p.Value)
				}
			}
		case metricdata.Gauge[int64]:
			for _, p := range d.DataPoints {
				if match(p.Attributes) {
					total += float64(p.Value)
				}
			}
		case metricdata.Gauge[float64]:
			for _, p := range d.DataPoints {
				if match(p.Attributes) {
					total += p.Value
				}
			}
		}
	}
	return total
}

// counter is value for the counters (the plan's helper name).
func (tm *testMetrics) counter(t testing.TB, name string, kv ...string) float64 {
	t.Helper()
	return tm.value(t, name, kv...)
}

// logBuffer captures log output for the privacy checks.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newLogBuffer() (*logBuffer, *slog.Logger) {
	lb := &logBuffer{}
	return lb, slog.New(slog.NewJSONHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
