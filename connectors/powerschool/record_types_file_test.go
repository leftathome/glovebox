package powerschool

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

var updateRecordTypesFile = flag.Bool("update", false, "rewrite charts/glovebox/journal-record-types/powerschool.json from RecordTypes()")

// recordTypesFile is the chart's copy of RecordTypes (decision S14),
// rendered into the journal's record_types when journal.connectorRecordTypes
// lists powerschool.
const recordTypesFile = "../../charts/glovebox/journal-record-types/powerschool.json"

// TestRecordTypesFileIsUpToDate fails when the chart's file and the Go
// definitions drift. Regenerate with:
//
//	go test ./connectors/powerschool/ -run TestRecordTypesFileIsUpToDate -update
func TestRecordTypesFileIsUpToDate(t *testing.T) {
	want, err := json.MarshalIndent(RecordTypes(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *updateRecordTypesFile {
		if err := os.WriteFile(recordTypesFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(recordTypesFile)
	if err != nil {
		t.Fatalf("%v (generate it with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not RecordTypes(); regenerate it with: go test ./connectors/powerschool/ -run TestRecordTypesFileIsUpToDate -update", recordTypesFile)
	}
	// What the journal core will load: the file decodes to the same types
	// and they form a valid registry.
	var types []record.Type
	if err := json.Unmarshal(got, &types); err != nil {
		t.Fatal(err)
	}
	if _, err := record.NewRegistry(types); err != nil {
		t.Fatal(err)
	}
	if len(types) != 7 {
		t.Fatalf("%d types in the file, want 7", len(types))
	}
}
