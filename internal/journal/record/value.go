// Package record is the record journal's data model without a database:
// typed field values in one canonical encoding, scopes, record types and
// their schema versions, and validation of snapshots and household notes
// (spec 2026-10-16 sections 3, 4.3, 4.7, 4.9a).
package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// FieldType is the type of one field (spec section 3).
type FieldType string

const (
	String     FieldType = "string"
	Text       FieldType = "text" // untrusted free text; scanned (4.11)
	Int        FieldType = "int"
	Number     FieldType = "number"
	Bool       FieldType = "bool"
	Date       FieldType = "date"      // "YYYY-MM-DD"
	Timestamp  FieldType = "timestamp" // RFC 3339, stored in UTC
	StringList FieldType = "string_list"
)

// Valid reports whether t is one of the field types.
func (t FieldType) Valid() bool {
	switch t {
	case String, Text, Int, Number, Bool, Date, Timestamp, StringList:
		return true
	}
	return false
}

// Limits (4.3). A snapshot over any of them is refused whole.
const (
	MaxStringBytes = 1024
	MaxTextBytes   = 64 << 10
	MaxListItems   = 256
)

// Rejection refuses a snapshot or note: the HTTP status and the reason code
// a connector's spool acts on (4.3), and a detail for the operator. A
// detail names fields and record positions, never values.
type Rejection struct {
	Status int
	Reason string
	Detail string
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("%d %s: %s", r.Status, r.Reason, r.Detail)
}

func schemaErr(format string, a ...any) *Rejection {
	return &Rejection{Status: http.StatusUnprocessableEntity, Reason: journalapi.ReasonSchema, Detail: fmt.Sprintf(format, a...)}
}

func tooLarge(format string, a ...any) *Rejection {
	return &Rejection{Status: http.StatusRequestEntityTooLarge, Reason: journalapi.ReasonTooLarge, Detail: fmt.Sprintf(format, a...)}
}

func (r *Rejection) prefixed(p string) *Rejection {
	out := *r
	out.Detail = p + out.Detail
	return &out
}

var (
	nameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	tokenRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// ValidName reports whether s is a valid field, type, rule or scope-part name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidToken reports whether s is a valid audience token.
func ValidToken(s string) bool { return tokenRE.MatchString(s) }

// NormalizeValue checks one JSON value against a field type and returns its
// canonical encoding, or nil for null.
func NormalizeValue(t FieldType, raw json.RawMessage) (json.RawMessage, *Rejection) {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || string(v) == "null" {
		return nil, nil
	}
	switch t {
	case String, Text:
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, schemaErr("want a JSON string")
		}
		if rej := checkString(t, s); rej != nil {
			return nil, rej
		}
		return encode(s), nil
	case Int:
		n, rej := jsonNumber(v)
		if rej != nil {
			return nil, rej
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return nil, schemaErr("want an integer in the int64 range")
		}
		return encode(i), nil
	case Number:
		n, rej := jsonNumber(v)
		if rej != nil {
			return nil, rej
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, schemaErr("want a finite number")
		}
		return encode(f), nil
	case Bool:
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, schemaErr("want true or false")
		}
		return encode(b), nil
	case Date:
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, schemaErr("want a date string")
		}
		if !ValidDate(s) {
			return nil, schemaErr("want a date as YYYY-MM-DD")
		}
		return encode(s), nil
	case Timestamp:
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, schemaErr("want a timestamp string")
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, schemaErr("want an RFC 3339 timestamp")
		}
		return encode(ts.UTC().Format(time.RFC3339Nano)), nil
	case StringList:
		var l []*string
		if err := json.Unmarshal(v, &l); err != nil {
			return nil, schemaErr("want a list of strings")
		}
		if len(l) > MaxListItems {
			return nil, tooLarge("a string_list of %d items exceeds %d", len(l), MaxListItems)
		}
		out := make([]string, len(l))
		for i, s := range l {
			if s == nil {
				return nil, schemaErr("null item in a string_list")
			}
			if rej := checkString(String, *s); rej != nil {
				return nil, rej
			}
			out[i] = *s
		}
		return encode(out), nil
	}
	return nil, schemaErr("unknown field type %q", t)
}

// ValidDate reports whether s is a calendar date written YYYY-MM-DD.
func ValidDate(s string) bool {
	d, err := time.Parse("2006-01-02", s)
	return err == nil && d.Format("2006-01-02") == s
}

func jsonNumber(v []byte) (json.Number, *Rejection) {
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return "", schemaErr("want a number")
	}
	n, ok := x.(json.Number)
	if !ok {
		return "", schemaErr("want a number")
	}
	return n, nil
}

func checkString(t FieldType, s string) *Rejection {
	limit := MaxStringBytes
	if t == Text {
		limit = MaxTextBytes
	}
	if len(s) > limit {
		return tooLarge("a %s value of %d bytes exceeds %d", t, len(s), limit)
	}
	for _, r := range s {
		if r == 0 {
			return schemaErr("NUL character in a value")
		}
		if t != Text && (r < 0x20 || r == 0x7f) {
			return schemaErr("control character in a %s value: free text belongs in text fields", t)
		}
	}
	return nil
}

// encode is the one JSON encoding of every stored value: no HTML escaping,
// no trailing newline.
func encode(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// Encode returns the canonical encoding of a Go string, bool or number;
// used by rules and tests to build values.
func Encode(v any) json.RawMessage { return encode(v) }
