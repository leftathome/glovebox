package engine

import (
	"bytes"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/text/unicode/norm"
)

// PreprocessedContent holds the scan-only views of an item's content.
//
// Original is always preserved byte-identical (spec 04 section 1.1): every
// other field exists so the matchers see what a reader -- or a model --
// sees, not the transport bytes. The optional fields are nil when they
// would duplicate Normalized, letting the scanner skip redundant passes.
type PreprocessedContent struct {
	Original   []byte // Preserved byte-identical
	Normalized []byte // After NFKC + invisible strip + HTML strip
	RawHTML    []byte // For text/html: normalized but pre-strip (rules run against both)

	// PreScrub is the NFKC'd content *before* invisible characters were
	// removed. Detectors run against it so they can flag the presence of
	// smuggled invisibles; without it the scrub upstream of the detector
	// meant the "invisible characters found" signal could never fire in
	// the real pipeline.
	PreScrub []byte

	// Folded is the homoglyph-folded skeleton of Normalized. Matchers run
	// against it so Cyrillic/Greek lookalikes cannot carry an injection
	// past ASCII patterns. Kept separate from Normalized so language
	// detection still sees the true script of the content.
	Folded []byte

	// Decoded holds text recovered from embedded base64/hex/percent
	// payloads, so an encoded injection is scanned rather than merely
	// flagged.
	Decoded []byte

	// Unescaped is Normalized with percent escapes, HTML character
	// references and backslash escapes decoded *in place*. Decoded
	// handles a payload that is entirely encoded; this handles the far
	// commoner shape where only the separators are escaped
	// ("Ignore%20all%20previous"), which leaves every word legible and
	// every \s in a matcher pattern unsatisfied.
	Unescaped []byte

	// Reordered is the text as a renderer lays it out, after applying the
	// UAX #9 explicit-embedding rules that the bidi controls ask for.
	// Stripping those controls stops them interleaving a payload, but
	// leaves the text they govern stored backwards, so a reversed
	// injection inside an RLO override renders forwards to a human and
	// stays unreadable to every matcher.
	Reordered []byte
}

func Preprocess(content []byte, contentType string) PreprocessedContent {
	original := make([]byte, len(content))
	copy(original, content)

	nfkc := norm.NFKC.Bytes(content)

	// Recover any Tags-block payload before it is scrubbed away, so the
	// ordinary rules get a chance to match what was hidden.
	tagPayload := DecodeTagChars(nfkc)

	scrubbed, removed := StripInvisible(nfkc)

	result := PreprocessedContent{
		Original:   original,
		Normalized: scrubbed,
	}
	preScrub := nfkc

	if strings.HasPrefix(contentType, "text/html") {
		// stripHTML reads via bytes.NewReader without mutating, so sharing the slice is safe
		result.RawHTML = scrubbed
		result.Normalized = stripHTML(scrubbed)
		// Keep PreScrub in the same shape as Normalized so the detector
		// pass over it differs by invisibles alone. Handing detectors raw
		// HTML here instead would change what template_structure and
		// language_detection see, and a spurious language boost (x1.5) is
		// enough to push a benign 0.6 signal over the threshold.
		preScrub = stripHTML(nfkc)
	}
	if removed {
		result.PreScrub = preScrub
	}

	result.Folded = FoldConfusables(result.Normalized)

	// The remaining views are built from the same shape as Normalized and
	// finished by derivedView, so a payload that combines an escape or a
	// bidi override with homoglyphs is still matched -- each hardening
	// composes with the others instead of being a separate door.
	if unescaped, ok := UnescapeInPlace(result.Normalized); ok {
		result.Unescaped = derivedView(unescaped)
	}
	// Reordering reads the bidi controls, so it runs on the pre-scrub text
	// and the scrub runs over its output. The view is kept only when the
	// reordering actually moved something, so content whose controls
	// changed nothing costs no extra pass.
	if reordered, ok := ReorderBidi(preScrub); ok {
		if clean, _ := StripInvisible(reordered); !bytes.Equal(clean, result.Normalized) {
			result.Reordered = derivedView(clean)
		}
	}

	decoded := ExtractDecoded(result.Normalized)
	if tagPayload != nil {
		decoded = append(append(tagPayload, '\n'), decoded...)
	}
	result.Decoded = decoded

	return result
}

// derivedView finishes a scan-only view built by unescaping or
// reordering: it gets the same NFKC normalization and homoglyph folding
// the primary view got, so the hardening layers compose instead of each
// being its own separate door.
//
// Normalizing again is not redundant. Decoding hands back characters that
// were not in the content before: "&nbsp;" is U+00A0, which no matcher's
// \s accepts until NFKC maps it onto an ordinary space, and a payload
// spelled with fullwidth or non-breaking separators would otherwise walk
// straight back out of the view built to catch it.
//
// FoldConfusables returns nil for plain ASCII, and a nil view means "skip
// this pass" to the scanner, so fall back to the unfolded bytes.
func derivedView(view []byte) []byte {
	view = norm.NFKC.Bytes(view)
	if folded := FoldConfusables(view); folded != nil {
		return folded
	}
	return view
}

func stripHTML(data []byte) []byte {
	tokenizer := html.NewTokenizer(bytes.NewReader(data))
	var buf bytes.Buffer

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return buf.Bytes()
			}
			return buf.Bytes()
		case html.TextToken:
			buf.Write(tokenizer.Text())
		}
	}
}
