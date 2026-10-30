package schoology

import (
	"fmt"
	"slices"

	"github.com/leftathome/glovebox/connector"
)

// applyRecordsDefaults fills the records block, only when it is enabled: a
// disabled block stays as it was written.
func applyRecordsDefaults(c *Config) {
	if c.Records.Enabled {
		c.Records.ApplyDefaults()
	}
}

// validateRecords checks the records block and, with records on, the
// per-kid routing records depend on (decision T5). A disabled block is
// never checked, so the connector behaves exactly as before records
// existed.
//
// Every record carries its child's data_subject, taken from the rule that
// routes the child's items. Today only the detail surface insists on an
// exact per-kid rule; the assignment surface may be routed by "*", which
// carries one subject for every child. With records on:
//
//   - each kid's schoology:<kid>:assignment rule matches exactly, ahead of
//     any "*" rule, and names a data_subject;
//   - no two kids share a subject;
//   - when the detail surface is on for the kid, its rule names the same
//     subject (validateAssignmentDetail already makes it exact).
func validateRecords(c *Config) error {
	if !c.Records.Enabled {
		return nil
	}
	if err := c.Records.Validate(); err != nil {
		return err
	}
	m := connector.NewRuleMatcher(c.Rules)
	owner := map[string]string{} // subject -> kid
	for _, kid := range c.Kids {
		key := assignmentMatchKey(kid.Name)
		if !firstMatchIsExact(c.Rules, key) {
			return fmt.Errorf("records: no rule matches %q exactly ahead of any \"*\" rule; "+
				"each kid's records need the kid's own data_subject", key)
		}
		rule, _ := m.Match(key)
		if rule.DataSubject == "" {
			return fmt.Errorf("records: the rule for %q has no data_subject", key)
		}
		if other, ok := owner[rule.DataSubject]; ok {
			return fmt.Errorf("records: kids %s and %s are both routed with data_subject %s; each kid needs its own",
				other, kid.Name, rule.DataSubject)
		}
		owner[rule.DataSubject] = kid.Name
		if c.AssignmentDetail.Enabled(kid.Name) {
			dkey := detailMatchKey(kid.Name)
			if d, ok := m.Match(dkey); !ok || d.DataSubject != rule.DataSubject {
				return fmt.Errorf("records: the rule for %q must name the same data_subject as %q (%s)",
					dkey, key, rule.DataSubject)
			}
		}
	}
	return nil
}

// assignmentMatchKey is the rule key for a kid's assignment items.
func assignmentMatchKey(kid string) string {
	return "schoology:" + kid + ":" + assignmentSurface
}

// Binding is who a kid's records on one surface are about and who may see
// them: the data_subject and audience of the rule that routes the kid's
// items on that surface. Records and items share one routing decision.
type Binding struct {
	DataSubject string
	Audience    []string
	// OnSkip counts a row or snapshot a builder left out, by reason
	// (schoology_record_rows_skipped_total). nil: not counted.
	OnSkip func(reason string)
}

// subjectFor resolves the binding of a kid's surface. Without a rule, or a
// rule without a data_subject, there are no records for the surface.
func subjectFor(m *connector.RuleMatcher, kid Kid, surface string) (Binding, bool) {
	if m == nil {
		return Binding{}, false
	}
	rule, ok := m.Match("schoology:" + kid.Name + ":" + surface)
	if !ok || rule.DataSubject == "" {
		return Binding{}, false
	}
	return Binding{DataSubject: rule.DataSubject, Audience: slices.Clone(rule.Audience)}, true
}
