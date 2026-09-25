package engine

import (
	"testing"
	"unicode"
)

// ucdDefaultIgnorable17 is the Default_Ignorable_Code_Point table copied
// from https://www.unicode.org/Public/17.0.0/ucd/DerivedCoreProperties.txt
// (header "DerivedCoreProperties-17.0.0.txt, Date: 2025-07-30"). Total code
// points: 4174. It is the external reference DefaultIgnorable is derived to
// match; it is NOT used by production code.
var ucdDefaultIgnorable17 = [][2]rune{
	{0x00AD, 0x00AD},   // SOFT HYPHEN
	{0x034F, 0x034F},   // COMBINING GRAPHEME JOINER
	{0x061C, 0x061C},   // ARABIC LETTER MARK
	{0x115F, 0x1160},   // HANGUL CHOSEONG/JUNGSEONG FILLER
	{0x17B4, 0x17B5},   // KHMER VOWEL INHERENT AQ..AA
	{0x180B, 0x180D},   // MONGOLIAN FREE VARIATION SELECTOR ONE..THREE
	{0x180E, 0x180E},   // MONGOLIAN VOWEL SEPARATOR
	{0x180F, 0x180F},   // MONGOLIAN FREE VARIATION SELECTOR FOUR
	{0x200B, 0x200F},   // ZERO WIDTH SPACE..RIGHT-TO-LEFT MARK
	{0x202A, 0x202E},   // LRE..RLO
	{0x2060, 0x2064},   // WORD JOINER..INVISIBLE PLUS
	{0x2065, 0x2065},   // <reserved-2065>
	{0x2066, 0x206F},   // LRI..NOMINAL DIGIT SHAPES
	{0x3164, 0x3164},   // HANGUL FILLER
	{0xFE00, 0xFE0F},   // VARIATION SELECTOR-1..16
	{0xFEFF, 0xFEFF},   // ZERO WIDTH NO-BREAK SPACE
	{0xFFA0, 0xFFA0},   // HALFWIDTH HANGUL FILLER
	{0xFFF0, 0xFFF8},   // <reserved-FFF0>..<reserved-FFF8>
	{0x1BCA0, 0x1BCA3}, // SHORTHAND FORMAT LETTER OVERLAP..UP STEP
	{0x1D173, 0x1D17A}, // MUSICAL SYMBOL BEGIN BEAM..END PHRASE
	{0xE0000, 0xE0000}, // <reserved-E0000>
	{0xE0001, 0xE0001}, // LANGUAGE TAG
	{0xE0002, 0xE001F}, // <reserved>
	{0xE0020, 0xE007F}, // TAG SPACE..CANCEL TAG
	{0xE0080, 0xE00FF}, // <reserved>
	{0xE0100, 0xE01EF}, // VARIATION SELECTOR-17..256
	{0xE01F0, 0xE0FFF}, // <reserved>
}

func inUCD17(r rune) bool {
	for _, rg := range ucdDefaultIgnorable17 {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// TestDefaultIgnorable_MatchesUCD pins the derivation against the published
// table. Every Unicode 17.0 default-ignorable must stay default-ignorable
// under any later toolchain (DICP only grows in practice); while Go ships
// 17.0.0 the match must be exact, code point for code point.
func TestDefaultIgnorable_MatchesUCD(t *testing.T) {
	exact := unicode.Version == "17.0.0"
	if !exact {
		t.Logf("Go ships Unicode %s; checking the 17.0.0 table as a subset only. "+
			"Refresh ucdDefaultIgnorable17 from that version's DerivedCoreProperties.txt.", unicode.Version)
	}
	count := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		got := IsDefaultIgnorable(r)
		want := inUCD17(r)
		if got {
			count++
		}
		if want && !got {
			t.Errorf("U+%04X is Default_Ignorable_Code_Point in UCD 17.0 but not in DefaultIgnorable", r)
		}
		if exact && got && !want {
			t.Errorf("U+%04X is in DefaultIgnorable but not Default_Ignorable_Code_Point in UCD 17.0", r)
		}
	}
	if exact && count != 4174 {
		t.Errorf("DefaultIgnorable has %d code points, UCD 17.0 lists 4174", count)
	}
}

// TestZeroWidth_CoversAuditedGaps pins, by name, every character the
// QUARK-06 audit found missing from the old seven-entry ZeroWidthRunes,
// plus the original seven. Deriving the set from Unicode tables must not
// silently drop any of them.
func TestZeroWidth_CoversAuditedGaps(t *testing.T) {
	want := []struct {
		name string
		r    rune
	}{
		// The original seven.
		{"ZERO WIDTH SPACE", 0x200B},
		{"ZERO WIDTH NON-JOINER", 0x200C},
		{"ZERO WIDTH JOINER", 0x200D},
		{"ZERO WIDTH NO-BREAK SPACE / BOM", 0xFEFF},
		{"WORD JOINER", 0x2060},
		{"LEFT-TO-RIGHT MARK", 0x200E},
		{"RIGHT-TO-LEFT MARK", 0x200F},
		// Named in the audit.
		{"SOFT HYPHEN", 0x00AD},
		{"ARABIC LETTER MARK", 0x061C},
		{"FUNCTION APPLICATION", 0x2061},
		{"INVISIBLE TIMES", 0x2062},
		{"INVISIBLE SEPARATOR", 0x2063},
		{"INVISIBLE PLUS", 0x2064},
		{"LEFT-TO-RIGHT EMBEDDING", 0x202A},
		{"RIGHT-TO-LEFT EMBEDDING", 0x202B},
		{"POP DIRECTIONAL FORMATTING", 0x202C},
		{"LEFT-TO-RIGHT OVERRIDE", 0x202D},
		{"RIGHT-TO-LEFT OVERRIDE", 0x202E},
		{"LEFT-TO-RIGHT ISOLATE", 0x2066},
		{"RIGHT-TO-LEFT ISOLATE", 0x2067},
		{"FIRST STRONG ISOLATE", 0x2068},
		{"POP DIRECTIONAL ISOLATE", 0x2069},
		// Found by deriving from Default_Ignorable_Code_Point.
		{"COMBINING GRAPHEME JOINER", 0x034F},
		{"HANGUL CHOSEONG FILLER", 0x115F},
		{"HANGUL JUNGSEONG FILLER", 0x1160},
		{"KHMER VOWEL INHERENT AQ", 0x17B4},
		{"MONGOLIAN VOWEL SEPARATOR", 0x180E},
		{"INHIBIT SYMMETRIC SWAPPING", 0x206A},
		{"NOMINAL DIGIT SHAPES", 0x206F},
		{"HANGUL FILLER", 0x3164},
		{"HALFWIDTH HANGUL FILLER", 0xFFA0},
		{"SHORTHAND FORMAT LETTER OVERLAP", 0x1BCA0},
		{"MUSICAL SYMBOL BEGIN BEAM", 0x1D173},
		{"LANGUAGE TAG", 0xE0001},
		{"TAG LATIN SMALL LETTER A", 0xE0061},
		{"reserved default-ignorable", 0x2065},
	}
	for _, tc := range want {
		if !IsZeroWidth(tc.r) {
			t.Errorf("IsZeroWidth(%s U+%04X) = false, want true", tc.name, tc.r)
		}
		if !IsInvisible(tc.r) {
			t.Errorf("IsInvisible(%s U+%04X) = false, want true", tc.name, tc.r)
		}
	}
}

// TestZeroWidth_ExcludesVariationSelectors pins the one deliberate
// carve-out: variation selectors are stripped before matching but are not
// counted as zero-width suspicion, because U+FE0F follows ordinary emoji
// and ideographic variation sequences spell CJK names.
func TestZeroWidth_ExcludesVariationSelectors(t *testing.T) {
	for _, r := range []rune{0x180B, 0x180F, 0xFE00, 0xFE0E, 0xFE0F, 0xE0100, 0xE01EF} {
		if IsZeroWidth(r) {
			t.Errorf("IsZeroWidth(U+%04X) = true; variation selectors are excluded", r)
		}
		if !IsDefaultIgnorable(r) {
			t.Errorf("IsDefaultIgnorable(U+%04X) = false, want true", r)
		}
		if !IsInvisible(r) {
			t.Errorf("IsInvisible(U+%04X) = false; variation selectors are still stripped", r)
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if IsZeroWidth(r) != (IsDefaultIgnorable(r) && !unicode.Is(unicode.Variation_Selector, r)) {
			t.Fatalf("ZeroWidth disagrees with DefaultIgnorable minus Variation_Selector at U+%04X", r)
		}
	}
}

// TestZeroWidth_ExcludesVisibleAndWhitespace guards the other direction:
// nothing that renders, and no whitespace, may be counted.
func TestZeroWidth_ExcludesVisibleAndWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    rune
	}{
		{"ascii a", 'a'},
		{"space", ' '},
		{"tab", '\t'},
		{"no-break space", 0x00A0},
		{"line separator", 0x2028},
		{"ideographic space", 0x3000},
		{"hangul syllable ga", 0xAC00},
		{"arabic number sign (prepended concatenation mark)", 0x0600},
		{"interlinear annotation anchor", 0xFFF9},
		{"egyptian hieroglyph vertical joiner", 0x13430},
		{"emoji", 0x1F600},
	} {
		if IsZeroWidth(tc.r) {
			t.Errorf("IsZeroWidth(%s U+%04X) = true, want false", tc.name, tc.r)
		}
	}
}

// TestIsBidiControl_TracksUnicodeBidiControl ties the hand-named explicit
// controls to Unicode's Bidi_Control property: they must be exactly
// Bidi_Control minus the three implicit marks, so a control added in a
// future Unicode version fails here instead of passing unnoticed.
func TestIsBidiControl_TracksUnicodeBidiControl(t *testing.T) {
	marks := map[rune]bool{0x061C: true, 0x200E: true, 0x200F: true}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		inProp := unicode.Is(unicode.Bidi_Control, r)
		want := inProp && !marks[r]
		if IsBidiControl(r) != want {
			t.Errorf("IsBidiControl(U+%04X) = %v, want %v (Bidi_Control=%v)", r, IsBidiControl(r), want, inProp)
		}
		if inProp && !IsZeroWidth(r) {
			t.Errorf("Bidi_Control U+%04X is not in ZeroWidth", r)
		}
	}
}

// TestTableFromRunes_WellFormed checks the invariants unicode.Is relies on:
// sorted, non-overlapping ranges, and a correct LatinOffset.
func TestTableFromRunes_WellFormed(t *testing.T) {
	for name, tab := range map[string]*unicode.RangeTable{"DefaultIgnorable": DefaultIgnorable, "ZeroWidth": ZeroWidth} {
		latin := 0
		for i, rg := range tab.R16 {
			if rg.Lo > rg.Hi || rg.Stride != 1 {
				t.Errorf("%s R16[%d] malformed: %+v", name, i, rg)
			}
			if i > 0 && tab.R16[i-1].Hi >= rg.Lo {
				t.Errorf("%s R16[%d] overlaps or is unsorted", name, i)
			}
			if rg.Hi <= unicode.MaxLatin1 {
				latin++
			}
		}
		for i, rg := range tab.R32 {
			if rg.Lo > rg.Hi || rg.Stride != 1 || rg.Lo <= 0xFFFF {
				t.Errorf("%s R32[%d] malformed: %+v", name, i, rg)
			}
			if i > 0 && tab.R32[i-1].Hi >= rg.Lo {
				t.Errorf("%s R32[%d] overlaps or is unsorted", name, i)
			}
		}
		if tab.LatinOffset != latin {
			t.Errorf("%s LatinOffset = %d, want %d", name, tab.LatinOffset, latin)
		}
	}

	straddle := tableFromRunes([]rune{0x10000, 0xFFFF, 0xFFFE, 0x41, 0x41}, func(rune) bool { return true })
	for _, r := range []rune{0x41, 0xFFFE, 0xFFFF, 0x10000} {
		if !unicode.Is(straddle, r) {
			t.Errorf("tableFromRunes lost U+%04X", r)
		}
	}
	if unicode.Is(straddle, 0x42) {
		t.Error("tableFromRunes invented U+0042")
	}
}

// TestIsInvisible_IsDefaultIgnorablePlusFormat pins the stripping surface
// to its definition over the whole code space.
func TestIsInvisible_IsDefaultIgnorablePlusFormat(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		want := IsDefaultIgnorable(r) || unicode.Is(unicode.Cf, r)
		if IsInvisible(r) != want {
			t.Fatalf("IsInvisible(U+%04X) = %v, want %v", r, IsInvisible(r), want)
		}
	}
}

// TestStripInvisible_TrojanSource strips a Trojan-Source style sample: an
// RLO override and isolates that make a comment render as live code.
func TestStripInvisible_TrojanSource(t *testing.T) {
	in := "/*\u202e } \u2066if (isAdmin)\u2069 \u2066 begin admins only */"
	got, removed := StripInvisible([]byte(in))
	if !removed {
		t.Fatal("expected bidi controls to be removed")
	}
	if want := "/* } if (isAdmin)  begin admins only */"; string(got) != want {
		t.Errorf("StripInvisible() = %q, want %q", got, want)
	}
}

// TestStripInvisible_NewlyCoveredClasses exercises one character from each
// class the derivation added to the stripping surface.
func TestStripInvisible_NewlyCoveredClasses(t *testing.T) {
	for _, r := range []rune{0x034F, 0x17B4, 0x180B, 0x2065, 0xFFF0, 0xE0080, 0xE01F0} {
		in := "ig" + string(r) + "nore"
		got, removed := StripInvisible([]byte(in))
		if !removed || string(got) != "ignore" {
			t.Errorf("StripInvisible(U+%04X) = %q, %v; want \"ignore\", true", r, got, removed)
		}
	}
}
