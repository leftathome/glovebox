package record

import (
	"encoding/json"
	"fmt"
	"sort"
)

// OwnerHousehold owns record types written through /v1/notes (4.9a).
const OwnerHousehold = "household"

// MaxAudienceTokens bounds an audience list.
const MaxAudienceTokens = 16

// FieldDef declares one field of a record type.
type FieldDef struct {
	Type     FieldType `json:"type"`
	Required bool      `json:"required,omitempty"`
}

// Type is one schema version of a record type (spec section 3, 4.7).
type Type struct {
	Name          string `json:"name"`
	SchemaVersion int    `json:"schema_version"`
	// Owner is the scope source allowed to write this type, or
	// OwnerHousehold.
	Owner  string              `json:"owner"`
	Fields map[string]FieldDef `json:"fields"`
	// Renames maps the previous version's field names to this version's.
	Renames map[string]string `json:"renames,omitempty"`
	// DefaultAudience is stamped on records that name none (source types).
	DefaultAudience []string `json:"default_audience,omitempty"`
	// FixedAudience is stamped on every record (household types; 4.9a).
	FixedAudience []string `json:"fixed_audience,omitempty"`
	// AllowNoSubject admits records with data_subject "".
	AllowNoSubject bool `json:"allow_no_subject,omitempty"`
}

// Household reports whether the type is written through /v1/notes.
func (t Type) Household() bool { return t.Owner == OwnerHousehold }

// IsText reports whether field is declared as text (scanned, maskable). An
// undeclared field is not text; masking also covers any value that carries a
// scan verdict, whatever its declared type.
func (t Type) IsText(field string) bool { return t.Fields[field].Type == Text }

// Validate checks one type version on its own.
func (t Type) Validate() error {
	if !ValidName(t.Name) {
		return fmt.Errorf("record type %q: invalid name", t.Name)
	}
	where := fmt.Sprintf("record type %s v%d", t.Name, t.SchemaVersion)
	if t.SchemaVersion < 1 {
		return fmt.Errorf("%s: schema_version starts at 1", where)
	}
	if !ValidName(t.Owner) {
		return fmt.Errorf("%s: invalid owner %q", where, t.Owner)
	}
	if len(t.Fields) == 0 {
		return fmt.Errorf("%s: declares no fields", where)
	}
	for name, def := range t.Fields {
		if !ValidName(name) {
			return fmt.Errorf("%s: invalid field name %q", where, name)
		}
		if !def.Type.Valid() {
			return fmt.Errorf("%s: field %s has unknown type %q", where, name, def.Type)
		}
	}
	if t.SchemaVersion == 1 && len(t.Renames) > 0 {
		return fmt.Errorf("%s: version 1 has nothing to rename", where)
	}
	targets := map[string]bool{}
	for from, to := range t.Renames {
		if !ValidName(from) {
			return fmt.Errorf("%s: invalid rename source %q", where, from)
		}
		if _, ok := t.Fields[to]; !ok {
			return fmt.Errorf("%s: rename %s -> %s targets an undeclared field", where, from, to)
		}
		if _, ok := t.Fields[from]; ok {
			return fmt.Errorf("%s: %s is renamed away but still declared", where, from)
		}
		if targets[to] {
			return fmt.Errorf("%s: two fields are renamed to %s", where, to)
		}
		targets[to] = true
	}
	for label, aud := range map[string][]string{"default_audience": t.DefaultAudience, "fixed_audience": t.FixedAudience} {
		if len(aud) > MaxAudienceTokens {
			return fmt.Errorf("%s: %s has more than %d tokens", where, label, MaxAudienceTokens)
		}
		seen := map[string]bool{}
		for _, tok := range aud {
			if !ValidToken(tok) {
				return fmt.Errorf("%s: invalid audience token %q", where, tok)
			}
			if seen[tok] {
				return fmt.Errorf("%s: duplicate audience token %q in %s", where, tok, label)
			}
			seen[tok] = true
		}
	}
	if t.Household() {
		if len(t.FixedAudience) == 0 {
			return fmt.Errorf("%s: a household type declares fixed_audience", where)
		}
		if len(t.DefaultAudience) > 0 {
			return fmt.Errorf("%s: a household type has a fixed audience, not a default one", where)
		}
		if t.Fields["date"].Type != Date {
			return fmt.Errorf("%s: a household type declares a date field of type date (its key is <date>:<on_behalf_of>)", where)
		}
	} else if len(t.FixedAudience) > 0 {
		return fmt.Errorf("%s: only household types have a fixed audience", where)
	}
	return nil
}

// Canonical is a stable encoding of the definition, used to detect a
// stored version whose definition changed.
func (t Type) Canonical() []byte {
	c := t
	if len(c.Renames) == 0 {
		c.Renames = nil
	}
	c.DefaultAudience = sortedCopy(c.DefaultAudience)
	c.FixedAudience = sortedCopy(c.FixedAudience)
	b, _ := json.Marshal(c)
	return b
}

func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// clone returns a deep copy of t.
func (t Type) clone() Type {
	c := t
	c.Fields = make(map[string]FieldDef, len(t.Fields))
	for k, v := range t.Fields {
		c.Fields[k] = v
	}
	if t.Renames != nil {
		c.Renames = make(map[string]string, len(t.Renames))
		for k, v := range t.Renames {
			c.Renames[k] = v
		}
	}
	c.DefaultAudience = append([]string(nil), t.DefaultAudience...)
	c.FixedAudience = append([]string(nil), t.FixedAudience...)
	if len(c.DefaultAudience) == 0 {
		c.DefaultAudience = nil
	}
	if len(c.FixedAudience) == 0 {
		c.FixedAudience = nil
	}
	return c
}

// Registry holds every version of every configured record type. It is
// immutable after NewRegistry: it keeps deep copies of its input and hands
// out deep copies, so no caller can change a type through it.
type Registry struct {
	versions map[string][]Type // index i holds version i+1
}

// NewRegistry validates the types and their version chains.
func NewRegistry(types []Type) (*Registry, error) {
	by := map[string][]Type{}
	for _, t := range types {
		if err := t.Validate(); err != nil {
			return nil, err
		}
		by[t.Name] = append(by[t.Name], t.clone())
	}
	for name, vs := range by {
		sort.Slice(vs, func(i, k int) bool { return vs[i].SchemaVersion < vs[k].SchemaVersion })
		// ever records the type every field name had in any earlier
		// version, so a dropped name cannot return as another type.
		ever := map[string]FieldType{}
		for i, t := range vs {
			if t.SchemaVersion != i+1 {
				return nil, fmt.Errorf("record type %s: versions must run 1..n without gaps or repeats", name)
			}
			if i == 0 {
				for f, d := range t.Fields {
					ever[f] = d.Type
				}
				continue
			}
			prev := vs[i-1]
			if t.Owner != prev.Owner {
				return nil, fmt.Errorf("record type %s: the owner cannot change between versions", name)
			}
			for from, to := range t.Renames {
				old, ok := prev.Fields[from]
				if !ok {
					return nil, fmt.Errorf("record type %s v%d: renames %s, which v%d does not declare", name, t.SchemaVersion, from, prev.SchemaVersion)
				}
				if _, used := ever[to]; used {
					return nil, fmt.Errorf("record type %s v%d: renaming %s onto %s, a name an earlier version used, would merge two fields", name, t.SchemaVersion, from, to)
				}
				if old.Type != t.Fields[to].Type {
					return nil, fmt.Errorf("record type %s v%d: renaming %s to %s changes its type; a type change needs a new field name (4.7)", name, t.SchemaVersion, from, to)
				}
			}
			// 4.7: a type change of an existing field requires a new field
			// name. Masking and rescans key on the field type (4.11), so a
			// text field turned string would be served unmasked.
			for field, def := range t.Fields {
				if old, ok := ever[field]; ok && old != def.Type {
					return nil, fmt.Errorf("record type %s v%d: field %s changes type from %s to %s; a type change needs a new field name (4.7)", name, t.SchemaVersion, field, old, def.Type)
				}
			}
			// A text field is never dropped: its stored values carry scan
			// verdicts and must stay maskable and rescannable.
			for field, def := range prev.Fields {
				if def.Type != Text {
					continue
				}
				if _, renamed := t.Renames[field]; renamed {
					continue
				}
				if _, kept := t.Fields[field]; !kept {
					return nil, fmt.Errorf("record type %s v%d: text field %s cannot be dropped (its stored values keep scan verdicts)", name, t.SchemaVersion, field)
				}
			}
			for f, d := range t.Fields {
				ever[f] = d.Type
			}
		}
		by[name] = vs
	}
	return &Registry{versions: by}, nil
}

// Active returns the newest version of a type.
func (r *Registry) Active(name string) (Type, bool) {
	vs := r.versions[name]
	if len(vs) == 0 {
		return Type{}, false
	}
	return vs[len(vs)-1].clone(), true
}

// Version returns one version of a type.
func (r *Registry) Version(name string, v int) (Type, bool) {
	vs := r.versions[name]
	if v < 1 || v > len(vs) {
		return Type{}, false
	}
	return vs[v-1].clone(), true
}

// Types returns every version of every type, by name then version.
func (r *Registry) Types() []Type {
	names := make([]string, 0, len(r.versions))
	for n := range r.versions {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Type
	for _, n := range names {
		for _, t := range r.versions[n] {
			out = append(out, t.clone())
		}
	}
	return out
}

// ForwardName maps a field name written at version from to the active
// version's name, through the renames of every later version.
func (r *Registry) ForwardName(name string, from int, field string) string {
	vs := r.versions[name]
	if from < 1 {
		from = 1 // out of range: treat as the first version
	}
	for v := from + 1; v <= len(vs); v++ {
		if to, ok := vs[v-1].Renames[field]; ok {
			field = to
		}
	}
	return field
}

// Forward maps every field name of f from version from to the active names.
// Undeclared stored keys keep their names. If one of them collides with the
// target of a rename, the renamed (declared) value wins and the undeclared
// value is dropped, so the result never depends on map order.
func (r *Registry) Forward(name string, from int, f Fields) Fields {
	out := make(Fields, len(f))
	for k, v := range f {
		if r.ForwardName(name, from, k) == k {
			out[k] = v
		}
	}
	for k, v := range f {
		if to := r.ForwardName(name, from, k); to != k {
			out[to] = v
		}
	}
	return out
}
