package engine

import (
	"bytes"
	"html"
	"strings"
	"unicode/utf8"
)

// UnescapeInPlace decodes escape sequences where they sit, keeping the
// literal text around them.
//
// ExtractDecoded answers "is there an encoded blob here, and what does it
// say" -- it cuts contiguous encoded runs out of the document, decodes
// them in isolation and throws away anything shorter than minDecodedLen.
// That is the right shape for a payload that is *entirely* encoded, and
// the wrong shape for the way encoding actually appears in transported
// text, where a library escapes only the characters it must:
//
//	Ignore%20all%20previous%20instructions
//
// Every word here is legible plain text, so nothing looks like a blob; the
// only encoded runs are lone %20s, each of which decodes to a single space
// and is discarded as too short. Meanwhile the matcher patterns want \s
// between the words and never see one. The document scored 0.00 -- not a
// single signal -- while carrying an override in clear text.
//
// Decoding in place fixes the class rather than the character: the
// separators come back, the words stay where they are, and the ordinary
// instruction patterns match. It covers the three families that reach a
// scanner as text -- percent-encoding, HTML character references and
// backslash escapes -- because all three are used the same way, to spell a
// separator without writing one. Each family is decoded exactly once: a
// doubly-escaped "%2520" stays a literal "%20" rather than becoming a
// space this view invented.
//
// The result is a scan-only buffer; the delivered item is untouched.
// Returns nil, false when nothing was escaped, so callers can skip a
// redundant scan pass.
func UnescapeInPlace(content []byte) ([]byte, bool) {
	out := content
	changed := false

	if d, ok := unescapePercent(out); ok {
		out, changed = d, true
	}
	if d, ok := unescapeBackslash(out); ok {
		out, changed = d, true
	}
	// html.UnescapeString covers the full named-reference table plus the
	// numeric forms, and is a no-op on text that carries no references.
	if bytes.IndexByte(out, '&') >= 0 {
		if s := html.UnescapeString(string(out)); s != string(out) {
			out, changed = []byte(s), true
		}
	}

	if !changed {
		return nil, false
	}
	return out, true
}

// unescapePercent replaces every %XX with the byte it names, leaving
// everything else -- including a bare % -- alone. Bytes rather than runes:
// a multi-byte character escapes as consecutive %XX and reassembles here
// on its own.
func unescapePercent(content []byte) ([]byte, bool) {
	if bytes.IndexByte(content, '%') < 0 {
		return nil, false
	}
	out := make([]byte, 0, len(content))
	changed := false
	for i := 0; i < len(content); i++ {
		if content[i] == '%' && i+2 < len(content) {
			hi, hok := hexNibble(content[i+1])
			lo, lok := hexNibble(content[i+2])
			if hok && lok {
				out = append(out, hi<<4|lo)
				changed = true
				i += 2
				continue
			}
		}
		out = append(out, content[i])
	}
	if !changed || !utf8.Valid(out) {
		// Percent escapes that do not reassemble into text are a binary
		// blob, not a smuggled sentence: ExtractDecoded's isLikelyText
		// gate owns that case.
		return nil, false
	}
	return out, true
}

// unescapeBackslash handles the numeric source-code escapes -- \xHH,
// \uHHHH and \UHHHHHHHH -- as a payload pasted out of a JSON string or a
// shell heredoc arrives spelled.
//
// The single-letter forms (\t, \n, \r) are deliberately *not* decoded.
// They are indistinguishable from an ordinary backslash followed by an
// ordinary letter, so decoding them rewrites `C:\Users\pat\report.docx`
// into a carriage return and mangles every Windows path, LaTeX fragment
// and regex in legitimate mail -- and it does so by *inserting
// whitespace*, which is exactly the thing the matcher patterns are
// looking for. The numeric forms need two to eight hex digits behind the
// marker, so they effectively cannot occur by accident, and they cover
// the same evasion. Buying a rare catch with a common false positive is
// the wrong trade for a scanner that quarantines mail.
func unescapeBackslash(content []byte) ([]byte, bool) {
	if bytes.IndexByte(content, '\\') < 0 {
		return nil, false
	}
	var out strings.Builder
	out.Grow(len(content))
	changed := false
	s := string(content)
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'x', 'u', 'U':
				width := hexEscapeWidth(s[i+1])
				if v, ok := hexValue(s, i+2, width); ok {
					out.WriteRune(rune(v))
					changed = true
					i += 1 + width
					continue
				}
			}
		}
		out.WriteByte(s[i])
	}
	if !changed {
		return nil, false
	}
	return []byte(out.String()), true
}

func hexEscapeWidth(marker byte) int {
	switch marker {
	case 'x':
		return 2
	case 'u':
		return 4
	default: // 'U'
		return 8
	}
}

func hexValue(s string, start, width int) (int, bool) {
	if start+width > len(s) {
		return 0, false
	}
	v := 0
	for i := start; i < start+width; i++ {
		n, ok := hexNibble(s[i])
		if !ok {
			return 0, false
		}
		v = v<<4 | int(n)
	}
	if v > 0x10FFFF {
		return 0, false
	}
	return v, true
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}
