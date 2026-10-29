package records

import (
	"testing"
	"time"

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
