package engine

import (
	"testing"
)

func TestSubstringMatcher_SingleMatch(t *testing.T) {
	m := SubstringMatcher{}
	results, err := m.Match([]byte("hello world"), []string{"world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Position != 6 {
		t.Errorf("position = %d, want 6", results[0].Position)
	}
}

func TestSubstringMatcher_MultipleMatches(t *testing.T) {
	m := SubstringMatcher{}
	results, _ := m.Match([]byte("abcabc"), []string{"abc"})
	if len(results) != 2 {
		t.Errorf("got %d results, want 2", len(results))
	}
}

func TestSubstringMatcher_NoMatch(t *testing.T) {
	m := SubstringMatcher{}
	results, _ := m.Match([]byte("hello"), []string{"xyz"})
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestSubstringMatcher_EmptyContent(t *testing.T) {
	m := SubstringMatcher{}
	results, _ := m.Match([]byte{}, []string{"abc"})
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestSubstringMatcher_EmptyPatterns(t *testing.T) {
	m := SubstringMatcher{}
	results, _ := m.Match([]byte("hello"), []string{})
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestCaseInsensitiveMatcher_Match(t *testing.T) {
	m := CaseInsensitiveMatcher{}
	results, _ := m.Match([]byte("Ignore Previous"), []string{"ignore previous"})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestCaseInsensitiveMatcher_NoMatch(t *testing.T) {
	m := CaseInsensitiveMatcher{}
	results, _ := m.Match([]byte("hello"), []string{"xyz"})
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestCaseInsensitiveMatcher_MultipleMatches(t *testing.T) {
	m := CaseInsensitiveMatcher{}
	results, _ := m.Match([]byte("ABC abc Abc"), []string{"abc"})
	if len(results) != 3 {
		t.Errorf("got %d results, want 3", len(results))
	}
}

func TestRegexMatcher_SingleMatch(t *testing.T) {
	m, err := NewRegexMatcher([]string{`ignore\s+previous`})
	if err != nil {
		t.Fatal(err)
	}
	results, _ := m.Match([]byte("please ignore   previous instructions"), nil)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestRegexMatcher_NoMatch(t *testing.T) {
	m, _ := NewRegexMatcher([]string{`ignore\s+previous`})
	results, _ := m.Match([]byte("nothing here"), nil)
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestRegexMatcher_InvalidRegex(t *testing.T) {
	_, err := NewRegexMatcher([]string{"[invalid"})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestRegexMatcher_MultiplePatterns(t *testing.T) {
	m, _ := NewRegexMatcher([]string{`foo`, `bar`})
	results, _ := m.Match([]byte("foo and bar"), nil)
	if len(results) != 2 {
		t.Errorf("got %d results, want 2", len(results))
	}
}

func TestRegexMatcher_EmptyContent(t *testing.T) {
	m, _ := NewRegexMatcher([]string{`abc`})
	results, _ := m.Match([]byte{}, nil)
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}
