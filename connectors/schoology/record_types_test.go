package schoology

import (
	"reflect"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestRecordTypesAreValidForTheJournal(t *testing.T) {
	if _, err := record.NewRegistry(RecordTypes()); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ty := range RecordTypes() {
		names[ty.Name] = true
		if ty.Owner != "schoology" || ty.SchemaVersion != 1 {
			t.Errorf("%s: owner %q version %d", ty.Name, ty.Owner, ty.SchemaVersion)
		}
		for name, def := range ty.Fields {
			if def.Required {
				t.Errorf("%s.%s is required; every field is nullable", ty.Name, name)
			}
		}
		if !reflect.DeepEqual(ty.DefaultAudience, []string{"subject", "guardians"}) {
			t.Errorf("%s default audience %v", ty.Name, ty.DefaultAudience)
		}
	}
	if len(names) != 2 || !names[TypeLMSAssignment] || !names[TypeLMSAssignmentDetail] {
		t.Fatalf("record types %v, want lms_assignment and lms_assignment_detail", names)
	}
}

// The field sets of spec 5.2, pinned by name and type. Decision T1: no
// teacher field in version 1 (the library does not expose one).
func TestRecordTypeFieldSets(t *testing.T) {
	want := map[string]map[string]record.FieldType{
		TypeLMSAssignment: {"title": record.String, "course": record.String, "status": record.String,
			"link": record.String, "due": record.Timestamp},
		TypeLMSAssignmentDetail: {"description": record.Text, "comments": record.Text},
	}
	for _, ty := range RecordTypes() {
		got := map[string]record.FieldType{}
		for n, d := range ty.Fields {
			got[n] = d.Type
		}
		if !reflect.DeepEqual(got, want[ty.Name]) {
			t.Errorf("%s fields %v, want %v", ty.Name, got, want[ty.Name])
		}
	}
}
