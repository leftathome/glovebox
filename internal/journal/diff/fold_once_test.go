package diff

import (
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// TestRegistryRefusesReusingARenamedAwayName pins why Fold applying a rename
// twice cannot be observed: a later version may not rename onto any name an
// earlier version used, so a rename chain never revisits a name and
// forwarding once or twice gives the same result.
func TestRegistryRefusesReusingARenamedAwayName(t *testing.T) {
	mk := func(v int, renames map[string]string, names ...string) record.Type {
		fields := map[string]record.FieldDef{}
		for _, n := range names {
			fields[n] = record.FieldDef{Type: record.String}
		}
		return record.Type{Name: "t", SchemaVersion: v, Owner: "src", Fields: fields, Renames: renames}
	}
	_, err := record.NewRegistry([]record.Type{
		mk(1, nil, "title", "date"),
		mk(2, map[string]string{"date": "due"}, "title", "due"),
		mk(3, map[string]string{"title": "date"}, "date", "due"),
	})
	if err == nil {
		t.Fatal("registry accepted v3 renaming title onto date, which v1 used and v2 renamed away")
	}
}
