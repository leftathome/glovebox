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

	var matched []string
	for _, tp := range templatePatterns {
		if tp.MatchString(text) {
			matched = append(matched, tp.String())
		}
	}

	if len(matched) == 0 {
		return nil, nil
	}

	// If conversational patterns explain all the matches, suppress
	if isFullyConversational(text) {
		return nil, nil
	}

	return []engine.Signal{{
		Name:    "prompt_template_structure",
		Weight:  0.6,
		Matched: fmt.Sprintf("template patterns detected: %s", strings.Join(matched, ", ")),
	}}, nil
}

// isFullyConversational returns true when the text contains conversational
// "you are" phrases but no template patterns that aren't covered by those
// phrases (e.g., <system> tags, markdown headers, delimiter patterns).
func isFullyConversational(text string) bool {
	hasConversational := false
	for _, cp := range conversationalPatterns {
		if cp.MatchString(text) {
			hasConversational = true
			break
		}
	}
	if !hasConversational {
		return false
	}

	// Check for template patterns that are NOT "you are" based
	// (XML tags, markdown headers, delimiters are never conversational)
	for _, tp := range templatePatterns {
		if !tp.MatchString(text) {
			continue
		}
		s := tp.String()
		if !strings.Contains(s, "you\\s+are") && !strings.Contains(s, "your\\s+instructions") {
			return false
		}
	}
	return true
}
