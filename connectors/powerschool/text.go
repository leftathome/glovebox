package powerschool

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Everything the portal returns as text was typed by a third party (a
// teacher, a school clerk) and is untrusted. This file is the one place it
// is made safe to lay out: oneLine for a value that must stay on the line
// its label is on, cleanText / boundText for a block of free text.

// maxTextBlockBytes bounds each teacher-authored text block of an
// assignment item's body (the teacher's comment, the assignment description
// and the category description), each on its own. A longer block is cut at
// a UTF-8 boundary and followed by truncationMarker.
const maxTextBlockBytes = 4 << 10

// maxLineBytes bounds a single-line value: a fact line of a body, a tag
// value, the subject, the sender.
const maxLineBytes = 256

// truncationMarker follows a text block that was cut, here or by the
// library (Assignment.TeacherCommentTruncated).
const truncationMarker = "[truncated]"

// textBlockIndent prefixes every line of a teacher-authored text block.
// The connector's own labels start in column one, so nothing a teacher
// types can pass for one of them.
const textBlockIndent = "  "

// isLineBreak reports whether r ends a line in some renderer: the ASCII
// ones, NEL, and the Unicode line and paragraph separators (categories Zl
// and Zp). All of them are treated as "\n" so that the layout this
// connector decides on is the layout every reader sees.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x85, 0x2028, 0x2029:
		return true
	}
	return unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// scrub maps every rune of untrusted text to what may be laid out: line
// breaks of every kind become "\n", a tab becomes a space, format
// characters (category Cf: bidirectional controls, zero-width spaces and
// joiners, the byte order mark) and all other C0/C1 controls are dropped.
// Invalid UTF-8 is dropped first.
func scrub(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case isLineBreak(r):
			return '\n'
		case r == '\t':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// cutUTF8 cuts s to at most max bytes without splitting a character.
func cutUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// oneLine reduces untrusted text to a single bounded line: scrubbed, every
// line break and run of whitespace turned into one space, the ends
// trimmed, cut to maxLineBytes. Whatever the input, the result contains no
// line break, so it cannot start a line of its own.
func oneLine(s string) string {
	line, _ := cutUTF8(strings.Join(strings.Fields(scrub(s)), " "), maxLineBytes)
	return strings.TrimRightFunc(line, unicode.IsSpace)
}

// cleanText prepares a block of untrusted free text for a text/plain body:
// scrubbed; within each line, runs of whitespace become one space and the
// ends are trimmed; blank lines at either end are dropped and a run of
// blank lines inside becomes one. Line breaks are kept. The words are not
// touched: no markup is parsed and nothing is decoded.
func cleanText(s string) string {
	var lines []string
	blank := true // drops leading blank lines
	for _, line := range strings.Split(scrub(s), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		blank = false
		lines = append(lines, line)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// boundText cleans s and cuts it to maxTextBlockBytes without splitting a
// character. cut reports whether anything was removed.
func boundText(s string) (text string, cut bool) {
	text, cut = cutUTF8(cleanText(s), maxTextBlockBytes)
	if cut {
		text = strings.TrimRightFunc(text, unicode.IsSpace)
	}
	return text, cut
}

// writeTextBlock appends one labelled block of free text, or nothing when
// text is empty. text must come from boundText. Every line with anything
// on it is indented, so no line of a block starts in column one.
func writeTextBlock(b *strings.Builder, label, text string, cut bool) {
	if text == "" {
		return
	}
	b.WriteString("\n" + label + ":\n")
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			b.WriteString(textBlockIndent)
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	if cut {
		b.WriteString(truncationMarker)
		b.WriteString("\n")
	}
}
