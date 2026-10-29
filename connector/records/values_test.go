package records

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestFieldsStringIsOneCleanLine(t *testing.T) {
	f := NewFields(nil)
	f.String("course", "EXAMPLE  SCIENCE 7\t\n(A)\x00\x07")
	f.String("empty", "  \t ")
	f.String("c1", "a\u0085b")
	m := f.Map()
	if got := string(m["course"]); got != `"EXAMPLE SCIENCE 7 (A)"` {
		t.Fatalf("course = %s", got)
	}
	if got := string(m["empty"]); got != "null" {
		t.Fatalf("blank string = %s, want null", got)
	}
	if got := string(m["c1"]); got != `"a b"` {
		t.Fatalf("C1 control = %s", got)
	}
	f.String("long", strings.Repeat("\u00e9", 2000))
	var s string
	_ = json.Unmarshal(f.Map()["long"], &s)
	if len(s) > record.MaxStringBytes || !utf8.ValidString(s) {
		t.Fatalf("string of %d bytes, valid %v", len(s), utf8.ValidString(s))
	}
}

func TestFieldsTextIsKeptAsReceived(t *testing.T) {
	in := "Line one.\r\n\tLine\u202e two.\u200b\n<b>&</b>"
	f := NewFields(nil)
	f.Text("comment", in)
	var out string
	if err := json.Unmarshal(f.Map()["comment"], &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("text changed: %q, want %q (the journal scans it; connectors do not pre-filter)", out, in)
	}
	f.Text("empty", "")
	if string(f.Map()["empty"]) != "null" {
		t.Fatalf("empty text = %s", f.Map()["empty"])
	}
}

func TestFieldsTextDropsOnlyNULAndInvalidUTF8AndCutsAt64KiB(t *testing.T) {
	f := NewFields(nil)
	f.Text("a", "x\x00y\xffz")
	if string(f.Map()["a"]) != `"xyz"` {
		t.Fatalf("a = %s", f.Map()["a"])
	}
	cut := 0
	f2 := NewFields(func() { cut++ })
	f2.Text("b", strings.Repeat("\u00e9", record.MaxTextBytes)) // 2 bytes each
	var s string
	_ = json.Unmarshal(f2.Map()["b"], &s)
	if len(s) > record.MaxTextBytes || len(s) < record.MaxTextBytes-1 || !utf8.ValidString(s) || cut != 1 {
		t.Fatalf("len %d valid %v cut %d", len(s), utf8.ValidString(s), cut)
	}
	f2.Text("c", strings.Repeat("a", record.MaxTextBytes))
	if cut != 1 {
		t.Fatalf("a value of exactly 64 KiB was cut")
	}
	// A nil callback is fine.
	NewFields(nil).Text("d", strings.Repeat("a", record.MaxTextBytes+1))
}

func TestFieldsTypedValues(t *testing.T) {
	f := NewFields(nil)
	n := 8.5
	yes := true
	f.Number("score", &n)
	f.Number("none", nil)
	f.Int("absences", 2)
	f.Bool("late", true)
	f.OptBool("excused", nil)
	f.OptBool("authorized", &yes)
	f.Null("gone")
	f.Date("due", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	f.Date("undated", time.Time{})
	f.Timestamp("at", time.Date(2026, 10, 20, 15, 4, 5, 0, time.FixedZone("x", -7*3600)))
	f.Timestamp("never", time.Time{})
	want := map[string]string{"score": "8.5", "none": "null", "absences": "2", "late": "true",
		"excused": "null", "authorized": "true", "gone": "null", "due": `"2026-10-20"`, "undated": "null",
		"at": `"2026-10-20T22:04:05Z"`, "never": "null"}
	for k, v := range want {
		if string(f.Map()[k]) != v {
			t.Errorf("%s = %s, want %s", k, f.Map()[k], v)
		}
	}
	if len(f.Map()) != len(want) {
		t.Errorf("fields %v", f.Map())
	}
}

// Every value Fields writes is accepted by the journal's own value check.
func TestFieldsValuesPassTheJournal(t *testing.T) {
	f := NewFields(nil)
	f.String("s", "x\x01y\u0085z\u202e")
	f.Text("t", "a\x07b\u202e\x00"+strings.Repeat("z", record.MaxTextBytes))
	for name, ft := range map[string]record.FieldType{"s": record.String, "t": record.Text} {
		if _, rej := record.NormalizeValue(ft, f.Map()[name]); rej != nil {
			t.Errorf("%s refused: %v", name, rej)
		}
	}
}

func TestSafeKeyAndScopeValue(t *testing.T) {
	if SafeKey("c1:98765") != "c1:98765" {
		t.Fatal("clean key changed")
	}
	k := SafeKey("name:example\nscience|1(A)")
	if !strings.HasPrefix(k, "h:") || len(k) != 34 {
		t.Fatalf("key with a newline = %q", k)
	}
	if SafeKey("name:example\nscience|1(A)") != k {
		t.Fatal("not deterministic")
	}
	if SafeKey("name:example\rscience|1(A)") == k {
		t.Fatal("different keys hash alike")
	}
	if got := SafeKey(strings.Repeat("a", 512)); got != strings.Repeat("a", 512) {
		t.Fatal("a key of exactly 512 bytes was hashed")
	}
	for _, bad := range []string{strings.Repeat("a", 513), "", "  ", "a\u202eb", "a\u0085b", "a\xffb", "a\u2028b", "a\x7fb"} {
		if got := SafeKey(bad); !strings.HasPrefix(got, "h:") {
			t.Errorf("key %q kept", bad)
		}
	}
	if got := SafeScopeValue(strings.Repeat("a", 256)); got != strings.Repeat("a", 256) {
		t.Fatal("a scope value of exactly 256 bytes was hashed")
	}
	if got := SafeScopeValue(strings.Repeat("a", 257)); !strings.HasPrefix(got, "h:") {
		t.Fatal("long scope value kept")
	}
	if got := SafeScopeValue(""); !strings.HasPrefix(got, "h:") {
		t.Fatal("empty scope value must not stay empty")
	}
	// Whatever comes out is accepted by the journal.
	for _, in := range []string{"", "a\nb", strings.Repeat("x", 600), "c1"} {
		if _, rej := record.ParseScope([]journalapi.ScopePart{{"source", SafeScopeValue(in)}}); rej != nil {
			t.Errorf("scope value from %q refused: %v", in, rej)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine(" a \n\n b\x00c "); got != "a b c" {
		t.Fatalf("OneLine = %q", got)
	}
}
