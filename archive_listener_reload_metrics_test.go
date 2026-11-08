package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/leftathome/glovebox/internal/ingest/auth"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// glovebox-l9zp: the archive listener's token reloads must be wired to the
// reload metrics, including the boot-time load that mounts the 503 fallback.
func TestBootstrapArchives_RecordsTokenReloadMetrics(t *testing.T) {
	prev := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	stagingRoot := t.TempDir()
	for _, d := range []string{"archives", ".tmp-archives"} {
		if err := os.MkdirAll(filepath.Join(stagingRoot, d), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failing := deniedVault{err: fmt.Errorf("simulated vault 403")}
	src := auth.NewVaultTokenSource(failing, "secret", "glovebox/ingest-tokens")
	if err := bootstrapArchivesWithSource(ctx, makeArchiveTestConfig(stagingRoot), http.NewServeMux(), src, nil); err != nil {
		t.Fatalf("bootstrapArchivesWithSource: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := map[string]int64{"failure": 1, "success": 0}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "glovebox_ingest_token_reload_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("reload_total is %T, want Sum[int64]", m.Data)
			}
			for _, p := range sum.DataPoints {
				if l, _ := p.Attributes.Value(attribute.Key("listener")); l.AsString() != "archive" {
					continue
				}
				r, _ := p.Attributes.Value(attribute.Key("result"))
				got[r.AsString()] = p.Value
			}
		}
	}
	for result, v := range want {
		if g, ok := got[result]; !ok || g != v {
			t.Errorf("reload_total{listener=archive,result=%s} = %v (present=%v), want %d", result, g, ok, v)
		}
	}
}

// deniedVault fails every call. Local to this test so it does not depend on
// the production failingVaultClient sentinel (removed by glovebox-jzli).
type deniedVault struct{ err error }

func (d deniedVault) ListPath(context.Context, string, string) ([]string, error) { return nil, d.err }

func (d deniedVault) ReadKV(context.Context, string, string) (map[string]any, error) {
	return nil, d.err
}
