package engine

import (
	"bytes"
	"slices"
	"strings"
	"unicode"
)

// Unicode Tags block. U+E0001 (language tag) plus U+E0020-U+E007F, the
// "tag" mirrors of printable ASCII. These carry a complete invisible ASCII
// channel: a payload written in tag characters renders as nothing at all,
// survives copy/paste, and is reconstructed verbatim by tokenizers that
// map the block back to ASCII. There is no legitimate use in ingested
// content -- the block was deprecated for language tagging in Unicode 5.1
// and only later revived for emoji flag sequences.
const (
	tagRangeLo = 0xE0000
	tagRangeHi = 0xE007F
)

// IsTagChar reports whether r is in the Unicode Tags block.
func IsTagChar(r rune) bool { return r >= tagRangeLo && r <= tagRangeHi }

// IsBidiControl reports whether r is an explicit bidirectional formatting
// control. These reorder rendered text without changing the underlying
// bytes ("Trojan Source"), so what a reviewer sees can differ from what a
// model consumes.
//
// This is Unicode's Bidi_Control property minus the three implicit marks
// (U+061C ALM, U+200E LRM, U+200F RLM): the marks change the direction of
// neutral characters around them but cannot reorder a run of Latin text,
// so they are counted with the zero-width set instead. The tests pin this
// relationship to unicode.Bidi_Control so a new Unicode control cannot be
// added upstream without this list noticing.
func IsBidiControl(r rune) bool {
	switch r {
	case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE RLE PDF LRO RLO
		0x2066, 0x2067, 0x2068, 0x2069: // LRI RLI FSI PDI
		return true
	}
	return false
}

// DefaultIgnorable is Unicode's Default_Ignorable_Code_Point property:
// the code points a renderer is told to display as nothing when it has no
// specific support for them. It is DERIVED at init from Go's own unicode
// tables using the formula published in DerivedCoreProperties.txt, rather
// than hand-listed, so it tracks the Unicode version the toolchain ships
// and cannot drift behind it:
//
//	Other_Default_Ignorable_Code_Point
//	+ Cf (Format characters)
//	+ Variation_Selector
//	- White_Space
//	- FFF9..FFFB (interlinear annotation format characters)
//	- 13430..13440 (Egyptian hieroglyph format characters)
//	- Prepended_Concatenation_Mark (format characters that render visibly)
//
// Go does not export the derived property itself, only its inputs. The
// tests pin the result against the published table and against the
// specific characters that motivated the audit.
var DefaultIgnorable = buildDefaultIgnorable()

// ZeroWidth is the set counted by the encoding anomaly detector as
// "zero-width characters", and the set a consumer that wants to canonicalise
// an identifier should drop: every Default_Ignorable_Code_Point EXCEPT the
// variation selectors.
//
// It covers the zero-width space/joiners, BOM and word joiner, the soft
// hyphen, the bidi marks (LRM, RLM, ALM), the explicit bidi embeddings,
// overrides and isolates (U+202A-U+202E, U+2066-U+2069 -- Trojan Source),
// the invisible math operators (U+2061-U+2064), the deprecated format
// controls (U+206A-U+206F), the Hangul fillers, the combining grapheme
// joiner, the Mongolian vowel separator, the Khmer inherent vowels, the
// shorthand and musical format controls, the Tags block, and the reserved
// code points Unicode pre-assigns as default-ignorable.
//
// Variation selectors (U+180B-U+180D, U+180F, U+FE00-U+FE0F,
// U+E0100-U+E01EF) are deliberately excluded. U+FE0F follows a large share
// of all emoji in real text, U+FE0E/U+FE00-U+FE0D select standardized glyph
// variants, and the ideographic variation sequences are how Japanese
// personal and place names are spelled correctly. Counting them would make
// the detector fire on ordinary chat and on correctly written CJK names.
// They are still stripped before matching (IsInvisible), where removing a
// glyph selector cannot change what a pattern means.
var ZeroWidth = buildZeroWidth()

// IsDefaultIgnorable reports whether r has Default_Ignorable_Code_Point.
func IsDefaultIgnorable(r rune) bool { return unicode.Is(DefaultIgnorable, r) }

// IsZeroWidth reports whether r is in ZeroWidth.
func IsZeroWidth(r rune) bool { return unicode.Is(ZeroWidth, r) }

// IsInvisible reports whether r renders as nothing (or as pure formatting)
// and should therefore be removed before matching.
//
// This is Default_Ignorable_Code_Point (variation selectors included) plus
// every remaining Cf format character. The extra Cf characters -- the
// Arabic number signs, interlinear annotation marks, Egyptian hieroglyph
// format controls -- are not default-ignorable because they can render,
// but none of them belongs inside an ASCII instruction, and removing them
// from the scan-only views is what keeps them from splitting a pattern.
//
// The original implementation stripped only a hand-kept list of seven
// zero-width runes, which left the entire Tags block, the bidi controls,
// the soft hyphen and every other Cf format character intact -- so an
// injection interleaved with them never matched an ASCII pattern.
func IsInvisible(r rune) bool {
	if r < 0x80 {
		return false
	}
	return unicode.Is(DefaultIgnorable, r) || unicode.Is(unicode.Cf, r)
}

// buildDefaultIgnorable applies the DerivedCoreProperties.txt formula to
// Go's unicode tables. The candidate universe is the union of the three
// additive inputs (a few thousand code points), so this is cheap at init.
func buildDefaultIgnorable() *unicode.RangeTable {
	var runes []rune
	for _, tab := range []*unicode.RangeTable{
		unicode.Other_Default_Ignorable_Code_Point,
		unicode.Cf,
		unicode.Variation_Selector,
	} {
		runes = appendTable(runes, tab)
	}
	return tableFromRunes(runes, func(r rune) bool {
		switch {
		case unicode.Is(unicode.White_Space, r):
			return false
		case r >= 0xFFF9 && r <= 0xFFFB:
			return false
		case r >= 0x13430 && r <= 0x13440:
			return false
		case unicode.Is(unicode.Prepended_Concatenation_Mark, r):
			return false
		}
		return true
	})
}

func buildZeroWidth() *unicode.RangeTable {
	return tableFromRunes(appendTable(nil, DefaultIgnorable), func(r rune) bool {
		return !unicode.Is(unicode.Variation_Selector, r)
	})
}

// appendTable appends every code point in tab to runes.
func appendTable(runes []rune, tab *unicode.RangeTable) []rune {
	for _, rg := range tab.R16 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			runes = append(runes, r)
		}
	}
	for _, rg := range tab.R32 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			runes = append(runes, r)
		}
	}
	return runes
}

// tableFromRunes builds a RangeTable of the runes that satisfy keep,
// deduplicated and coalesced into stride-1 ranges.
func tableFromRunes(runes []rune, keep func(rune) bool) *unicode.RangeTable {
	slices.Sort(runes)
	runes = slices.Compact(runes)
	t := &unicode.RangeTable{}
	add := func(lo, hi rune) {
		if hi <= 0xFFFF {
			t.R16 = append(t.R16, unicode.Range16{Lo: uint16(lo), Hi: uint16(hi), Stride: 1})
			if hi <= unicode.MaxLatin1 {
				t.LatinOffset++
			}
			return
		}
		if lo <= 0xFFFF { // split a range that straddles the BMP boundary
			t.R16 = append(t.R16, unicode.Range16{Lo: uint16(lo), Hi: 0xFFFF, Stride: 1})
			lo = 0x10000
		}
		t.R32 = append(t.R32, unicode.Range32{Lo: uint32(lo), Hi: uint32(hi), Stride: 1})
	}
	started := false
	var lo, hi rune
	for _, r := range runes {
		if !keep(r) {
			continue
		}
		switch {
		case !started:
			lo, hi, started = r, r, true
		case r == hi+1:
			hi = r
		default:
			add(lo, hi)
			lo, hi = r, r
		}
	}
	if started {
		add(lo, hi)
	}
	return t
}

// DecodeTagChars recovers the ASCII text smuggled in Unicode Tags
// characters: U+E0020-U+E007E mirror printable ASCII 0x20-0x7E one for
// one, so a payload written in that block renders as nothing but is read
// back verbatim by anything that maps the block down to ASCII.
//
// Returning the decoded text lets the caller feed it to the ordinary
// rules, so an invisible "ignore all previous instructions" trips
// instruction_override exactly as the visible form would.
//
// Returns nil when content carries no tag characters.
func DecodeTagChars(content []byte) []byte {
	var buf bytes.Buffer
	for _, r := range string(content) {
		if !IsTagChar(r) {
			continue
		}
		if r >= 0xE0020 && r <= 0xE007E {
			buf.WriteRune(r - 0xE0000)
		}
	}
	if buf.Len() == 0 {
		return nil
	}
	return buf.Bytes()
}

// CountInvisible returns the number of Tags-block characters and
// bidi controls in content. Used by the detectors to report what was
// smuggled without re-walking the string.
func CountInvisible(content []byte) (tags, bidi int) {
	for _, r := range string(content) {
		switch {
		case IsTagChar(r):
			tags++
		case IsBidiControl(r):
			bidi++
		}
	}
	return tags, bidi
}

// StripInvisible removes every rune IsInvisible reports, returning the
// cleaned bytes and whether anything was removed.
func StripInvisible(data []byte) ([]byte, bool) {
	removed := false
	out := strings.Map(func(r rune) rune {
		if IsInvisible(r) {
			removed = true
			return -1
		}
		return r
	}, string(data))
	return []byte(out), removed
}
