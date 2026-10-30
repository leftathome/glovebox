package powerschool

import (
	"encoding/json"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// documentedRules are the journal.rules values.yaml documents for gitops
// next to journal.connectorRecordTypes (Part B1 Task 21), as JSON. Keep the
// two the same.
const documentedRules = `[
 {"name": "full_day_absence",
  "match": {"record_type": "attendance_day", "kind": ["created", "updated"],
            "field_becomes": {"classification": "full_day", "any_unexcused": true}},
  "importance": "high",
  "notify": [{"token": "guardians", "mode": "push"}, {"token": "subject", "mode": "prompt", "purpose": "attendance_followup"}],
  "group_by": ["data_subject", "field:date"]},
 {"name": "partial_day_absence",
  "match": {"record_type": "attendance_day", "field_becomes": {"classification": "partial_day", "any_unexcused": true}},
  "importance": "normal",
  "notify": [{"token": "guardians", "mode": "digest"}, {"token": "subject", "mode": "prompt", "purpose": "attendance_followup"}],
  "group_by": ["data_subject", "field:date"]}
]`

// TestDocumentedJournalRulesCompile: the rules the chart documents compile
// against the PowerSchool record types as the core compiles them, and the
// values they match on are values this connector writes.
func TestDocumentedJournalRulesCompile(t *testing.T) {
	reg, err := record.NewRegistry(RecordTypes())
	if err != nil {
		t.Fatal(err)
	}
	var rs []rules.Rule
	if err := json.Unmarshal([]byte(documentedRules), &rs); err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Compile(rs, reg); err != nil {
		t.Fatal(err)
	}
	full := (&dayTally{met: 2, absUnex: 2}).classification()
	partial := (&dayTally{met: 2, absUnex: 1}).classification()
	if full != "full_day" || partial != "partial_day" {
		t.Errorf("the rules match full_day and partial_day; attendance_day writes %q and %q", full, partial)
	}
}
