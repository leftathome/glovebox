package powerschool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestRecordTypesAreValidForTheJournal(t *testing.T) {
	if _, err := record.NewRegistry(RecordTypes()); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ty := range RecordTypes() {
		names[ty.Name] = true
		if ty.Owner != "powerschool" || ty.SchemaVersion != 1 {
			t.Errorf("%s: owner %q version %d", ty.Name, ty.Owner, ty.SchemaVersion)
		}
		for name, def := range ty.Fields {
			if def.Required {
				t.Errorf("%s.%s is required; every field is nullable", ty.Name, name)
			}
		}
	}
	if len(names) != 7 {
		t.Fatalf("%d record types, want 7", len(names))
	}
}

// The field sets of spec 5.1 (with S9), pinned by name and type.
func TestRecordTypeFieldSets(t *testing.T) {
	want := map[string]map[string]record.FieldType{
		TypeTermGrade:       {"course": record.String, "term": record.String, "grade": record.String, "earned": record.Number, "attempted": record.Number},
		TypeTermComment:     {"course": record.String, "term": record.String, "comment": record.Text},
		TypeAttendanceTotal: {"course": record.String, "absences": record.Int, "tardies": record.Int},
		TypeAttendanceMark: {"date": record.Date, "course": record.String, "period": record.String, "code": record.String,
			"code_description": record.String, "kind": record.String, "excused": record.Bool},
	}
	counts := map[string]int{TypeCourseMark: 15, TypeAssignment: 17, TypeAttendanceDay: 11}
	for _, ty := range RecordTypes() {
		if w, ok := want[ty.Name]; ok {
			got := map[string]record.FieldType{}
			for n, d := range ty.Fields {
				got[n] = d.Type
			}
			if !reflect.DeepEqual(got, w) {
				t.Errorf("%s fields %v, want %v", ty.Name, got, w)
			}
		} else if len(ty.Fields) != counts[ty.Name] {
			t.Errorf("%s has %d fields, want %d", ty.Name, len(ty.Fields), counts[ty.Name])
		}
	}
	for _, ty := range RecordTypes() {
		if ty.Name == TypeAssignment && ty.Fields["absent"].Type != record.Bool {
			t.Error("assignment.absent must be a bool (decision S9)")
		}
	}
}

func TestTermCommentDefaultAudienceIsGuardiansOnly(t *testing.T) {
	for _, ty := range RecordTypes() {
		want := []string{"subject", "guardians"}
		if ty.Name == TypeTermComment {
			want = []string{"guardians"}
		}
		if !reflect.DeepEqual(ty.DefaultAudience, want) {
			t.Fatalf("%s default audience %v, want %v", ty.Name, ty.DefaultAudience, want)
		}
	}
}

func TestRecordsConfigRefusesCustomAttendanceKinds(t *testing.T) {
	cfg := validRecordsConfig(t)
	cfg.AttendanceDays.Kinds = map[string][]string{"absence_unexcused": {"UA"}, "tardy_custom": {"T"}}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("err = %v", err)
	}
	cfg.Records.Enabled = false
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("without records the custom kind is allowed as today: %v", err)
	}
}

func TestRecordsConfigDefaultsAndBounds(t *testing.T) {
	cfg := validRecordsConfig(t)
	if cfg.Records.AttendanceDaySettleSchoolDays != 3 || cfg.Records.TimeoutSeconds != 90 {
		t.Fatalf("defaults: settle %d timeout %d", cfg.Records.AttendanceDaySettleSchoolDays, cfg.Records.TimeoutSeconds)
	}
	for _, n := range []int{-1, 11} {
		bad := validRecordsConfig(t)
		bad.Records.AttendanceDaySettleSchoolDays = n
		if err := ValidateConfig(&bad); err == nil || !strings.Contains(err.Error(), "settle") {
			t.Errorf("settle %d: err = %v", n, err)
		}
	}
	for _, n := range []int{1, 10} {
		ok := validRecordsConfig(t)
		ok.Records.AttendanceDaySettleSchoolDays = n
		if err := ValidateConfig(&ok); err != nil {
			t.Errorf("settle %d: %v", n, err)
		}
	}
	bad := validRecordsConfig(t)
	bad.Records.JournalURL = "ftp://journal"
	if err := ValidateConfig(&bad); err == nil || !strings.Contains(err.Error(), "journal_url") {
		t.Errorf("journal_url: err = %v", err)
	}
}

// A disabled block is left as written and never checked: today's configs
// are unaffected.
func TestRecordsOffConfigIsUntouched(t *testing.T) {
	cfg := validConfig()
	if !reflect.DeepEqual(cfg.Records, PowerSchoolRecords{}) {
		t.Fatalf("disabled records block was filled: %+v", cfg.Records)
	}
	cfg.Records.JournalURL = "not a url"
	cfg.Records.AttendanceDaySettleSchoolDays = 99
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("a disabled block was validated: %v", err)
	}
}

// The embedded shared block is flattened into "records".
func TestRecordsConfigJSONIsFlat(t *testing.T) {
	var cfg Config
	raw := `{"records":{"enabled":true,"journal_url":"http://j:9095","token_file":"/t","attendance_day_settle_school_days":4,"spool":{"max_age_hours":24}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	r := cfg.Records
	if !r.Enabled || r.JournalURL != "http://j:9095" || r.TokenFile != "/t" || r.AttendanceDaySettleSchoolDays != 4 || r.Spool.MaxAgeHours != 24 {
		t.Fatalf("decoded %+v", r)
	}
}

func TestSubjectForUsesTheKidsRule(t *testing.T) {
	m := connector.NewRuleMatcher(recordRules())
	b, ok := subjectFor(m, Kid{Name: "k2"}, teacherCommentSurface)
	if !ok || b.DataSubject != "e_child2" || !reflect.DeepEqual(b.Audience, []string{"guardians"}) {
		t.Fatalf("binding %+v %v", b, ok)
	}
	b, ok = subjectFor(m, Kid{Name: "k1"}, gradeSurface)
	if !ok || b.DataSubject != "e_child1" || !reflect.DeepEqual(b.Audience, []string{"subject", "guardians"}) {
		t.Fatalf("binding %+v %v", b, ok)
	}
	if _, ok := subjectFor(m, Kid{Name: "k3"}, gradeSurface); ok {
		t.Fatal("a kid with no rule has no binding")
	}
	noSubject := connector.NewRuleMatcher([]connector.Rule{{Match: "powerschool:k1:grade", Destination: "school"}})
	if _, ok := subjectFor(noSubject, Kid{Name: "k1"}, gradeSurface); ok {
		t.Fatal("a rule without data_subject gives no binding")
	}
	// The audience is a copy: a builder cannot change the rule.
	b, _ = subjectFor(m, Kid{Name: "k1"}, gradeSurface)
	b.Audience[0] = "public"
	if again, _ := subjectFor(m, Kid{Name: "k1"}, gradeSurface); again.Audience[0] != "subject" {
		t.Fatal("binding audience aliases the rule")
	}
}
