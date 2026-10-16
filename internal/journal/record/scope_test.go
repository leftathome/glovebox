package record

import (
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
