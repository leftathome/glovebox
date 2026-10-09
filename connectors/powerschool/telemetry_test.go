package powerschool

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

func TestNewTelemetry_RequiresArguments(t *testing.T) {
	base, err := connector.NewMetrics("powerschool-test-args")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	defer base.Shutdown()

	if _, err := NewTelemetry(nil, "m", "t"); err == nil {
		t.Error("nil base accepted")
	}
	if _, err := NewTelemetry(base, "", "t"); err == nil {
		t.Error("empty meter name accepted")
	}
	if _, err := NewTelemetry(base, "m", ""); err == nil {
		t.Error("empty tracer name accepted")
	}
}

// exerciseTelemetry calls every powerschool-specific Record* helper, some
// more than once, and StartSpan.
func exerciseTelemetry(tel *Telemetry) {
	tel.RecordItemProduced("school")
	tel.RecordItemDropped("no_rule")
	tel.RecordError("auth")
	tel.RecordPollByTrigger("scheduled")
	tel.RecordPollByTrigger("scheduled")
	tel.RecordPollByTrigger("catch_up")
	tel.RecordItem("grade", "k1", ChangeNew)
	tel.RecordItem("grade", "k1", ChangeNew)
	tel.RecordItem("grade", "k1", ChangeUpdated)
	tel.RecordItem("assignment", "k2", ChangeNew)
	tel.RecordSurfaceError("assignment", "network", "k1")
	tel.RecordSurfaceError("home", "selection_unknown", "")
	tel.RecordKidSkipped("k2", skipUnknownStudent)
	tel.RecordKidSkipped("k2", skipUnknownStudent)
	tel.RecordKidSkipped("k1", skipNumberMismatch)
	_, span := tel.StartSpan(context.Background(), "powerschool.test")
	span.End()
}

// TestTelemetry_RecordedValues reads every powerschool_* counter back and
// checks name, label set and value.
func TestTelemetry_RecordedValues(t *testing.T) {
	tel, rec := newRecordingTelemetry(t)
	exerciseTelemetry(tel)

	rec.want(map[string]int64{
		"powerschool_polls_total{trigger_source=scheduled}":                                 2,
		"powerschool_polls_total{trigger_source=catch_up}":                                  1,
		"powerschool_items_total{change=new,kid=k1,surface=grade}":                          2,
		"powerschool_items_total{change=updated,kid=k1,surface=grade}":                      1,
		"powerschool_items_total{change=new,kid=k2,surface=assignment}":                     1,
		"powerschool_surface_errors_total{error_class=network,kid=k1,surface=assignment}":   1,
		"powerschool_surface_errors_total{error_class=selection_unknown,kid=,surface=home}": 1,
		"powerschool_kids_skipped_total{kid=k2,reason=unknown_student}":                     2,
		"powerschool_kids_skipped_total{kid=k1,reason=student_number_mismatch}":             1,
	})
}

// TestTelemetry_ExportedOnTheMetricsEndpoint: the instruments NewTelemetry
// creates must be registered with the framework's own provider, the one
// behind /metrics. Built on the global otel provider (which connector.Run
// never sets) they would record nothing, silently.
func TestTelemetry_ExportedOnTheMetricsEndpoint(t *testing.T) {
	base, err := connector.NewMetrics("powerschool-export-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = base.Shutdown() })
	tel, err := NewTelemetry(base, "glovebox.powerschool.exporttest", "glovebox.powerschool.exporttest")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	tel.RecordKidSkipped("kexport", skipUnknownStudent)

	rr := httptest.NewRecorder()
	base.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rr.Body)

	found := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "powerschool_kids_skipped_total{") &&
			strings.Contains(line, `kid="kexport"`) &&
			strings.Contains(line, `reason="unknown_student"`) &&
			strings.HasSuffix(strings.TrimSpace(line), " 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("/metrics does not export powerschool_kids_skipped_total{kid=\"kexport\",reason=\"unknown_student\"} 1")
	}
}

// TestTelemetry_NilSafe: processors are handed a nil *Telemetry in tests
// and must not panic on it.
func TestTelemetry_NilSafe(t *testing.T) {
	exerciseTelemetry(nil)
}
