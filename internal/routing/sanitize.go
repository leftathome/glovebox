package routing

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxSanitizedChars = 4096

func SanitizeContent(content []byte) []byte {
	var b strings.Builder

	b.WriteString("--- UNTRUSTED QUARANTINED CONTENT (first 4096 chars) ---\n")

	charCount := 0
	for i := 0; i < len(content) && charCount < maxSanitizedChars; {
		r, size := utf8.DecodeRune(content[i:])
		if r == utf8.RuneError && size <= 1 {
			b.WriteString(fmt.Sprintf("\\x%02x", content[i]))
			i++
			charCount++
			continue
		}

		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			b.WriteString(fmt.Sprintf("\\u%04x", r))
		} else if r > 0x7E {
			if r > 0xFFFF {
				b.WriteString(fmt.Sprintf("\\U%08x", r))
			} else {
				b.WriteString(fmt.Sprintf("\\u%04x", r))
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
