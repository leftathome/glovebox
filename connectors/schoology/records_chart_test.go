package schoology

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/leftathome/glovebox/connector/records"
)

// TestHelmRenderedRecordsBlock: the records block the chart renders for
// schoologyConnector.records with its default values
// (testdata/helm-rendered-records.json; scripts/test-chart-records.sh pins
// the render to it) is one the connector accepts, has no key the connector
// does not know (no PowerSchool settle time), and leaves no value to the
// connector's defaults.
func TestHelmRenderedRecordsBlock(t *testing.T) {
	data, err := os.ReadFile("testdata/helm-rendered-records.json")
	if err != nil {
		t.Fatal(err)
	}
	var rendered records.Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rendered); err != nil {
		t.Fatalf("the rendered block has a key the connector does not read: %v", err)
	}
	cfg := recordsConfig(t)
	cfg.Records = rendered
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("chart -> Go: the rendered records block is refused: %v", err)
	}
	r := cfg.Records
	if !r.Enabled || r.JournalURL != "http://glovebox-glovebox-journal:9095" ||
		r.TokenFile != "/var/run/secrets/glovebox-journal/token" {
		t.Errorf("chart -> Go mismatch: %+v", r)
	}
	if r.TimeoutSeconds != rendered.TimeoutSeconds || r.Spool.MaxAgeHours != rendered.Spool.MaxAgeHours ||
		r.Spool.MaxBytes != rendered.Spool.MaxBytes {
		t.Errorf("defaults changed a value the chart rendered: rendered %+v, running %+v", rendered, r)
	}
	if rendered.TimeoutSeconds == 0 || rendered.Spool.MaxAgeHours == 0 || rendered.Spool.MaxBytes == 0 {
		t.Errorf("the chart left a value to the connector's defaults: %+v", rendered)
	}
}
