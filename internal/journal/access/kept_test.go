package access

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// Kept notes (spec 4.13): roster grants and "public" never reach a
// pseudonymous subject. Its author's grant reaches every row of it; any
// other frozen reader's grant reaches only rows whose audience is exactly
// the frozen one. Frozen-reader grants never reach another row.
func TestVisibleKeptNotes(t *testing.T) {
	const p, q = "erased:00000000000000000000000000000000:1", "erased:00000000000000000000000000000000:2"
	frozen := []string{"guardians", "subject"}
	reader := append([]roster.Grant{gr(p, ReaderToken(frozen))}, parent...)
	author := []roster.Grant{gr(p, ErasedAuthor)}
	for _, c := range []struct {
		g        []roster.Grant
		subject  string
		audience []string
		want     bool
	}{
		{reader, p, frozen, true},
		{reader, p, []string{"subject"}, false},   // another audience: withheld
		{reader, p, []string{"guardians"}, false}, // a part of it: withheld
		{reader, p, []string{"guardians", "subject", "x"}, false},
		{reader, p, nil, false},
		{author, p, []string{"subject"}, true}, // the author sees the whole history
		{author, p, nil, true},
		{author, q, frozen, false},
		{reader, q, frozen, false},
		{parent, p, []string{"public"}, false},
		{parent, p, []string{"guardians", "household"}, false},
		{[]roster.Grant{gr("", "guardians"), gr("", "household")}, p, []string{"guardians", "household"}, false},
		{[]roster.Grant{gr(p, "guardians")}, p, []string{"guardians"}, false},
		{[]roster.Grant{gr(p, ErasedReaderPrefix+"guardians")}, p, []string{"guardians"}, true},
		{[]roster.Grant{gr(p, ErasedReaderPrefix)}, p, []string{"guardians"}, false},
		{[]roster.Grant{gr(p, ErasedAuthor), gr(p, ReaderToken(frozen))}, "e_child1", []string{ErasedAuthor}, false},
		{[]roster.Grant{gr(p, ErasedAuthor)}, "", []string{"guardians"}, false},
		{reader, "e_child1", []string{"guardians"}, true}, // the roster grants still work
	} {
		if got := Visible(c.g, c.subject, c.audience); got != c.want {
			t.Errorf("Visible(%v, %q, %v) = %v", c.g, c.subject, c.audience, got)
		}
	}
	if ReaderToken(frozen) != "erased:reader:guardians,subject" {
		t.Error(ReaderToken(frozen))
	}
	if !IsErased(p) || IsErased("e_child1") || IsErased("") {
		t.Error("IsErased")
	}
	if n := Notices(author, p, []rules.Notice{{Token: "guardians", Mode: "push"}, {Token: ErasedAuthor, Mode: "push"}}); len(n) != 0 {
		t.Errorf("a kept note's notices reached %v", n)
	}
	if n := Notices(reader, "e_child1", []rules.Notice{{Token: "guardians", Mode: "push"}}); len(n) != 1 {
		t.Errorf("notices of an ordinary row: %v", n)
	}
	// Old values across an audience change: only from the frozen audience,
	// or for the author.
	one := json.RawMessage(`1`)
	if string(OldValue(reader, p, []string{"subject"}, true, one)) == `1` {
		t.Error("an old value from another audience reached a frozen reader")
	}
	if string(OldValue(reader, p, frozen, true, one)) != `1` || string(OldValue(author, p, []string{"subject"}, true, one)) != `1` {
		t.Error("an old value was withheld from its reader")
	}
	if string(OldValue(parent, p, frozen, true, one)) == `1` {
		t.Error("an old value of a kept note reached a non-reader")
	}
}

// Property: over random rosters, frozen grants and audiences, a kept note's
// row is visible iff the reader holds its author grant, or the reader grant
// of exactly that row's audience; adding frozen-reader grants changes
// nothing about any other row.
func TestPropertyKeptNotesFollowOnlyTheirReaders(t *testing.T) {
	kept := []string{"erased:" + fmt.Sprintf("%032x", 1) + ":1", "erased:" + fmt.Sprintf("%032x", 1) + ":2"}
	auds := [][]string{{"guardians"}, {"guardians", "subject"}, {"household"}, {"public"}}
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
			isAuthor, frozen := map[string]bool{}, map[string][]string{}
			for _, k := range kept {
				switch r.Intn(3) {
				case 0:
					g = append(g, roster.Grant{Subject: k, Token: ErasedAuthor})
					isAuthor[k] = true
				case 1:
					frozen[k] = auds[r.Intn(len(auds))]
					g = append(g, roster.Grant{Subject: k, Token: ReaderToken(frozen[k])})
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
					want := isAuthor[k] || frozen[k] != nil && fmt.Sprint(frozen[k]) == fmt.Sprint(aud)
					if Visible(g, k, aud) != want {
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
