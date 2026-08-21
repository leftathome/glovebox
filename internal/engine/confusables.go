package engine

import (
	"bytes"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// confusables maps visually-confusable non-ASCII letters to the ASCII
// letter they impersonate.
//
// NFKC (applied earlier in Preprocess) folds *compatibility* characters --
// fullwidth forms, mathematical alphanumerics, ligatures -- but it has no
// mapping from Cyrillic/Greek/Cherokee lookalikes to Latin: Cyrillic "о"
// (U+043E) and Latin "o" are distinct characters that NFKC leaves alone.
// An attacker swapping one letter per keyword ("ignоre all previоus
// instructiоns") therefore evades every ASCII matcher while a human -- and
// an LLM -- reads the original phrase.
//
// This table is deliberately curated rather than generated from the full
// UTS-39 confusables set: it covers the script pairs actually used in
// homoglyph attacks against English instruction keywords, and every entry
// maps to a single ASCII rune so folding cannot change string length in a
// way that breaks offset reporting for the caller. Entries are one-way;
// the folded buffer is used for matching only and is never delivered.
var confusables = map[rune]rune{
	// Cyrillic uppercase
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K',
	'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P',
	'С': 'C', 'Т': 'T', 'У': 'Y', 'Х': 'X',
	'І': 'I', 'Ј': 'J', 'Ѕ': 'S',
	// Cyrillic lowercase
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p',
	'с': 'c', 'у': 'y', 'х': 'x', 'м': 'm',
	'т': 't', 'і': 'i', 'ј': 'j', 'ѕ': 's',
	'һ': 'h', 'ӏ': 'l', 'ԁ': 'd', 'ԛ': 'q',
	'ԝ': 'w', 'н': 'h', 'в': 'b',
	// Greek uppercase
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z',
	'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M',
	'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T',
	'Υ': 'Y', 'Χ': 'X',
	// Greek lowercase
	'α': 'a', 'ε': 'e', 'ι': 'i', 'κ': 'k',
	'ο': 'o', 'ρ': 'p', 'τ': 't', 'ν': 'v',
	'γ': 'y', 'υ': 'u',
	// Armenian
	'օ': 'o', 'ո': 'n', 'ս': 's', 'գ': 'q',
	// Cherokee
	'Ꭺ': 'A', 'Ꭻ': 'B', 'Ꭿ': 'C', 'Ꭰ': 'D',
	'Ꭼ': 'E', 'Ꮃ': 'W', 'Ꮋ': 'G', 'Ꮐ': 'V',
	// Latin-script lookalikes outside ASCII
	'ı': 'i', // dotless i
	'ɑ': 'a', // latin alpha
	'ɡ': 'g', // script g
	'ɪ': 'I', // small capital I
	'ǀ': 'l', // dental click
	'ⱥ': 'a',
	'ɐ': 'a',
	// Slash lookalikes (NFKC leaves these alone; they read as "/")
	'⁄': '/', // fraction slash
	'∕': '/', // division slash
}

// FoldConfusables produces the "skeleton" form of content used for
// homoglyph-resistant matching: combining marks are removed and
// confusable letters are folded to the ASCII letter they impersonate.
//
// The result is a scan-only buffer. Per the design invariant (spec 04
// section 1.1) the original content is never modified and is delivered
// byte-identical; folding exists so the matchers see what a reader sees.
//
// Returns nil when folding is a no-op (the overwhelmingly common case of
// plain ASCII content), so callers can skip a redundant scan pass.
func FoldConfusables(content []byte) []byte {
	if isASCII(content) {
		return nil
	}

	decomposed := norm.NFD.Bytes(content)

	var buf bytes.Buffer
	buf.Grow(len(decomposed))
	changed := false

	for _, r := range string(decomposed) {
		// Drop nonspacing marks: defeats "i̇gnore"-style evasion where a
		// combining diacritic is stacked onto an ASCII letter.
		if unicode.Is(unicode.Mn, r) {
			changed = true
			continue
		}
		if folded, ok := confusables[r]; ok {
			buf.WriteRune(folded)
			changed = true
			continue
		}
		buf.WriteRune(r)
	}

	if !changed && bytes.Equal(buf.Bytes(), content) {
		return nil
	}
	return buf.Bytes()
}

func isASCII(content []byte) bool {
	for _, b := range content {
		if b >= 0x80 {
			return false
		}
	}
	return true
}
