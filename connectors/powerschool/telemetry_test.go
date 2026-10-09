package powerschool

import (
	"context"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// NewTestTelemetry builds a Telemetry on a real *connector.Metrics.
func NewTestTelemetry(t *testing.T) *Telemetry {
	t.Helper()
	base, err := connector.NewMetrics("powerschool-test")
	if err != nil {
		t.Fatalf("connector.NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = base.Shutdown() })
	tel, err := NewTelemetry(base, "powerschool-test", "powerschool-test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	return tel
}

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

// exerciseTelemetry calls every Record* helper and StartSpan once.
func exerciseTelemetry(tel *Telemetry) {
	tel.RecordItemProduced("school")
	tel.RecordItemDropped("no_rule")
	tel.RecordError("auth")
	tel.RecordPollByTrigger("scheduled")
	tel.RecordItem("grade", "k1", ChangeNew)
	tel.RecordSurfaceError("assignment", "network", "k1")
	tel.RecordKidSkipped("k2", "not_selected")
	_, span := tel.StartSpan(context.Background(), "powerschool.test")
	span.End()
}

func TestTelemetry_RecordsWithoutPanic(t *testing.T) {
	exerciseTelemetry(NewTestTelemetry(t))
}

// TestTelemetry_NilSafe: processors are handed a nil *Telemetry in tests
// and must not panic on it.
func TestTelemetry_NilSafe(t *testing.T) {
	exerciseTelemetry(nil)
}
