package access

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/roster"
)

type person struct {
	Agent      string   `json:"agent"`
	GuardianOf []string `json:"guardian_of,omitempty"`
	Siblings   []string `json:"siblings,omitempty"`
}

type doc struct {
	People           map[string]person `json:"people"`
	HouseholdMembers []string          `json:"household_members"`
}

func genRoster(r *rand.Rand) doc {
	n := 2 + r.Intn(5)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("e_m%d", i)
	}
	d := doc{People: map[string]person{}}
	for i, id := range ids {
		p := person{Agent: "agent-" + id}
		for _, other := range ids {
			if other == id {
				continue // nobody is their own guardian or sibling
			}
			if r.Intn(4) == 0 {
				p.GuardianOf = append(p.GuardianOf, other)
			}
			if r.Intn(4) == 0 {
				p.Siblings = append(p.Siblings, other)
			}
		}
		d.People[id] = p
		if r.Intn(3) != 0 || i == 0 {
			d.HouseholdMembers = append(d.HouseholdMembers, id)
		}
	}
	return d
}

// entitled is spec 11's audience semantics written from the relationships,
// independently of the adapter's grants.
func entitled(d doc, reader, subject string, audience []string) bool {
	p := d.People[reader]
	for _, tok := range audience {
		switch tok {
		case "public":
			return true
		case "subject":
			if subject != "" && subject == reader {
				return true
			}
		case "guardians":
			if subject == "" && len(p.GuardianOf) > 0 || subject != "" && slices.Contains(p.GuardianOf, subject) {
				return true
			}
		case "siblings":
			if subject != "" && slices.Contains(p.Siblings, subject) {
				return true
			}
		case "household":
			if slices.Contains(d.HouseholdMembers, reader) && (subject == "" || slices.Contains(d.HouseholdMembers, subject)) {
				return true
			}
		}
	}
	return false
}

var tokens = []string{"subject", "guardians", "siblings", "household", "public", "caregivers"}

func TestPropertyGrantsMatchRelationships(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		d := genRoster(r)
		raw, _ := json.Marshal(d)
		ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, raw)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		subjects := append(ros.Members(), "", "e_outsider")
		for _, reader := range ros.Members() {
			grants, _ := ros.Grants(reader)
			for _, subject := range subjects {
				for mask := 1; mask < 1<<len(tokens); mask++ {
					var aud []string
					for i, tok := range tokens {
						if mask&(1<<i) != 0 {
							aud = append(aud, tok)
						}
					}
					got, want := Visible(grants, subject, aud), entitled(d, reader, subject, aud)
					if got != want {
						t.Fatalf("seed %d reader %s subject %q audience %v: visible %v, entitled %v", seed, reader, subject, aud, got, want)
					}
					// A guardians-only row never reaches its own subject.
					if subject == reader && len(aud) == 1 && aud[0] == "guardians" && got {
						t.Fatalf("seed %d: guardians-only row about %s visible to %s", seed, subject, reader)
					}
				}
			}
		}
	}
}
