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

func findAllOffsets(haystack []byte, needle []byte) []int {
	var positions []int
	offset := 0
	for {
		idx := bytes.Index(haystack[offset:], needle)
		if idx == -1 {
			break
		}
		positions = append(positions, offset+idx)
		offset += idx + len(needle)
	}
	return positions
}

type SubstringMatcher struct{}

func (m SubstringMatcher) Match(content []byte, patterns []string) ([]MatchResult, error) {
	var results []MatchResult
	for _, p := range patterns {
		for _, pos := range findAllOffsets(content, []byte(p)) {
			results = append(results, MatchResult{Pattern: p, Position: pos})
		}
	}
	return results, nil
}

type CaseInsensitiveMatcher struct{}

func (m CaseInsensitiveMatcher) Match(content []byte, patterns []string) ([]MatchResult, error) {
	lower := bytes.ToLower(content)
	var results []MatchResult
	for _, p := range patterns {
		for _, pos := range findAllOffsets(lower, []byte(strings.ToLower(p))) {
			results = append(results, MatchResult{Pattern: p, Position: pos})
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
