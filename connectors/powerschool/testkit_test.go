package powerschool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/leftathome/glovebox/connector"
)

// failingBackend is a StagingBackend whose NewItem fails while Fail is set
// and otherwise delegates to a real backend. It models the ingest endpoint
// (or the staging volume) being unavailable.
type failingBackend struct {
	connector.StagingBackend
	Fail     bool
	Attempts int
}

var errBackendDown = errors.New("synthetic staging backend failure")

func (b *failingBackend) NewItem(opts connector.ItemOptions) (*connector.StagingItem, error) {
	b.Attempts++
	if b.Fail {
		return nil, errBackendDown
	}
	return b.StagingBackend.NewItem(opts)
}

// syncBuffer is a bytes.Buffer safe to write from the code under test while
// the test reads it.
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
// duration of the test and returns it. Everything the package logs --
// every level -- lands there as text.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// metricRecorder reads back what a Telemetry recorded.
type metricRecorder struct {
	t      *testing.T
	reader *sdkmetric.ManualReader
}

// newRecordingTelemetry builds a Telemetry whose powerschool_* instruments
// are registered on a private provider with a manual reader.
func newRecordingTelemetry(t *testing.T) (*Telemetry, *metricRecorder) {
	t.Helper()
	base, err := connector.NewMetrics("powerschool-recording-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = base.Shutdown() })

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tel, err := newTelemetry(base, provider.Meter("powerschool-recording-test"), otel.Tracer("powerschool-recording-test"))
	if err != nil {
		t.Fatalf("newTelemetry: %v", err)
	}
	return tel, &metricRecorder{t: t, reader: reader}
}

// counters returns every recorded counter data point as
// "name{k=v,k=v}" -> value, attributes in key order.
func (r *metricRecorder) counters() map[string]int64 {
	r.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		r.t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				var attrs []string
				for _, kv := range dp.Attributes.ToSlice() {
					attrs = append(attrs, fmt.Sprintf("%s=%s", kv.Key, kv.Value.Emit()))
				}
				sort.Strings(attrs)
				out[m.Name+"{"+strings.Join(attrs, ",")+"}"] += dp.Value
			}
		}
	}
	return out
}

// want asserts the recorded counters are exactly want.
func (r *metricRecorder) want(want map[string]int64) {
	r.t.Helper()
	got := r.counters()
	for k, v := range want {
		if got[k] != v {
			r.t.Errorf("metric %s = %d, want %d", k, got[k], v)
		}
	}
	for k, v := range got {
		if _, expected := want[k]; !expected {
			r.t.Errorf("unexpected metric %s = %d", k, v)
		}
	}
}
