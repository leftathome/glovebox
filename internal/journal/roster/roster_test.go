package roster_test

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

func g(subject, token string) roster.Grant { return roster.Grant{Subject: subject, Token: token} }

// The mapping of 4.9b, pinned member by member.
func TestV1GrantsArePinned(t *testing.T) {
	r, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(journaltest.HouseholdV1))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]roster.Grant{
		"e_parent1": {g("", "guardians"), g("", "household"), g("e_child1", "guardians"), g("e_child1", "household"),
			g("e_child2", "guardians"), g("e_child2", "household"), g("e_parent1", "household"), g("e_parent1", "subject"),
			g("e_parent2", "household")},
		"e_child1": {g("", "household"), g("e_child1", "household"), g("e_child1", "subject"), g("e_child2", "household"),
			g("e_child2", "siblings"), g("e_parent1", "household"), g("e_parent2", "household")},
		"e_guest1": {g("e_guest1", "subject")},
	}
	for member, w := range want {
		got, ok := r.Grants(member)
		if !ok || !reflect.DeepEqual(got, w) {
			t.Errorf("%s:\n got %v\nwant %v", member, got, w)
		}
	}
	if _, ok := r.Grants("e_nobody"); ok {
		t.Error("a non-member has grants")
	}
	if !r.Has("e_guest1") || r.Has("e_nobody") || r.Has("") {
		t.Error("Has")
	}
	sum := sha256.Sum256([]byte(journaltest.HouseholdV1))
	if r.SHA256() != hex.EncodeToString(sum[:]) {
		t.Error("the hash is not over the file's bytes (openclaw logs the same hash)")
	}
	// No caregivers grant and no ""-subject subject/siblings grant exists.
	for _, m := range r.Members() {
		gs, _ := r.Grants(m)
		for _, gr := range gs {
			if gr.Token == "caregivers" || (gr.Subject == "" && (gr.Token == "subject" || gr.Token == "siblings")) {
				t.Errorf("%s holds %v", m, gr)
			}
		}
	}
}

func TestV1RefusesWhatOpenclawRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"not json":          `{`,
		"unknown key":       `{"people": {"e_a": {"agent": "a", "gaurdian_of": []}}, "household_members": []}`,
		"no agent":          `{"people": {"e_a": {"agent": ""}}, "household_members": []}`,
		"dangling guardian": `{"people": {"e_a": {"agent": "a", "guardian_of": ["e_b"]}}, "household_members": []}`,
		"dangling sibling":  `{"people": {"e_a": {"agent": "a", "siblings": ["e_b"]}}, "household_members": []}`,
		"dangling member":   `{"people": {"e_a": {"agent": "a"}}, "household_members": ["e_b"]}`,
		"unsafe id":         `{"people": {"e a": {"agent": "a"}}, "household_members": []}`,
		"shared agent":      `{"people": {"e_a": {"agent": "a"}, "e_b": {"agent": "a"}}, "household_members": []}`,
		"no people":         `{"people": {}, "household_members": []}`,
		"trailing data":     `{"people": {"e_a": {"agent": "a"}}, "household_members": []} {}`,
	} {
		if _, err := roster.Parse(roster.FormatOpenclawHouseholdV1, []byte(doc)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := roster.Parse("some-other-format", []byte(journaltest.HouseholdV1)); err == nil {
		t.Error("an unknown format parsed")
	}
}
