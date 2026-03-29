package detector

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/leftathome/glovebox/internal/engine"
)

var base64Pattern = regexp.MustCompile(`[A-Za-z0-9+/]{50,}={0,2}`)

var zeroWidthRunes = map[rune]string{
	0x200B: "ZERO WIDTH SPACE",
	0x200C: "ZERO WIDTH NON-JOINER",
	0x200D: "ZERO WIDTH JOINER",
	0xFEFF: "BYTE ORDER MARK",
	0x2060: "WORD JOINER",
	0x200E: "LEFT-TO-RIGHT MARK",
	0x200F: "RIGHT-TO-LEFT MARK",
}

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

	zwCount := 0
	text := string(content)
	for _, r := range text {
		if _, ok := zeroWidthRunes[r]; ok {
			zwCount++
		}
	}
	if zwCount > 0 {
		signals = append(signals, engine.Signal{
			Name:    "suspicious_encoding",
			Weight:  0.7,
			Matched: fmt.Sprintf("zero-width characters found: %d", zwCount),
		})
	}

	unicodeEscapeCount := 0
	for _, r := range text {
		if r > 0x7E && !unicode.IsLetter(r) && !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			unicodeEscapeCount++
		}
	}
	if unicodeEscapeCount > 10 {
		signals = append(signals, engine.Signal{
			Name:    "suspicious_encoding",
			Weight:  0.7,
			Matched: fmt.Sprintf("excessive unusual unicode: %d characters", unicodeEscapeCount),
		})
	}

	return signals, nil
}
