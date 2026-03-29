package detector

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/leftathome/glovebox/internal/engine"
)

var base64Pattern = regexp.MustCompile(`[A-Za-z0-9+/]{50,}={0,2}`)

// Uses engine.ZeroWidthSet for membership checks

type EncodingAnomalyDetector struct{}

func (d EncodingAnomalyDetector) Detect(content []byte) ([]engine.Signal, error) {
	var signals []engine.Signal

	if matches := base64Pattern.FindAllIndex(content, -1); len(matches) > 0 {
		var details []string
		for _, m := range matches {
			details = append(details, fmt.Sprintf("base64 block at offset %d, length %d", m[0], m[1]-m[0]))
		}
		signals = append(signals, engine.Signal{
			Name:    "suspicious_encoding",
			Weight:  0.7,
			Matched: strings.Join(details, "; "),
		})
	}

	// Single pass for zero-width chars and unusual unicode
	zwCount := 0
	unusualUnicodeCount := 0
	for _, r := range string(content) {
		if _, ok := engine.ZeroWidthSet[r]; ok {
			zwCount++
		} else if r > 0x7E && !unicode.IsLetter(r) && !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			unusualUnicodeCount++
		}
	}

	if zwCount > 0 {
		signals = append(signals, engine.Signal{
			Name:    "suspicious_encoding",
			Weight:  0.7,
			Matched: fmt.Sprintf("zero-width characters found: %d", zwCount),
		})
	}

	if unusualUnicodeCount > 10 {
		signals = append(signals, engine.Signal{
			Name:    "suspicious_encoding",
			Weight:  0.7,
			Matched: fmt.Sprintf("excessive unusual unicode: %d characters", unusualUnicodeCount),
		})
	}

	return signals, nil
}
