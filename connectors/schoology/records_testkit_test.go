package schoology

import (
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/records"
)

// Record tests use their own subjects (e_child1, e_child2) so that nothing
// here depends on the item fixtures' ids. All names, courses and text are
// invented.

// recordRules routes k1 and k2 by exact rules on both record surfaces, with
// subjects e_child1 / e_child2 and audience [subject, guardians].
func recordRules() []connector.Rule {
	aud := []string{"subject", "guardians"}
	return []connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "e_child1", Audience: aud},
		{Match: "schoology:k1:assignment_detail", Destination: "school", DataSubject: "e_child1", Audience: aud},
		{Match: "schoology:k2:assignment", Destination: "school", DataSubject: "e_child2", Audience: aud},
		{Match: "schoology:k2:assignment_detail", Destination: "school", DataSubject: "e_child2", Audience: aud},
		{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
	}
}

// recordsBlock is an enabled records block that passes validation.
func recordsBlock(url string) records.Config {
	return records.Config{Enabled: true, JournalURL: url, TokenFile: "/var/run/secrets/glovebox-journal/token"}
}

// recordsConfig is a two-kid config (k1, k2) with the detail surface on for
// both, exact rules for both record surfaces and records enabled. It has not
// been through ApplyDefaults or ValidateConfig.
func recordsConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Kids:             []Kid{detailKid1, detailKid2},
		PollSchedule:     PollSchedule{Windows: []PollWindow{{Start: "08:00", End: "09:00"}}},
		AssignmentDetail: AssignmentDetailConfig{Kids: []string{"k1", "k2"}},
		Records:          recordsBlock("http://journal.example:9095"),
	}
	cfg.Rules = recordRules()
	ApplyDefaults(&cfg)
	return cfg
}
