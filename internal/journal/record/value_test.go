package record

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestNormalizeValueCanonicalForms(t *testing.T) {
	for _, c := range []struct {
		t        FieldType
		in, want string
	}{
		{String, `"Worksheet <3> & more"`, `"Worksheet <3> & more"`}, // no HTML escaping
		{Text, `"line one\nline two"`, `"line one\nline two"`},
		{Int, `10`, `10`},
		{Int, `-3`, `-3`},
		{Number, `8.50`, `8.5`},
		{Number, `10`, `10`},
		{Number, `1e21`, `1e+21`},
		{Bool, `false`, `false`},
		{Date, `"2026-10-20"`, `"2026-10-20"`},
		{Timestamp, `"2026-10-16T06:00:41-07:00"`, `"2026-10-16T13:00:41Z"`},
		{Timestamp, `"2026-10-16T13:00:41.500Z"`, `"2026-10-16T13:00:41.5Z"`},
		{StringList, `["a","b"]`, `["a","b"]`},
		{Number, `-0`, `0`},
		{Number, `-0.0`, `0`},
		{Int, `-0`, `0`},
		{StringList, `[]`, `[]`},
		{Timestamp, `"0000-01-01T00:00:00Z"`, `"0000-01-01T00:00:00Z"`},
		{Timestamp, `"9999-12-31T23:59:59Z"`, `"9999-12-31T23:59:59Z"`},
		{String, "\"a\\u0085b\\u202ec\\u2067d\"", "\"a\ufffdb\ufffdc\ufffdd\""},
		{StringList, "[\"a\\u009f\"]", "[\"a\ufffd\"]"},
		{Text, "\"a\\u202eb\"", "\"a\u202eb\""},
		{String, `null`, ``},
		{Int, ` null `, ``},
	} {
		got, rej := NormalizeValue(c.t, json.RawMessage(c.in))
		if rej != nil || string(got) != c.want {
			t.Errorf("%s %s: got %s, %v; want %s", c.t, c.in, got, rej, c.want)
		}
	}
}

func TestNormalizeValueRefusals(t *testing.T) {
	long := `"` + strings.Repeat("x", MaxStringBytes+1) + `"`
	longText := `"` + strings.Repeat("x", MaxTextBytes+1) + `"`
	list := `[` + strings.Repeat(`"a",`, MaxListItems) + `"a"]`
	for _, c := range []struct {
		t      FieldType
		in     string
		status int
	}{
		{String, long, 413},
		{Text, longText, 413},
		{StringList, list, 413},
		{String, `"tab\there"`, 422},
		{String, `"nul\u0000"`, 422},
		{Text, `"nul\u0000"`, 422},
		{String, `5`, 422},
		{Int, `8.0`, 422},
		{Int, `"8"`, 422},
		{Int, `9223372036854775808`, 422},
		{Number, `"8.5"`, 422},
		{Bool, `"true"`, 422},
		{Date, `"2026-02-30"`, 422},
		{Date, `"2026-1-5"`, 422},
		{Timestamp, `"2026-10-16 13:00"`, 422},
		{StringList, `["a", null]`, 422},
		{StringList, `"a"`, 422},
		{Timestamp, `"0000-01-01T00:00:00+01:00"`, 422},
		{Timestamp, `"9999-12-31T23:30:00-01:00"`, 422},
		{Number, `1 2`, 422},
		{Int, `1 2`, 422},
		{FieldType("blob"), `"a"`, 422},
	} {
		_, rej := NormalizeValue(c.t, json.RawMessage(c.in))
		if rej == nil || rej.Status != c.status {
			t.Errorf("%s %.40s: %v, want status %d", c.t, c.in, rej, c.status)
		}
	}
}

func TestNamesAndTokens(t *testing.T) {
	for s, ok := range map[string]bool{"title": true, "max_score": true, "a1": true, "": false, "1a": false, "Title": false, "a-b": false, strings.Repeat("a", 65): false} {
		if ValidName(s) != ok {
			t.Errorf("ValidName(%q) != %v", s, ok)
		}
	}
	for s, ok := range map[string]bool{"guardians": true, "public": true, "e_x": true, "": false, "Guardians": false, strings.Repeat("a", 33): false} {
		if ValidToken(s) != ok {
			t.Errorf("ValidToken(%q) != %v", s, ok)
		}
	}
}

func TestEncodePanicsOnUnencodable(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Encode(NaN) did not panic")
		}
	}()
	Encode(math.NaN())
}

func TestEncodeNegativeZero(t *testing.T) {
	if got := string(Encode(math.Copysign(0, -1))); got != "0" {
		t.Errorf("Encode(-0) = %s", got)
	}
}

func FuzzNormalizeIdempotent(f *testing.F) {
	types := []FieldType{String, Text, Int, Number, Bool, Date, Timestamp, StringList}
	for _, s := range []string{`"x"`, `10`, `-0`, `8.50`, `1e21`, `true`, `"2026-10-20"`,
		`"2026-10-16T06:00:41-07:00"`, `["a","b"]`, `[]`, `null`, `"\u0085\u202e"`, `"0000-01-01T00:00:00+01:00"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, ft := range types {
			once, rej := NormalizeValue(ft, json.RawMessage(in))
			if rej != nil || once == nil {
				continue
			}
			twice, rej := NormalizeValue(ft, once)
			if rej != nil || string(twice) != string(once) {
				t.Fatalf("%s %q: once %s, twice %s, %v", ft, in, once, twice, rej)
			}
		}
	})
}
