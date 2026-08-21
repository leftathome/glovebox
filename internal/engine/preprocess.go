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

	decoded := ExtractDecoded(result.Normalized)
	if tagPayload != nil {
		decoded = append(append(tagPayload, '\n'), decoded...)
	}
	result.Decoded = decoded

	return result
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
