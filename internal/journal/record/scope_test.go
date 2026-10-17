package record

import (
	"net/url"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func parts(kv ...string) []journalapi.ScopePart {
	var out []journalapi.ScopePart
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, journalapi.ScopePart{kv[i], kv[i+1]})
	}
	return out
}

func TestScopeCanonicalAndParts(t *testing.T) {
	s, rej := ParseScope(parts("source", "powerschool", "surface", "assignments", "data_subject", "e_child1", "course", "c1/2=x%"))
	if rej != nil {
		t.Fatal(rej)
	}
	want := "source=powerschool/surface=assignments/data_subject=e_child1/course=c1%2F2%3Dx%25"
	if s.Canonical() != want {
		t.Errorf("canonical %s", s.Canonical())
	}
	if s.Source() != "powerschool" || s.Subject() != "e_child1" {
		t.Errorf("source %q subject %q", s.Source(), s.Subject())
	}
	if v, ok := s.Get("course"); !ok || v != "c1/2=x%" {
		t.Errorf("Get course = %q", v)
	}
	noSubject, _ := ParseScope(parts("source", "gcalendar", "calendar", "family"))
	if noSubject.Subject() != "" {
		t.Error("a scope without a data_subject part has subject \"\"")
	}
}

func TestScopeEscapingIsInjective(t *testing.T) {
	a, _ := ParseScope(parts("source", "x", "a", "b/c=d"))
	b, _ := ParseScope(parts("source", "x", "a", "b", "c", "d"))
	if a.Canonical() == b.Canonical() {
		t.Error("two different scopes share a canonical form")
	}
}

func TestScopeRefusals(t *testing.T) {
	for name, in := range map[string][]journalapi.ScopePart{
		"empty":            nil,
		"source not first": parts("surface", "a", "source", "x"),
		"duplicate part":   parts("source", "x", "course", "1", "course", "2"),
		"bad name":         parts("source", "x", "Course", "1"),
		"empty value":      parts("source", "x", "course", ""),
		"control in value": parts("source", "x", "course", "a\nb"),
		"nul in value":     parts("source", "x", "course", "a\x00b"),
		"invalid utf8":     parts("source", "x", "course", "a\xffb"),
		"c1 control":       parts("source", "x", "course", "a\u0085b"),
		"bidi control":     parts("source", "x", "course", "a\u202eb"),
		"long value":       parts("source", "x", "course", strings.Repeat("v", MaxScopeValueBytes+1)),
		"too many parts":   parts("source", "x", "a", "1", "b", "1", "c", "1", "d", "1", "e", "1", "f", "1", "g", "1", "h", "1"),
	} {
		if _, rej := ParseScope(in); rej == nil || rej.Status != 422 {
			t.Errorf("%s: %v", name, rej)
		}
	}
}

func TestHouseholdScope(t *testing.T) {
	if got := HouseholdScope("attendance_note", "e_child1"); got != "source=household/record_type=attendance_note/data_subject=e_child1" {
		t.Errorf("HouseholdScope = %s", got)
	}
	if got := HouseholdScope("pantry_item", ""); got != "source=household/record_type=pantry_item" {
		t.Errorf("HouseholdScope without subject = %s", got)
	}
}

func TestScopeBoundariesAccepted(t *testing.T) {
	kv := []string{"source", "x"}
	for i := 1; i < MaxScopeParts; i++ {
		kv = append(kv, "p"+strings.Repeat("a", i), "v")
	}
	if _, rej := ParseScope(parts(kv...)); rej != nil {
		t.Errorf("exactly MaxScopeParts parts: %v", rej)
	}
	if _, rej := ParseScope(parts("source", strings.Repeat("v", MaxScopeValueBytes))); rej != nil {
		t.Errorf("exactly MaxScopeValueBytes bytes: %v", rej)
	}
}

func FuzzScopeInjective(f *testing.F) {
	f.Add("a%2F/=b", "c=%", "x")
	f.Fuzz(func(t *testing.T, v1, v2, v3 string) {
		in := parts("source", v1, "a", v2, "b", v3)
		s, rej := ParseScope(in)
		if rej != nil {
			return
		}
		var got []journalapi.ScopePart
		for _, seg := range strings.Split(s.Canonical(), "/") {
			kv := strings.Split(seg, "=")
			if len(kv) != 2 {
				t.Fatalf("bad segment %q", seg)
			}
			k, err1 := url.PathUnescape(kv[0])
			v, err2 := url.PathUnescape(kv[1])
			if err1 != nil || err2 != nil {
				t.Fatalf("unescape %q", seg)
			}
			got = append(got, journalapi.ScopePart{k, v})
		}
		if len(got) != len(in) {
			t.Fatalf("%d parts, want %d", len(got), len(in))
		}
		for i := range in {
			if got[i] != in[i] {
				t.Fatalf("%q != %q", got, in)
			}
		}
	})
}
