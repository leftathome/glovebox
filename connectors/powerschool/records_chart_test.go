package powerschool

import (
	"encoding/json"
	"os"
	"testing"
)

// TestHelmRenderedRecordsBlock: the records block the chart renders with its
// default records values (scripts/test-chart-records.sh compares the render
// with testdata/helm-rendered-records.json) is a block this connector
// accepts, and every value in it is the one that runs: applying the
// defaults changes nothing the chart set.
func TestHelmRenderedRecordsBlock(t *testing.T) {
	data, err := os.ReadFile("testdata/helm-rendered-records.json")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal([]byte(`{"records":`+string(data)+`}`), &c); err != nil {
		t.Fatal(err)
	}
	rendered := c.Records
	applyRecordsDefaults(&c.Records)
	if err := validateRecords(&c); err != nil {
		t.Fatalf("chart -> Go: the rendered records block is refused: %v", err)
	}
	r := c.Records
	if !r.Enabled || r.JournalURL != "http://glovebox-glovebox-journal:9095" ||
		r.TokenFile != "/var/run/secrets/glovebox-journal/token" {
		t.Errorf("chart -> Go mismatch: %+v", r)
	}
	if r.TimeoutSeconds != rendered.TimeoutSeconds || r.AttendanceDaySettleSchoolDays != rendered.AttendanceDaySettleSchoolDays ||
		r.AttendanceDayCarryMaxDays != rendered.AttendanceDayCarryMaxDays ||
		r.Spool.MaxAgeHours != rendered.Spool.MaxAgeHours || r.Spool.MaxBytes != rendered.Spool.MaxBytes {
		t.Errorf("defaults changed a value the chart rendered: rendered %+v, running %+v", rendered, r)
	}
	if rendered.TimeoutSeconds == 0 || rendered.AttendanceDaySettleSchoolDays == 0 || rendered.AttendanceDayCarryMaxDays == 0 ||
		rendered.Spool.MaxAgeHours == 0 || rendered.Spool.MaxBytes == 0 {
		t.Errorf("the chart left a value to the connector's defaults: %+v", rendered)
	}
}
