package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// Fields maps field names to canonical values. A null field is absent.
type Fields map[string]json.RawMessage

// Keys returns the field names, sorted.
func (f Fields) Keys() []string {
	out := make([]string, 0, len(f))
	for k := range f {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Hash is SHA-256 over the sorted names and canonical values: equal fields,
// equal hash, whatever the map order.
func (f Fields) Hash() []byte {
	h := sha256.New()
	for _, k := range f.Keys() {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(f[k])
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// Clone returns a copy that shares no map with f.
func (f Fields) Clone() Fields {
	out := make(Fields, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}

// JSON encodes the fields as an object with sorted keys; nil encodes as {}.
func (f Fields) JSON() []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range f.Keys() {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(encode(k))
		buf.WriteByte(':')
		buf.Write(f[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// NormalizeFields checks incoming fields against a type version: every
// field declared, every value valid for its type, every required field
// present. Nulls are dropped.
func NormalizeFields(t Type, in map[string]json.RawMessage) (Fields, *Rejection) {
	out := make(Fields, len(in))
	for _, name := range sortedNames(in) {
		def, ok := t.Fields[name]
		if !ok {
			if !ValidName(name) {
				return nil, schemaErr("invalid field name")
			}
			return nil, schemaErr("a field is not declared by the record type's version")
		}
		v, rej := NormalizeValue(def.Type, in[name])
		if rej != nil {
			return nil, rej.prefixed(fmt.Sprintf("field %q: ", name))
		}
		if v != nil {
			out[name] = v
		}
	}
	for name, def := range t.Fields {
		if def.Required && out[name] == nil {
			return nil, schemaErr("required field %q is missing or null", name)
		}
	}
	return out, nil
}

// Reload decodes fields stored as jsonb and returns every declared value to
// its canonical encoding (PostgreSQL reformats numbers and strings).
// Undeclared names are kept as stored and so enter the content hash; the
// type-sync migration relies on this, having applied Renames first.
func Reload(t Type, stored []byte) (Fields, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(stored, &in); err != nil {
		return nil, fmt.Errorf("record: stored fields: %w", err)
	}
	out := make(Fields, len(in))
	for name, v := range in {
		def, ok := t.Fields[name]
		if !ok {
			out[name] = v
			continue
		}
		nv, rej := NormalizeValue(def.Type, v)
		if rej != nil {
			return nil, fmt.Errorf("record: stored field %q of %s: %s", name, t.Name, rej.Detail)
		}
		if nv != nil {
			out[name] = nv
		}
	}
	return out, nil
}

func sortedNames(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Scan verdicts (4.11). Anything but VerdictPass is withheld on read.
const (
	VerdictPass  = "pass"
	VerdictError = "error" // the scanner failed or timed out: fail closed
)

// ScanEntry is the verdict on one stored text value of record_current.
type ScanEntry struct {
	Verdict        string `json:"verdict"`
	ScannerVersion string `json:"scanner_version"`
}

// ScanMap is record_current.scan: one entry per text field present.
type ScanMap map[string]ScanEntry

// EventScan is the verdicts on one text field of a record_event row: its
// old value, its new value, or both.
type EventScan struct {
	Old            string `json:"old,omitempty"`
	New            string `json:"new,omitempty"`
	ScannerVersion string `json:"scanner_version"`
}
