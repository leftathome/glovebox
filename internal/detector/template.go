package detector

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/leftathome/glovebox/internal/engine"
)

var templatePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^you\s+are\s+a\s+`),
	regexp.MustCompile(`(?i)your\s+instructions\s+are`),
	regexp.MustCompile(`(?i)<system>`),
	regexp.MustCompile(`(?i)<instructions>`),
	regexp.MustCompile(`(?i)<prompt>`),
	regexp.MustCompile(`(?i)##\s*(system|instructions|prompt)\b`),
	regexp.MustCompile(`(?i)---\s*BEGIN\s+INSTRUCTIONS\s*---`),
	regexp.MustCompile(`(?i)you\s+are\s+a\s+helpful\s+assistant`),
}

// conversationalPatterns match common non-prompt uses of "you are"
var conversationalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)you\s+are\s+invited`),
	regexp.MustCompile(`(?i)you\s+are\s+welcome`),
	regexp.MustCompile(`(?i)you\s+are\s+correct`),
	regexp.MustCompile(`(?i)you\s+are\s+right`),
	regexp.MustCompile(`(?i)you\s+are\s+the\s+best`),
	regexp.MustCompile(`(?i)hope\s+you\s+are`),
	regexp.MustCompile(`(?i)you\s+are\s+receiving`),
}

type TemplateStructureDetector struct{}

func (d TemplateStructureDetector) Detect(content []byte) ([]engine.Signal, error) {
	text := string(content)

	for _, cp := range conversationalPatterns {
		if cp.Match(content) {
			hasOtherPatterns := false
			for _, tp := range templatePatterns {
				if tp.MatchString(text) && !isConversationalMatch(tp, text) {
					hasOtherPatterns = true
					break
				}
			}
			if !hasOtherPatterns {
				return nil, nil
			}
		}
	}

	var matched []string
	for _, tp := range templatePatterns {
		if tp.MatchString(text) {
			matched = append(matched, tp.String())
		}
	}

	if len(matched) == 0 {
		return nil, nil
	}

	return []engine.Signal{{
		Name:    "prompt_template_structure",
		Weight:  0.6,
		Matched: fmt.Sprintf("template patterns detected: %s", strings.Join(matched, ", ")),
	}}, nil
}

func isConversationalMatch(tp *regexp.Regexp, text string) bool {
	loc := tp.FindStringIndex(text)
	if loc == nil {
		return false
	}
	matchedText := text[loc[0]:loc[1]]
	for _, cp := range conversationalPatterns {
		if cp.MatchString(matchedText) {
			return true
		}
	}
	return false
}
