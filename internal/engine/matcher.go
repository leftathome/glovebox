package engine

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

type MatchResult struct {
	Pattern  string
	Position int
}

type Matcher interface {
	Match(content []byte, patterns []string) ([]MatchResult, error)
}

type SubstringMatcher struct{}

func (m SubstringMatcher) Match(content []byte, patterns []string) ([]MatchResult, error) {
	var results []MatchResult
	for _, p := range patterns {
		pat := []byte(p)
		offset := 0
		for {
			idx := bytes.Index(content[offset:], pat)
			if idx == -1 {
				break
			}
			results = append(results, MatchResult{Pattern: p, Position: offset + idx})
			offset += idx + len(pat)
		}
	}
	return results, nil
}

type CaseInsensitiveMatcher struct{}

func (m CaseInsensitiveMatcher) Match(content []byte, patterns []string) ([]MatchResult, error) {
	lower := bytes.ToLower(content)
	var results []MatchResult
	for _, p := range patterns {
		pat := []byte(strings.ToLower(p))
		offset := 0
		for {
			idx := bytes.Index(lower[offset:], pat)
			if idx == -1 {
				break
			}
			results = append(results, MatchResult{Pattern: p, Position: offset + idx})
			offset += idx + len(pat)
		}
	}
	return results, nil
}

type RegexMatcher struct {
	compiled []*regexp.Regexp
}

func NewRegexMatcher(patterns []string) (*RegexMatcher, error) {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compile pattern %q: %w", p, err)
		}
		compiled[i] = re
	}
	return &RegexMatcher{compiled: compiled}, nil
}

func (m *RegexMatcher) Match(content []byte, _ []string) ([]MatchResult, error) {
	var results []MatchResult
	for _, re := range m.compiled {
		locs := re.FindAllIndex(content, -1)
		for _, loc := range locs {
			results = append(results, MatchResult{Pattern: re.String(), Position: loc[0]})
		}
	}
	return results, nil
}
