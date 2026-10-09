package schoology

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/leftathome/glovebox/connector"
)

// exportedCounter returns the value of the counter `name` carrying label
// `label`=`value` in the registry the framework's /metrics handler serves
// (the default Prometheus registry), and whether it was found.
//
// It gathers from the registry rather than through the HTTP handler: other
// tests in this package leave their own exporters registered, the duplicate
// target_info series makes the handler answer 500, and Gather still returns
// every family it could collect alongside that error. A connector process
// has one exporter and no such conflict.
func exportedCounter(name, label, value string) (float64, bool) {
	families, _ := prometheus.DefaultGatherer.Gather()
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// TestTelemetry_ExportedOnTheMetricsEndpoint records schoology-specific
// counters and reads them back from the registry behind /metrics.
//
// The instruments used to be created with otel.Meter(), which resolves
// against the global meter provider. connector.Run never sets one, so in a
// connector process every schoology_* instrument was a no-op and none of
// them ever appeared on /metrics. They must be registered on the
// framework's own provider.
func TestTelemetry_ExportedOnTheMetricsEndpoint(t *testing.T) {
	base, err := connector.NewMetrics("schoology-export-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = base.Shutdown() })
	tel, err := NewTelemetry(base, "glovebox.schoology.exporttest", "glovebox.schoology.exporttest")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	tel.RecordPollByTrigger("exporttest")
	tel.RecordPollByTrigger("exporttest")
	tel.RecordTriggerRequest("exporttest")

	for _, tc := range []struct {
		name, label string
		want        float64
	}{
		{"schoology_polls_total", "trigger_source", 2},
		{"schoology_trigger_requests_total", "outcome", 1},
	} {
		got, ok := exportedCounter(tc.name, tc.label, "exporttest")
		if !ok {
			t.Errorf("%s{%s=\"exporttest\"} is not exported", tc.name, tc.label)
		} else if got != tc.want {
			t.Errorf("%s{%s=\"exporttest\"} = %v, want %v", tc.name, tc.label, got, tc.want)
		}
	}
}
