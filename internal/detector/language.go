package detector

import (
	"fmt"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/pemistahl/lingua-go"
)

const minContentLength = 20

// minProseLength is how much natural language must survive the structured
// strip before this detector will name a language. It is measured in
// non-whitespace bytes of the prose residue, not in the length of the
// original item, because an item can be kilobytes long and contain almost
// no writing: "Here is the logo inline:" followed by a 2 KiB data: URI is
// 24 characters of English and an attachment.
//
// It is the same 20 as minContentLength on purpose. The bar for "enough
// text to identify" does not change because some of the text turned out to
// be an attachment; what changes is which bytes are counted.
const minProseLength = minContentLength

// AllowSampling opts language detection into prefix+suffix sampling.
//
// Language is a property of the document as a whole: a 128 KB sample
// identifies it as well as the full text, and lingua's model evaluation is
// the expensive part of a scan. Positioning buys an attacker nothing here
// either -- this rule carries weight 0.0 and only multiplies other
// signals, so hiding non-English text outside the window forfeits a
// booster rather than evading a detection.
func (d *LanguageDetectionDetector) AllowSampling() bool { return true }

type LanguageDetectionDetector struct {
	detector lingua.LanguageDetector
}

func NewLanguageDetectionDetector() *LanguageDetectionDetector {
	languages := []lingua.Language{
		lingua.English, lingua.French, lingua.German, lingua.Spanish,
		lingua.Italian, lingua.Portuguese, lingua.Dutch, lingua.Russian,
		lingua.Chinese, lingua.Japanese, lingua.Korean, lingua.Arabic,
		lingua.Turkish, lingua.Polish, lingua.Czech, lingua.Swedish,
	}
	detector := lingua.NewLanguageDetectorBuilder().
		FromLanguages(languages...).
		WithMinimumRelativeDistance(0.25).
		Build()

	return &LanguageDetectionDetector{detector: detector}
}

// Detect names the language of an item's *prose*.
//
// The rule this feeds is a x1.5 weight booster, and what justifies it is
// that the matchers are written in English: an instruction override in
// French walks past every pattern in the ruleset, so whatever signal does
// fire on non-English writing is worth more than the same signal on
// English writing. That argument is about human language. It says nothing
// about a base64 blob, a PEM block or a PGP signature, which are not
// writing in any language at all.
//
// Asked anyway, lingua does not decline: an inline PNG comes back as Dutch
// with confidence 1.00 and a raw base64 payload as Swedish with confidence
// 1.00. The booster then multiplied encoding_anomaly's 0.70 to 1.05 and
// quarantined every inline image and every PGP-signed message that arrived.
// So the armour is removed before the question is asked, and if too little
// writing is left to identify, no language is named and nothing is boosted.
//
// This narrows only the booster. Every matcher, the encoding-anomaly
// detector and the decode-then-scan views still see the whole item,
// armour included -- a payload hidden inside a PGP block is decoded and
// matched exactly as before.
func (d *LanguageDetectionDetector) Detect(content []byte) ([]engine.Signal, error) {
	if len(content) < minContentLength {
		return nil, nil
	}

	prose := engine.StripStructured(content)
	if engine.NonSpaceLen(prose) < minProseLength {
		return nil, nil
	}

	text := string(prose)
	lang, exists := d.detector.DetectLanguageOf(text)
	if !exists {
		return nil, nil
	}

	if lang == lingua.English {
		return nil, nil
	}

	confidence := d.detector.ComputeLanguageConfidence(text, lang)

	return []engine.Signal{{
		Name:    "non_english_content",
		Weight:  0.0,
		Matched: fmt.Sprintf("detected language: %s (confidence: %.2f)", lang.String(), confidence),
	}}, nil
}
