package schoology

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/records"
)

// T5: with records on, each kid's assignment rule is exact and ahead of
// any "*" rule, so a record never carries the wildcard's default subject.
func TestRecordsNeedAnExactAssignmentRulePerKid(t *testing.T) {
	cfg := recordsConfig(t)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Rules = append([]connector.Rule{{Match: "*", Destination: "school", DataSubject: "e_child1"}}, cfg.Rules...)
	err := ValidateConfig(&cfg)
	if err == nil || !strings.Contains(err.Error(), "records") || !strings.Contains(err.Error(), "schoology:k1:assignment") {
		t.Fatalf("a * rule ahead of the kid's rule: err = %v", err)
	}
}

// Without the detail surface the assignment rule alone is checked, and a
// kid with no assignment rule at all is refused.
func TestRecordsNeedAnAssignmentRuleEvenWithoutDetail(t *testing.T) {
	cfg := recordsConfig(t)
	cfg.AssignmentDetail.Kids = nil
	cfg.Rules = []connector.Rule{recordRules()[0], recordRules()[3], {Match: "*", Destination: "school"}}
	err := ValidateConfig(&cfg)
	if err == nil || !strings.Contains(err.Error(), "schoology:k2:assignment") {
		t.Fatalf("k2 routed by the wildcard: err = %v", err)
	}
}

func TestRecordsRefuseOneSubjectForTwoKids(t *testing.T) {
	cfg := recordsConfig(t)
	for i := range cfg.Rules {
		if cfg.Rules[i].DataSubject == "e_child2" {
			cfg.Rules[i].DataSubject = "e_child1"
		}
	}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "records") || !strings.Contains(err.Error(), "e_child1") {
		t.Fatalf("two kids with one subject: err = %v", err)
	}
}

func TestRecordsRefuseARuleWithoutSubject(t *testing.T) {
	cfg := recordsConfig(t)
	cfg.Rules[2].DataSubject = "" // schoology:k2:assignment
	cfg.Rules[2].Audience = []string{"household"}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "data_subject") {
		t.Fatalf("assignment rule without subject: err = %v", err)
	}
}

// Decision 2026-10-30 (fail closed): a record's audience is its rule's
// audience, never audience_default. A rule that leaves the audience to
// audience_default would give the item that default (say [operator]) and
// the record the type's [subject, guardians]; with records on it is
// refused, naming the kid and the surface.
func TestRecordsRefuseARuleWithoutAudience(t *testing.T) {
	cfg := recordsConfig(t)
	cfg.AudienceDefault = []string{"operator"}
	cfg.Rules[0].Audience = nil // schoology:k1:assignment
	err := ValidateConfig(&cfg)
	if err == nil || !strings.Contains(err.Error(), "audience") || !strings.Contains(err.Error(), "schoology:k1:assignment") ||
		!strings.Contains(err.Error(), "k1") {
		t.Fatalf("assignment rule without audience: err = %v", err)
	}
	cfg = recordsConfig(t)
	cfg.Rules[3].Audience = nil // schoology:k2:assignment_detail
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "schoology:k2:assignment_detail") ||
		!strings.Contains(err.Error(), "audience") {
		t.Fatalf("detail rule without audience: err = %v", err)
	}
	// The detail surface off for k2: its detail rule is not used for records.
	cfg.AssignmentDetail.Kids = []string{"k1"}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("detail off for k2: %v", err)
	}
	// Records off: as liberal as before.
	cfg = recordsConfig(t)
	cfg.Records = records.Config{}
	cfg.AssignmentDetail.Kids = nil
	cfg.Rules[0].Audience = nil
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("records off: %v", err)
	}
}

func TestRecordsRefuseDetailRuleWithAnotherSubject(t *testing.T) {
	cfg := recordsConfig(t)
	cfg.Rules[1].DataSubject = "e_child2" // schoology:k1:assignment_detail
	err := ValidateConfig(&cfg)
	if err == nil || !strings.Contains(err.Error(), "schoology:k1:assignment_detail") {
		t.Fatalf("k1's detail rule names k2's subject: err = %v", err)
	}
	// Off the detail surface for k1, the same rules are fine for records.
	cfg.AssignmentDetail.Kids = []string{"k2"}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("detail off for k1: %v", err)
	}
}

func TestWithoutRecordsTheRulesAreAsLiberalAsToday(t *testing.T) {
	cfg := recordsConfig(t)
	cfg.Records = records.Config{}
	cfg.AssignmentDetail.Kids = nil
	cfg.Rules = append([]connector.Rule{{Match: "*", Destination: "school", DataSubject: "e_child1"}}, cfg.Rules...)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("records off: %v", err)
	}
	for i := range cfg.Rules {
		cfg.Rules[i].DataSubject = ""
		cfg.Rules[i].Audience = nil
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("records off, no subjects: %v", err)
	}
}

// The block is checked and defaulted only when enabled: a disabled block
// stays as written, so today's configs are unaffected.
func TestRecordsBlockDefaultsAndValidation(t *testing.T) {
	cfg := recordsConfig(t)
	if cfg.Records.TimeoutSeconds != 90 || cfg.Records.Spool.MaxAgeHours != 168 {
		t.Fatalf("defaults not applied: %+v", cfg.Records)
	}
	cfg.Records.JournalURL = "ftp://journal"
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "journal_url") {
		t.Fatalf("bad journal_url: err = %v", err)
	}

	off := detailConfig()
	off.Records.JournalURL = "not a url"
	ApplyDefaults(&off)
	if !reflect.DeepEqual(off.Records, records.Config{JournalURL: "not a url"}) {
		t.Fatalf("a disabled block was filled: %+v", off.Records)
	}
	if err := ValidateConfig(&off); err != nil {
		t.Fatalf("a disabled block was validated: %v", err)
	}
}

func TestRecordsConfigJSON(t *testing.T) {
	var cfg Config
	raw := `{"records":{"enabled":true,"journal_url":"http://j:9095","token_file":"/t","spool":{"max_age_hours":24}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	r := cfg.Records
	if !r.Enabled || r.JournalURL != "http://j:9095" || r.TokenFile != "/t" || r.Spool.MaxAgeHours != 24 {
		t.Fatalf("decoded %+v", r)
	}
}

func TestSubjectForUsesTheKidsRule(t *testing.T) {
	m := connector.NewRuleMatcher(recordRules())
	b, ok := subjectFor(m, detailKid2, detailSurface)
	if !ok || b.DataSubject != "e_child2" || !reflect.DeepEqual(b.Audience, []string{"subject", "guardians"}) {
		t.Fatalf("binding %+v %v", b, ok)
	}
	if _, ok := subjectFor(m, Kid{Name: "k3"}, assignmentSurface); ok {
		t.Fatal("a kid with no rule has no binding")
	}
	noSubject := connector.NewRuleMatcher([]connector.Rule{{Match: "schoology:k1:assignment", Destination: "school"}})
	if _, ok := subjectFor(noSubject, detailKid1, assignmentSurface); ok {
		t.Fatal("a rule without data_subject gives no binding")
	}
	b, _ = subjectFor(m, detailKid1, assignmentSurface)
	b.Audience[0] = "public"
	if again, _ := subjectFor(m, detailKid1, assignmentSurface); again.Audience[0] != "subject" {
		t.Fatal("binding audience aliases the rule")
	}
}
