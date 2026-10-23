package rules

import (
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
)

func regWithText(t *testing.T) *record.Registry {
	t.Helper()
	r, err := record.NewRegistry([]record.Type{{
		Name: "assignment", SchemaVersion: 1, Owner: "powerschool",
		Fields: map[string]record.FieldDef{"title": {Type: record.String}, "comment": {Type: record.Text},
			"due": {Type: record.Date}},
		DefaultAudience: []string{"subject", "guardians"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A group key is returned as written, outside the scan verdicts, so a rule
// cannot group by a text field: the key would carry untrusted text, or the
// sha256 of it once it passes the length cap (a guess-confirmation oracle).
func TestGroupByATextFieldIsRefused(t *testing.T) {
	for _, g := range [][]string{
		{"field:comment"},
		{"data_subject", "field:comment"},
		{"field:title", "field:comment"},
	} {
		_, err := Compile([]Rule{{Name: "g", Importance: Normal, Match: Match{RecordType: "assignment"}, GroupBy: g}}, regWithText(t))
		if err == nil || !strings.Contains(err.Error(), "text field") {
			t.Errorf("group_by %v: %v", g, err)
		}
	}
}

func TestGroupByNonTextFieldsAndBuiltinsStillCompile(t *testing.T) {
	for _, g := range [][]string{
		{"field:title"}, {"field:due"}, {"data_subject", "record_type", "key", "scope"},
		{"data_subject", "field:title", "field:due"},
	} {
		if _, err := Compile([]Rule{{Name: "g", Importance: Normal, Match: Match{RecordType: "assignment"}, GroupBy: g}}, regWithText(t)); err != nil {
			t.Errorf("group_by %v: %v", g, err)
		}
	}
}

// The refusal looks at the active type of the rule's own record type: a
// text field of another type does not matter.
func TestGroupByTextRefusalIsPerRuleType(t *testing.T) {
	r, err := record.NewRegistry([]record.Type{
		{Name: "assignment", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: []string{"subject"},
			Fields: map[string]record.FieldDef{"title": {Type: record.Text}}},
		{Name: "attendance_day", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: []string{"subject"},
			Fields: map[string]record.FieldDef{"title": {Type: record.String}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(typ string) error {
		_, err := Compile([]Rule{{Name: "g", Importance: Normal, Match: Match{RecordType: typ}, GroupBy: []string{"field:title"}}}, r)
		return err
	}
	if mk("assignment") == nil {
		t.Error("a text title was accepted")
	}
	if err := mk("attendance_day"); err != nil {
		t.Errorf("a string title was refused: %v", err)
	}
}
