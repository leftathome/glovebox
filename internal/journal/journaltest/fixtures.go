package journaltest

// HouseholdV1 is the roster fixture: two guardians of two siblings, a
// guest outside the household. Placeholder ids only.
const HouseholdV1 = `{
  "people": {
    "e_parent1": {"agent": "agent-parent1", "guardian_of": ["e_child1", "e_child2"]},
    "e_parent2": {"agent": "agent-parent2", "guardian_of": ["e_child1", "e_child2"]},
    "e_child1":  {"agent": "agent-child1", "siblings": ["e_child2"]},
    "e_child2":  {"agent": "agent-child2", "siblings": ["e_child1"]},
    "e_guest1":  {"agent": "agent-guest1"}
  },
  "household_members": ["e_parent1", "e_parent2", "e_child1", "e_child2"]
}
`
