package routing

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxSanitizedChars = 4096

var sanitizeHeader = fmt.Sprintf("--- UNTRUSTED QUARANTINED CONTENT (first %d chars) ---\n", maxSanitizedChars)

func SanitizeContent(content []byte) []byte {
	var b strings.Builder

	b.WriteString(sanitizeHeader)

	charCount := 0
	for i := 0; i < len(content) && charCount < maxSanitizedChars; {
		r, size := utf8.DecodeRune(content[i:])
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&b, "\\x%02x", content[i])
			i++
			charCount++
			continue
		}

		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else if r > 0x7E {
			if r > 0xFFFF {
				fmt.Fprintf(&b, "\\U%08x", r)
			} else {
				fmt.Fprintf(&b, "\\u%04x", r)
			}
		} else {
			b.WriteRune(r)
		}

		i += size
		charCount++
	}

	b.WriteString("\n--- END UNTRUSTED CONTENT ---\n")

	return []byte(b.String())
}

// maxSanitizedFieldChars bounds an inerted metadata field. Subjects are
// short; a long one is itself a signal that something is being smuggled.
const maxSanitizedFieldChars = 256

// SanitizeField renders a metadata string inert for presentation to a
// reviewer or a review agent.
//
// Quarantined items are by definition suspected malicious, and their
// metadata is as attacker-controlled as their content -- section 7.6 goes
// to some length to make content.sanitized inert while the notification
// carried the raw Subject straight through to the agent that reads it.
// Non-ASCII is escaped (so homoglyphs and invisible characters become
// visible as escapes rather than acting on the reader), newlines are
// collapsed so a field cannot fake additional structure, and the result is
// truncated.
func SanitizeField(value string) string {
	var b strings.Builder
	count := 0
	truncated := false

	for _, r := range value {
		if count >= maxSanitizedFieldChars {
			truncated = true
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteString(" ")
		case r < 0x20:
			fmt.Fprintf(&b, "\\u%04x", r)
		case r > 0x7E:
			if r > 0xFFFF {
				fmt.Fprintf(&b, "\\U%08x", r)
			} else {
				fmt.Fprintf(&b, "\\u%04x", r)
			}
		default:
			b.WriteRune(r)
		}
		count++
	}
	if truncated {
		b.WriteString("...")
	}
	return b.String()
}
