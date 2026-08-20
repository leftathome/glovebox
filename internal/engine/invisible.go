package engine

import (
	"bytes"
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
func IsBidiControl(r rune) bool {
	switch r {
	case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE RLE PDF LRO RLO
		0x2066, 0x2067, 0x2068, 0x2069: // LRI RLI FSI PDI
		return true
	}
	return false
}

// ZeroWidthRunes is the canonical list of zero-width Unicode characters
// stripped during pre-processing and flagged by the encoding anomaly
// detector. Retained as the narrow, named set used by the detector's
// zero-width count; IsInvisible covers the broader stripping surface.
var ZeroWidthRunes = []rune{
	0x200B, // zero-width space
	0x200C, // zero-width non-joiner
	0x200D, // zero-width joiner
	0xFEFF, // byte order mark / zero-width no-break space
	0x2060, // word joiner
	0x200E, // left-to-right mark
	0x200F, // right-to-left mark
}

var ZeroWidthSet = func() map[rune]struct{} {
	m := make(map[rune]struct{}, len(ZeroWidthRunes))
	for _, r := range ZeroWidthRunes {
		m[r] = struct{}{}
	}
	return m
}()

// IsInvisible reports whether r renders as nothing (or as pure formatting)
// and should therefore be removed before matching.
//
// The old implementation stripped only the seven ZeroWidthRunes above,
// which left the entire Tags block, the bidi controls, the soft hyphen and
// every other Cf format character intact -- so an injection interleaved
// with them never matched an ASCII pattern.
func IsInvisible(r rune) bool {
	if _, ok := ZeroWidthSet[r]; ok {
		return true
	}
	if IsTagChar(r) || IsBidiControl(r) {
		return true
	}
	switch r {
	case 0x00AD, // soft hyphen
		0x180E,                         // Mongolian vowel separator
		0x2061, 0x2062, 0x2063, 0x2064, // invisible math operators
		0x115F, 0x1160, // Hangul filler / choseong filler
		0x3164, 0xFFA0: // Hangul filler compatibility forms
		return true
	}
	// Remaining format characters (Cf) and unassigned-but-invisible
	// variation selectors.
	if unicode.Is(unicode.Cf, r) {
		return true
	}
	if r >= 0xFE00 && r <= 0xFE0F { // variation selectors
		return true
	}
	if r >= 0xE0100 && r <= 0xE01EF { // variation selectors supplement
		return true
	}
	return false
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
