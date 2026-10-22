// Package policy is the per-service policy of the record journal (spec
// 2026-10-16 section 4.9): what each authenticated ServiceAccount may
// write, whom it may read as, and whether it may count or administer.
// Grants are not here: the journal computes them from the roster for the
// one reader a request names.
//
// Readers "*" and Allows do not consult the roster: the journal must
// confirm that the reader (or on_behalf_of member) is a roster member before
// it calls MayRead, MayCount or MayWriteNote.
//
// A Policy is immutable: New copies its input and Service returns copies.
package policy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// RosterSubjects: writes only for subjects in the current roster (plus ""
// for types that allow it). The only write_subjects mode.
const RosterSubjects = "roster_subjects"

// Readers is "*" (any roster member) or a list of members.
type Readers struct {
	Any bool
	IDs []string
}

// UnmarshalJSON accepts "*" or a list of strings.
func (r *Readers) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != "*" {
			return fmt.Errorf(`readers: want "*" or a list of roster members`)
		}
		r.Any = true
		return nil
	}
	return json.Unmarshal(b, &r.IDs)
}

// MarshalJSON writes what UnmarshalJSON reads.
func (r Readers) MarshalJSON() ([]byte, error) {
	if r.Any {
		return []byte(`"*"`), nil
	}
	return json.Marshal(r.IDs)
}

// Allows reports whether the service may act for member.
func (r Readers) Allows(member string) bool {
	return r.Any || slices.Contains(r.IDs, member)
}

func (r Readers) empty() bool { return !r.Any && len(r.IDs) == 0 }

// Service is one ServiceAccount's policy.
type Service struct {
	Read          bool                `json:"read,omitempty"`
	Readers       Readers             `json:"readers"`
	WriteTypes    []string            `json:"write_types,omitempty"`
	WriteScopes   map[string][]string `json:"write_scopes,omitempty"`
	WriteSubjects string              `json:"write_subjects,omitempty"`
	Counts        bool                `json:"counts,omitempty"`
	Admin         bool                `json:"admin,omitempty"`
}

// UnmarshalJSON refuses unknown keys: a misspelled key in a security policy
// (write_scope for write_scopes) would otherwise vanish and widen the grant.
func (s *Service) UnmarshalJSON(b []byte) error {
	type bare Service
	return strictjson.Decode(b, (*bare)(s))
}

// Parse decodes a policy document strictly (unknown, duplicate and
// case-variant keys are errors, at the top level too) and validates it.
func Parse(raw []byte, reg *record.Registry) (*Policy, error) {
	var s map[string]Service
	if err := strictjson.Decode(raw, &s); err != nil {
		return nil, fmt.Errorf("journal policy: %w", err)
	}
	return New(s, reg)
}

func (s Service) clone() Service {
	s.Readers.IDs = slices.Clone(s.Readers.IDs)
	s.WriteTypes = slices.Clone(s.WriteTypes)
	if s.WriteScopes != nil {
		m := make(map[string][]string, len(s.WriteScopes))
		for k, v := range s.WriteScopes {
			m[k] = slices.Clone(v)
		}
		s.WriteScopes = m
	}
	return s
}

// Policy maps "namespace/serviceaccount" to its Service.
type Policy struct {
	services map[string]Service
}

var callerRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)

// New validates the services against the record types. A caller holding a
// raw JSON document must use Parse, not json.Unmarshal followed by New:
// plain json.Unmarshal into a map lets a repeated or case-variant caller key
// win silently (the last one), which can drop a restricted entry before New
// ever sees it.
func New(in map[string]Service, reg *record.Registry) (*Policy, error) {
	if reg == nil {
		return nil, fmt.Errorf("journal policy: no record types")
	}
	services := make(map[string]Service, len(in))
	for caller, s := range in {
		services[caller] = s.clone()
	}
	for caller, s := range services {
		ns, name, _ := strings.Cut(caller, "/")
		if !callerRE.MatchString(caller) || len(ns) > 63 || len(name) > 253 {
			return nil, fmt.Errorf("journal policy: %q is not namespace/serviceaccount", caller)
		}
		household, snapshots := false, false
		for _, tname := range s.WriteTypes {
			t, ok := reg.Active(tname)
			if !ok {
				return nil, fmt.Errorf("journal policy %s: unknown record type %q", caller, tname)
			}
			if t.Household() {
				household = true
			} else {
				snapshots = true
				if s.WriteSubjects != RosterSubjects {
					return nil, fmt.Errorf("journal policy %s: a writer of %s states write_subjects: %s", caller, tname, RosterSubjects)
				}
			}
		}
		if s.WriteSubjects != "" && s.WriteSubjects != RosterSubjects {
			return nil, fmt.Errorf("journal policy %s: write_subjects must be %s", caller, RosterSubjects)
		}
		if len(s.WriteScopes) > 0 && !snapshots {
			return nil, fmt.Errorf("journal policy %s: write_scopes has no effect without a source write type", caller)
		}
		if !s.Read && !s.Counts && !household && !s.Readers.empty() {
			return nil, fmt.Errorf("journal policy %s: readers has no effect without read, counts or a household write type", caller)
		}
		if !s.Readers.Any {
			seen := map[string]bool{}
			for _, id := range s.Readers.IDs {
				if !roster.ValidID(id) || seen[id] {
					return nil, fmt.Errorf("journal policy %s: readers entry %q is not a unique member id", caller, id)
				}
				seen[id] = true
			}
		}
		for part, vals := range s.WriteScopes {
			if !record.ValidName(part) || len(vals) == 0 {
				return nil, fmt.Errorf("journal policy %s: write_scopes %q needs a valid part name and values", caller, part)
			}
		}
		if (s.Read || s.Counts || household) && s.Readers.empty() {
			return nil, fmt.Errorf("journal policy %s: reads, counts and household writes name their readers", caller)
		}
		if !s.Read && !s.Counts && len(s.WriteTypes) == 0 && !s.Admin {
			return nil, fmt.Errorf("journal policy %s: grants nothing; remove it", caller)
		}
	}
	return &Policy{services: services}, nil
}

// Callers is the TokenReview allow-list: every service with a policy.
func (p *Policy) Callers() []string {
	out := make([]string, 0, len(p.services))
	for c := range p.services {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Service returns a caller's policy.
func (p *Policy) Service(caller string) (Service, bool) {
	s, ok := p.services[caller]
	return s.clone(), ok
}

func denied(format string, a ...any) *record.Rejection {
	return &record.Rejection{Status: http.StatusForbidden, Reason: journalapi.ReasonPolicy, Detail: fmt.Sprintf(format, a...)}
}

// MayWrite decides POST /v1/records for a type and scope.
func (s Service) MayWrite(t record.Type, sc record.Scope) *record.Rejection {
	if t.Household() || !slices.Contains(s.WriteTypes, t.Name) {
		return denied("this service may not write %s snapshots", t.Name)
	}
	if sc.Source() != t.Owner {
		return denied("the scope's source %q is not the owner of %s", sc.Source(), t.Name)
	}
	for part, allowed := range s.WriteScopes {
		v, ok := sc.Get(part)
		if !ok || !slices.Contains(allowed, v) {
			return denied("scope part %s is outside this service's write_scopes", part)
		}
	}
	return nil
}

// WritesSnapshots reports whether the service may write any source (not
// household) record type: only such a service may use POST /v1/records at
// all -- including the step-1 lookup, so others cannot probe snapshot ids.
func (s Service) WritesSnapshots(reg *record.Registry) bool {
	for _, name := range s.WriteTypes {
		if t, ok := reg.Active(name); ok && !t.Household() {
			return true
		}
	}
	return false
}

// MayWriteNote decides POST /v1/notes for a type and its on_behalf_of.
func (s Service) MayWriteNote(t record.Type, onBehalfOf string) *record.Rejection {
	if !t.Household() || !slices.Contains(s.WriteTypes, t.Name) {
		return denied("this service may not write %s", t.Name)
	}
	if !s.Readers.Allows(onBehalfOf) {
		return denied("this service may not write on behalf of that member")
	}
	return nil
}

// MayRead decides GET /v1/state and /v1/events for a reader.
func (s Service) MayRead(reader string) bool { return s.Read && s.Readers.Allows(reader) }

// MayCount decides GET /v1/counts for a reader.
func (s Service) MayCount(reader string) bool { return s.Counts && s.Readers.Allows(reader) }
