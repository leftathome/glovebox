package record

import (
	"strings"
	"unicode/utf8"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Scope parts with a meaning to the journal.
const (
	PartSource  = "source"
	PartSubject = "data_subject"
)

// Scope limits.
const (
	MaxScopeParts      = 8
	MaxScopeValueBytes = 256
)

// Scope is a validated scope (4.3). The order of its parts is part of its
// identity and is kept as given (never sorted). Values are compared
// byte-for-byte with no Unicode normalisation: normalising would merge
// byte-distinct values, against the refuse-don't-merge rule. The zero Scope{}
// has an empty canonical form and is never valid input.
type Scope struct {
	parts []journalapi.ScopePart
}

// ParseScope validates a scope: it starts with source, names are valid and
// distinct, values are non-empty, short and free of control characters.
func ParseScope(in []journalapi.ScopePart) (Scope, *Rejection) {
	if len(in) == 0 || len(in) > MaxScopeParts {
		return Scope{}, schemaErr("a scope has 1 to %d parts", MaxScopeParts)
	}
	seen := map[string]bool{}
	for i, p := range in {
		name, value := p[0], p[1]
		if !ValidName(name) {
			return Scope{}, schemaErr("scope part %d: invalid name", i)
		}
		if seen[name] {
			return Scope{}, schemaErr("scope part %q appears twice", name)
		}
		seen[name] = true
		if i == 0 && name != PartSource {
			return Scope{}, schemaErr("a scope starts with its source part")
		}
		if value == "" || len(value) > MaxScopeValueBytes {
			return Scope{}, schemaErr("scope part %q: value must be 1 to %d bytes", name, MaxScopeValueBytes)
		}
		if !utf8.ValidString(value) {
			return Scope{}, schemaErr("scope part %q: not valid UTF-8", name)
		}
		for _, r := range value {
			// Scope values become stored keys, so unlike field values they
			// are refused, not cleaned, when they carry C0/C1 or bidi controls.
			if r < 0x20 || r == 0x7f || suspicious(r) {
				return Scope{}, schemaErr("scope part %q: control character", name)
			}
		}
	}
	return Scope{parts: append([]journalapi.ScopePart(nil), in...)}, nil
}

var scopeEscaper = strings.NewReplacer("%", "%25", "/", "%2F", "=", "%3D")

// EscapeScopeValue escapes the three characters that structure a canonical
// scope or group key.
func EscapeScopeValue(s string) string { return scopeEscaper.Replace(s) }

// Canonical is the stored form: name=value parts joined by "/".
func (s Scope) Canonical() string {
	var b strings.Builder
	for i, p := range s.parts {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(EscapeScopeValue(p[0]))
		b.WriteByte('=')
		b.WriteString(EscapeScopeValue(p[1]))
	}
	return b.String()
}

// Get returns the value of a named part.
func (s Scope) Get(name string) (string, bool) {
	for _, p := range s.parts {
		if p[0] == name {
			return p[1], true
		}
	}
	return "", false
}

// Source is the value of the source part.
func (s Scope) Source() string { v, _ := s.Get(PartSource); return v }

// Subject is the value of the data_subject part, "" when there is none.
func (s Scope) Subject() string { v, _ := s.Get(PartSubject); return v }

// HouseholdScope is the canonical scope of a household record (notes have
// no snapshot, but every record has a scope). Callers must already have
// validated recordType and subject; this function does not.
func HouseholdScope(recordType, subject string) string {
	p := []journalapi.ScopePart{{PartSource, OwnerHousehold}, {"record_type", recordType}}
	if subject != "" {
		p = append(p, journalapi.ScopePart{PartSubject, subject})
	}
	return Scope{parts: p}.Canonical()
}
