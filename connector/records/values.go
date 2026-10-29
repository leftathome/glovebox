package records

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// Fields builds one record's fields. Every declared field of the record
// type must be written (null when unknown): the journal decodes strictly.
type Fields struct {
	m map[string]json.RawMessage
	// OnCut is called once for each text value cut to 64 KiB (S8).
	OnCut func()
}

// NewFields returns an empty field set; onCut may be nil.
func NewFields(onCut func()) *Fields { return &Fields{m: map[string]json.RawMessage{}, OnCut: onCut} }

// Map returns the fields for journalapi.Record.Fields.
func (f *Fields) Map() map[string]json.RawMessage { return f.m }

var null = json.RawMessage("null")

// Null stores null.
func (f *Fields) Null(name string) { f.m[name] = null }

// String stores s as one line: every run of whitespace and control
// characters becomes one space, ends trimmed, cut to 1024 bytes on a rune
// boundary. Blank is null. The journal refuses C0 controls in strings
// (422, permanent) and replaces C1 and bidi controls itself.
func (f *Fields) String(name, s string) {
	s = OneLine(s)
	if s == "" {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(s)
}

// OneLine is the cleaning String applies, for keys and scope values built
// from page text.
func OneLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s, _ = cutRunes(s, record.MaxStringBytes)
	return strings.TrimRight(s, " ")
}

// Text stores s as received apart from what JSON and the journal cannot
// carry: invalid UTF-8 and NUL are dropped, and a value over 64 KiB is cut
// on a rune boundary (decision S8). Empty is null.
func (f *Fields) Text(name, s string) {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
	if s == "" {
		f.m[name] = null
		return
	}
	var cut bool
	if s, cut = cutRunes(s, record.MaxTextBytes); cut && f.OnCut != nil {
		f.OnCut()
	}
	f.m[name] = record.Encode(s)
}

// Int stores an integer.
func (f *Fields) Int(name string, v int) { f.m[name] = record.Encode(int64(v)) }

// Bool stores a boolean.
func (f *Fields) Bool(name string, v bool) { f.m[name] = record.Encode(v) }

// OptBool stores *v, or null.
func (f *Fields) OptBool(name string, v *bool) {
	if v == nil {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(*v)
}

// Number stores *v, or null. v must be finite.
func (f *Fields) Number(name string, v *float64) {
	if v == nil {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(*v)
}

// Date stores the calendar date of t as written in t's own location; zero
// is null. Callers pass dates already in the school's calendar.
func (f *Fields) Date(name string, t time.Time) {
	if t.IsZero() {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(t.Format("2006-01-02"))
}

// Timestamp stores t in UTC, RFC 3339; zero is null.
func (f *Fields) Timestamp(name string, t time.Time) {
	if t.IsZero() {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(t.UTC().Format(time.RFC3339))
}

func cutRunes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

const (
	maxKeyBytes        = record.MaxKeyBytes
	maxScopeValueBytes = record.MaxScopeValueBytes
)

// SafeKey returns k when the journal accepts it as a record key (1-512
// bytes, not blank, valid UTF-8, no control or format characters), else
// "h:" + 32 hex digits of its SHA-256. Deterministic, so a record keeps its
// key across polls.
func SafeKey(k string) string { return safe(k, maxKeyBytes) }

// SafeScopeValue is SafeKey for a scope value (1-256 bytes).
func SafeScopeValue(v string) string { return safe(v, maxScopeValueBytes) }

func safe(s string, max int) string {
	if strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unsafeRune) {
		return s
	}
	return "h:" + hash32(s)
}

// unsafeRune is wider than the journal's own check (C0, DEL, C1, bidi and
// zero-width characters, line and paragraph separators): every format
// character is hashed away.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

func hash32(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
