package engine

import (
	"bytes"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/text/unicode/norm"
)

type PreprocessedContent struct {
	Original   []byte // Preserved byte-identical
	Normalized []byte // After NFKC + zero-width strip + HTML strip
	RawHTML    []byte // For text/html: normalized but pre-strip (rules run against both)
}

var zeroWidthRunes = []rune{
	0x200B, // zero-width space
	0x200C, // zero-width non-joiner
	0x200D, // zero-width joiner
	0xFEFF, // byte order mark / zero-width no-break space
	0x2060, // word joiner
	0x200E, // left-to-right mark
	0x200F, // right-to-left mark
}

func Preprocess(content []byte, contentType string) PreprocessedContent {
	original := make([]byte, len(content))
	copy(original, content)

	normalized := norm.NFKC.Bytes(content)
	normalized = stripZeroWidth(normalized)

	result := PreprocessedContent{
		Original:   original,
		Normalized: normalized,
	}

	if strings.HasPrefix(contentType, "text/html") {
		// stripHTML reads via bytes.NewReader without mutating, so sharing the slice is safe
		result.RawHTML = normalized
		result.Normalized = stripHTML(normalized)
	}

	return result
}

var zeroWidthSet = func() map[rune]bool {
	m := make(map[rune]bool, len(zeroWidthRunes))
	for _, r := range zeroWidthRunes {
		m[r] = true
	}
	return m
}()

func stripZeroWidth(data []byte) []byte {
	return []byte(strings.Map(func(r rune) rune {
		if zeroWidthSet[r] {
			return -1
		}
		return r
	}, string(data)))
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
