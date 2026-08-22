package engine

import (
	"bytes"
	"strings"
	"unicode"
)

// Explicit bidirectional formatting controls (UAX #9). These are the only
// characters that can make Latin text render in an order other than the
// order it is stored in, which is exactly what the "Trojan Source" class
// of injection relies on: the payload is written backwards, wrapped in an
// override, and renders forwards to anyone who looks at it.
const (
	runeLRE = 0x202A
	runeRLE = 0x202B
	runePDF = 0x202C
	runeLRO = 0x202D
	runeRLO = 0x202E
	runeLRI = 0x2066
	runeRLI = 0x2067
	runeFSI = 0x2068
	runePDI = 0x2069
)

// maxBidiDepth is UAX #9's max_depth: embeddings deeper than this are
// ignored rather than pushed, which also bounds the stack an attacker can
// force us to allocate.
const maxBidiDepth = 125

type bidiFrame struct {
	level   int
	isolate bool
}

// ReorderBidi returns content as a reader sees it rendered, with the
// explicit bidi controls consumed and the text they govern reordered.
//
// StripInvisible removes bidi controls, which stops them interleaving a
// payload past an ASCII pattern -- but deletion leaves the *governed text*
// exactly as stored. An injection written in reverse inside an RLO..PDF
// override therefore renders as "Ignore all previous instructions" to
// every human who inspects it and stays stored as ".snoitcurtsni ..." for
// every matcher that looks at it. The scanner saw an encoding anomaly and
// nothing else. This builds the view the renderer builds.
//
// This implements the explicit-level part of UAX #9 -- the X rules that
// assign embedding levels from LRE/RLE/LRO/RLO/LRI/RLI/FSI/PDF/PDI, then
// rule L2's reversal of each maximal run at or above every level from the
// highest down to the lowest odd one. The implicit rules (W*, N*, I*) that
// resolve levels for unmarked Arabic and Hebrew are deliberately not
// implemented: without an explicit control, strong-LTR characters such as
// ASCII letters can never be reordered, so no injection can hide there,
// and leaving natural RTL prose untouched keeps this off the
// false-positive surface entirely. Mirroring (L4) is skipped for the same
// reason -- it changes bracket glyphs, never word order.
//
// The result is a scan-only buffer; the delivered item is untouched.
// Returns nil, false when content has no explicit controls to act on, so
// callers can skip a redundant scan pass.
func ReorderBidi(content []byte) ([]byte, bool) {
	if !bytes.ContainsRune(content, runeRLE) &&
		!bytes.ContainsRune(content, runeLRE) &&
		!bytes.ContainsRune(content, runeRLO) &&
		!bytes.ContainsRune(content, runeLRO) &&
		!bytes.ContainsRune(content, runeRLI) &&
		!bytes.ContainsRune(content, runeLRI) &&
		!bytes.ContainsRune(content, runeFSI) {
		return nil, false
	}

	// Bidi state does not survive a paragraph break, so reorder each line
	// on its own and keep the line structure the matchers rely on.
	lines := strings.Split(string(content), "\n")
	changed := false
	for i, line := range lines {
		out, did := reorderParagraph(line)
		if did {
			changed = true
			lines[i] = out
		}
	}
	if !changed {
		return nil, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

// reorderParagraph applies the explicit-level rules to one paragraph and
// returns its rendered order.
func reorderParagraph(para string) (string, bool) {
	runes := make([]rune, 0, len(para))
	levels := make([]int, 0, len(para))

	// Paragraph level 0: content arriving over mail and web transports is
	// LTR-framed. An attacker who wants RTL has to say so with a control,
	// which is precisely the case handled below.
	level := 0
	stack := []bidiFrame{{level: 0}}

	push := func(newLevel int, isolate bool) {
		if newLevel > maxBidiDepth || len(stack) >= maxBidiDepth {
			return
		}
		stack = append(stack, bidiFrame{level: newLevel, isolate: isolate})
		level = newLevel
	}
	nextOdd := func(l int) int {
		if l%2 == 0 {
			return l + 1
		}
		return l + 2
	}
	nextEven := func(l int) int {
		if l%2 == 0 {
			return l + 2
		}
		return l + 1
	}

	src := []rune(para)
	for i := 0; i < len(src); i++ {
		r := src[i]
		switch r {
		case runeRLE, runeRLO:
			push(nextOdd(level), false)
		case runeLRE, runeLRO:
			push(nextEven(level), false)
		case runeRLI:
			push(nextOdd(level), true)
		case runeLRI:
			push(nextEven(level), true)
		case runeFSI:
			// FSI takes the direction of the first strong character up to
			// its matching PDI, so resolve it rather than assuming LTR --
			// an FSI-wrapped payload is otherwise a free bypass of an
			// RLI-only implementation.
			if firstStrongIsRTL(src[i+1:]) {
				push(nextOdd(level), true)
			} else {
				push(nextEven(level), true)
			}
		case runePDF:
			// PDF terminates an embedding or override, never an isolate.
			if n := len(stack); n > 1 && !stack[n-1].isolate {
				stack = stack[:n-1]
				level = stack[len(stack)-1].level
			}
		case runePDI:
			// PDI closes the innermost isolate, discarding any embeddings
			// left unterminated inside it. A PDI with no matching isolate
			// initiator does nothing at all.
			if hasIsolate(stack) {
				for n := len(stack); n > 1; n = len(stack) {
					top := stack[n-1]
					stack = stack[:n-1]
					if top.isolate {
						break
					}
				}
				level = stack[len(stack)-1].level
			}
		default:
			runes = append(runes, r)
			levels = append(levels, level)
		}
	}

	maxLevel, minOdd := 0, maxBidiDepth+2
	for _, l := range levels {
		if l > maxLevel {
			maxLevel = l
		}
		if l%2 == 1 && l < minOdd {
			minOdd = l
		}
	}
	if maxLevel == 0 || minOdd > maxLevel {
		// Controls were present but governed nothing that renders in a
		// different order (an LTR embedding, or an override with no text).
		// Dropping them alone is what StripInvisible already does.
		return "", false
	}

	// UAX #9 rule L2, applied to the output while the ranges are taken
	// from the logical levels.
	for lvl := maxLevel; lvl >= minOdd; lvl-- {
		for i := 0; i < len(levels); i++ {
			if levels[i] < lvl {
				continue
			}
			j := i
			for j < len(levels) && levels[j] >= lvl {
				j++
			}
			reverseRunes(runes[i:j])
			i = j
		}
	}
	return string(runes), true
}

// firstStrongIsRTL reports the direction FSI resolves to: the direction of
// the first strong character before the matching PDI.
func firstStrongIsRTL(rest []rune) bool {
	depth := 0
	for _, r := range rest {
		switch r {
		case runeLRI, runeRLI, runeFSI:
			depth++
			continue
		case runePDI:
			if depth == 0 {
				return false
			}
			depth--
			continue
		}
		if depth > 0 {
			continue
		}
		switch {
		case unicode.In(r, unicode.Hebrew, unicode.Arabic, unicode.Syriac,
			unicode.Thaana, unicode.Nko, unicode.Samaritan, unicode.Mandaic):
			return true
		case unicode.IsLetter(r):
			return false
		}
	}
	return false
}

func hasIsolate(stack []bidiFrame) bool {
	for _, f := range stack {
		if f.isolate {
			return true
		}
	}
	return false
}

func reverseRunes(r []rune) {
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
}
