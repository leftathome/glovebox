package detector

import (
	"fmt"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/pemistahl/lingua-go"
)

const minContentLength = 20

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

func (d *LanguageDetectionDetector) Detect(content []byte) ([]engine.Signal, error) {
	if len(content) < minContentLength {
		return nil, nil
	}

	text := string(content)
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
