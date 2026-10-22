// Package roster turns the household roster -- the ConfigMap openclaw
// reads, which neither application can modify -- into each member's
// grants (spec 2026-10-16 section 4.9b). It is the only journal code that
// knows household relationships.
package roster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// FormatOpenclawHouseholdV1 is openclaw's household.json (openclaw
// image/internal/household).
const FormatOpenclawHouseholdV1 = "openclaw-household-v1"

// Grant is one (subject, token) pair a reader holds.
type Grant struct {
	Subject string
	Token   string
}

// Roster is a parsed roster: grants per member and the file's hash.
type Roster struct {
	grants map[string][]Grant
	sha    string
}

// Parse reads a roster in the named format.
func Parse(format string, raw []byte) (*Roster, error) {
	switch format {
	case FormatOpenclawHouseholdV1:
		return parseV1(raw)
	}
	return nil, fmt.Errorf("roster: unknown format %q", format)
}

// idPattern is openclaw's entityIDPattern.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type v1Person struct {
	Agent      string   `json:"agent"`
	GuardianOf []string `json:"guardian_of"`
	Siblings   []string `json:"siblings"`
}

type v1Doc struct {
	People           map[string]v1Person `json:"people"`
	HouseholdMembers []string            `json:"household_members"`
}

func parseV1(raw []byte) (*Roster, error) {
	var doc v1Doc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("roster: parse: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("roster: parse: data after the document")
	}
	if len(doc.People) == 0 {
		return nil, fmt.Errorf("roster: no people")
	}
	ids := make([]string, 0, len(doc.People))
	for id := range doc.People {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	agents := map[string]string{}
	for _, id := range ids {
		p := doc.People[id]
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("roster: entity id %q is not path-safe", id)
		}
		if p.Agent == "" {
			return nil, fmt.Errorf("roster: %s has no agent", id)
		}
		if other, dup := agents[p.Agent]; dup {
			return nil, fmt.Errorf("roster: agent %s is bound to both %s and %s", p.Agent, other, id)
		}
		agents[p.Agent] = id
		for _, ref := range append(append([]string(nil), p.GuardianOf...), p.Siblings...) {
			if _, ok := doc.People[ref]; !ok {
				return nil, fmt.Errorf("roster: %s names undefined person %s", id, ref)
			}
		}
	}
	members := map[string]bool{}
	for _, m := range doc.HouseholdMembers {
		if _, ok := doc.People[m]; !ok {
			return nil, fmt.Errorf("roster: household_members names undefined person %s", m)
		}
		members[m] = true
	}
	r := &Roster{grants: map[string][]Grant{}}
	for _, id := range ids {
		p := doc.People[id]
		gs := []Grant{{id, "subject"}}
		for _, c := range p.GuardianOf {
			gs = append(gs, Grant{c, "guardians"})
		}
		if len(p.GuardianOf) > 0 {
			gs = append(gs, Grant{"", "guardians"}) // spec 11 3.4: the household's guardians
		}
		for _, s := range p.Siblings {
			gs = append(gs, Grant{s, "siblings"}) // id is a sibling of s
		}
		if members[id] {
			for m := range members {
				gs = append(gs, Grant{m, "household"})
			}
			gs = append(gs, Grant{"", "household"})
		}
		r.grants[id] = sortGrants(gs)
	}
	sum := sha256.Sum256(raw)
	r.sha = hex.EncodeToString(sum[:])
	return r, nil
}

func sortGrants(gs []Grant) []Grant {
	sort.Slice(gs, func(i, k int) bool {
		if gs[i].Subject != gs[k].Subject {
			return gs[i].Subject < gs[k].Subject
		}
		return gs[i].Token < gs[k].Token
	})
	out := gs[:0]
	for _, gr := range gs {
		if len(out) == 0 || gr != out[len(out)-1] {
			out = append(out, gr)
		}
	}
	return out
}

// Has reports whether id is a member of the roster.
func (r *Roster) Has(id string) bool {
	_, ok := r.grants[id]
	return ok
}

// Grants returns a copy of member's grants.
func (r *Roster) Grants(member string) ([]Grant, bool) {
	gs, ok := r.grants[member]
	if !ok {
		return nil, false
	}
	return append([]Grant(nil), gs...), true
}

// Members lists the roster's members, sorted.
func (r *Roster) Members() []string {
	out := make([]string, 0, len(r.grants))
	for m := range r.grants {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// SHA256 is the hex SHA-256 of the roster file's bytes. openclaw logs the
// same hash of the file it read; equal hashes mean the same roster.
func (r *Roster) SHA256() string { return r.sha }
