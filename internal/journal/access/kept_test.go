package access

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// Kept notes (spec 4.13): a pseudonymous subject is visible exactly with a
// frozen-reader grant for it, whatever the audience; roster grants and
// "public" never reach it; frozen-reader grants never reach another row.
func TestVisibleKeptNotes(t *testing.T) {
	const p, q = "erased:00000000000000000000000000000000:1", "erased:00000000000000000000000000000000:2"
	reader := append([]roster.Grant{gr(p, ErasedReader)}, parent...)
	for _, c := range []struct {
		g        []roster.Grant
		subject  string
		audience []string
		want     bool
	}{
		{reader, p, []string{"guardians", "subject"}, true},
		{reader, p, nil, true},
		{reader, q, []string{"public"}, false},
		{parent, p, []string{"public"}, false},
		{parent, p, []string{"guardians", "household"}, false},
		{[]roster.Grant{gr("", "guardians"), gr("", "household")}, p, []string{"guardians", "household"}, false},
		{[]roster.Grant{gr(p, "guardians")}, p, []string{"guardians"}, false},
		{[]roster.Grant{gr(p, ErasedReader)}, "e_child1", []string{ErasedReader}, false},
		{[]roster.Grant{gr(p, ErasedReader)}, "", []string{"guardians"}, false},
		{reader, "e_child1", []string{"guardians"}, true}, // the roster grants still work
	} {
		if got := Visible(c.g, c.subject, c.audience); got != c.want {
			t.Errorf("Visible(%v, %q, %v) = %v", c.g, c.subject, c.audience, got)
		}
	}
	if !IsErased(p) || IsErased("e_child1") || IsErased("") {
		t.Error("IsErased")
	}
	if n := Notices(reader, p, []rules.Notice{{Token: "guardians", Mode: "push"}, {Token: ErasedReader, Mode: "push"}}); len(n) != 0 {
		t.Errorf("a kept note's notices reached %v", n)
	}
	if n := Notices(reader, "e_child1", []rules.Notice{{Token: "guardians", Mode: "push"}}); len(n) != 1 {
		t.Errorf("notices of an ordinary row: %v", n)
	}
	if OldValue(parent, p, []string{"guardians"}, true, json.RawMessage(`1`)) == nil ||
		string(OldValue(parent, p, []string{"guardians"}, true, json.RawMessage(`1`))) == `1` {
		t.Error("an old value of a kept note reached a non-reader")
	}
	if string(OldValue(reader, p, nil, true, json.RawMessage(`1`))) != `1` {
		t.Error("an old value of a kept note was withheld from its reader")
	}
}

// Property: over random rosters, frozen-reader grants and audiences, a kept
// note is visible iff the reader holds its frozen-reader grant, and adding
// frozen-reader grants changes nothing about any other row.
func TestPropertyKeptNotesFollowOnlyTheirReaders(t *testing.T) {
	kept := []string{"erased:" + fmt.Sprintf("%032x", 1) + ":1", "erased:" + fmt.Sprintf("%032x", 1) + ":2"}
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		d := genRoster(r)
		raw, _ := json.Marshal(d)
		ros, err := roster.Parse(roster.FormatOpenclawHouseholdV1, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, reader := range ros.Members() {
			base, _ := ros.Grants(reader)
			g := append([]roster.Grant(nil), base...)
			holds := map[string]bool{}
			for _, k := range kept {
				if r.Intn(2) == 0 {
					g = append(g, roster.Grant{Subject: k, Token: ErasedReader})
					holds[k] = true
				}
			}
			for mask := 0; mask < 1<<len(tokens); mask++ {
				var aud []string
				for i, tok := range tokens {
					if mask&(1<<i) != 0 {
						aud = append(aud, tok)
					}
				}
				for _, k := range kept {
					if Visible(g, k, aud) != holds[k] {
						t.Fatalf("seed %d reader %s kept %s audience %v", seed, reader, k, aud)
					}
				}
				for _, s := range append(ros.Members(), "") {
					if Visible(g, s, aud) != Visible(base, s, aud) {
						t.Fatalf("seed %d: frozen-reader grants changed row %q %v", seed, s, aud)
					}
				}
			}
		}
	}
}
