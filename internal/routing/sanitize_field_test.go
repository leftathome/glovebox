package routing

import (
	"strings"
	"testing"
)

func TestSanitizeField(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain ascii unchanged", "Re: lunch tomorrow", "Re: lunch tomorrow"},
		{"newlines collapsed", "Subject\nfake: header", "Subject fake: header"},
		{"tab collapsed", "a\tb", "a b"},
		{"cyrillic escaped", "ign\u043ere", `ign\u043ere`},
		{"zero width escaped", "ig\u200bnore", `ig\u200bnore`},
		{"control chars escaped", "a\x01b", `a\u0001b`},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeField(tc.input); got != tc.want {
				t.Errorf("SanitizeField(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeField_Truncates(t *testing.T) {
	got := SanitizeField(strings.Repeat("a", maxSanitizedFieldChars+50))
	if !strings.HasSuffix(got, "...") {
		t.Error("long field was not truncated")
	}
	if len(got) > maxSanitizedFieldChars+3 {
		t.Errorf("truncated length = %d, want <= %d", len(got), maxSanitizedFieldChars+3)
	}
}

// The notification is read by the agent that summarises hostile items, so a
// Subject carrying an injection must not arrive as live text.
func TestSanitizeField_InertsSmuggledSubject(t *testing.T) {
	// "ignore" written in Unicode Tags characters: invisible on screen,
	// read back verbatim by a model.
	subject := "Re: invoice\U000E0069\U000E0067\U000E006E\U000E006F\U000E0072\U000E0065"

	got := SanitizeField(subject)
	if strings.ContainsRune(got, 0xE0069) {
		t.Errorf("tag characters survived inerting: %q", got)
	}
	if !strings.Contains(got, `\U000e0069`) {
		t.Errorf("expected escaped tag characters, got %q", got)
	}
}
