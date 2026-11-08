package archives

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// withManualReader installs a ManualReader-backed global MeterProvider for
// the test. The cleanup re-installs the previous provider, but otel's
// default global delegates permanently to the first provider set, so this
// is not a full restore; nothing in this package reads the default after.
func withManualReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	prev := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

// point finds the data point of metric name whose attributes include every
// key/value in want. It returns the value as float64 and whether it exists.
func point(t *testing.T, reader *sdkmetric.ManualReader, name string, want map[string]string) (float64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	match := func(set attribute.Set) bool {
		for k, v := range want {
			got, ok := set.Value(attribute.Key(k))
			if !ok || got.AsString() != v {
				return false
			}
		}
		return true
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					if match(p.Attributes) {
						return float64(p.Value), true
					}
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					if match(p.Attributes) {
						return float64(p.Value), true
					}
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					if match(p.Attributes) {
						return p.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

// glovebox-l9zp: the Vault token reload failed every 5 minutes for 2+ weeks
// with only a WARN log. Reload outcomes must be visible on /metrics.
func TestRecordTokenReload(t *testing.T) {
	reader := withManualReader(t)
	tel, err := NewTelemetry("glovebox-test", "glovebox-test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	ctx := context.Background()

	tel.RecordTokenReload(ctx, "sanitize", errors.New("403 invalid token"), 0)

	if v, ok := point(t, reader, "glovebox_ingest_token_reload_total", map[string]string{"listener": "sanitize", "result": "failure"}); !ok || v != 1 {
		t.Errorf("reload_total{sanitize,failure} = %v (present=%v), want 1", v, ok)
	}
	if _, ok := point(t, reader, "glovebox_ingest_token_reload_last_success_timestamp_seconds", map[string]string{"listener": "sanitize"}); ok {
		t.Error("last_success_timestamp set by a failed reload")
	}
	// Review of l9zp: a listener that fails once and never reloads again
	// (the archive listener after a failed boot load) must still leave a
	// success series behind, at 0, so "failed and never succeeded" is
	// expressible without relying on absent().
	if v, ok := point(t, reader, "glovebox_ingest_token_reload_total", map[string]string{"listener": "sanitize", "result": "success"}); !ok || v != 0 {
		t.Errorf("reload_total{sanitize,success} after only a failure = %v (present=%v), want 0 and present", v, ok)
	}

	tel.RecordTokenReload(ctx, "sanitize", nil, 3)

	if v, ok := point(t, reader, "glovebox_ingest_token_reload_total", map[string]string{"listener": "sanitize", "result": "success"}); !ok || v != 1 {
		t.Errorf("reload_total{sanitize,success} = %v (present=%v), want 1", v, ok)
	}
	if v, ok := point(t, reader, "glovebox_ingest_token_reload_total", map[string]string{"listener": "archive", "result": "failure"}); ok {
		t.Errorf("reload_total{archive,failure} = %v appeared without any archive reload", v)
	}
	if v, ok := point(t, reader, "glovebox_ingest_tokens_loaded", map[string]string{"listener": "sanitize"}); !ok || v != 3 {
		t.Errorf("tokens_loaded{sanitize} = %v (present=%v), want 3", v, ok)
	}
	if v, ok := point(t, reader, "glovebox_ingest_token_reload_last_success_timestamp_seconds", map[string]string{"listener": "sanitize"}); !ok || v <= 0 {
		t.Errorf("last_success_timestamp{sanitize} = %v (present=%v), want > 0", v, ok)
	}
}

func TestRecordTokenReloadNilSafe(t *testing.T) {
	var tel *Telemetry
	tel.RecordTokenReload(context.Background(), "archive", nil, 1)
}

// A success records a failure series at 0 as well, so both results exist
// from the first reload whichever way it went.
func TestRecordTokenReloadCreatesBothResultSeries(t *testing.T) {
	reader := withManualReader(t)
	tel, err := NewTelemetry("glovebox-test", "glovebox-test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	tel.RecordTokenReload(context.Background(), "archive", nil, 2)
	if v, ok := point(t, reader, "glovebox_ingest_token_reload_total", map[string]string{"listener": "archive", "result": "failure"}); !ok || v != 0 {
		t.Errorf("reload_total{archive,failure} after only a success = %v (present=%v), want 0 and present", v, ok)
	}
}
