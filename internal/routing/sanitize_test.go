package routing

import (
	"bytes"
	"strings"
	"testing"
)

func TestSanitizeContent_ShortContent(t *testing.T) {
	input := []byte("Hello, this is safe content.")
	result := SanitizeContent(input)
	if !bytes.Contains(result, []byte("Hello, this is safe content.")) {
		t.Errorf("short content should be fully included: %q", result)
	}
	if !bytes.Contains(result, []byte("UNTRUSTED QUARANTINED CONTENT")) {
		t.Error("missing UNTRUSTED header")
	}
}

func TestSanitizeContent_LongContentTruncated(t *testing.T) {
	input := bytes.Repeat([]byte("x"), 10000)
	result := SanitizeContent(input)
	// Count only lowercase 'x' to avoid matching header/footer text
	xCount := bytes.Count(result, []byte("x"))
	if xCount > 4096 {
		t.Errorf("content not truncated: %d chars of x found, want <= 4096", xCount)
	}
	if xCount < 4090 {
		t.Errorf("content too aggressively truncated: %d chars of x found, want ~4096", xCount)
	}
}

func TestSanitizeContent_NonASCIIEscaped(t *testing.T) {
	input := []byte("hello \xc3\xa9 world") // e-acute
	result := SanitizeContent(input)
	if bytes.Contains(result, []byte("\xc3\xa9")) {
		t.Error("non-ASCII byte should be escaped")
	}
	if !bytes.Contains(result, []byte(`\u00e9`)) {
		t.Errorf("expected unicode escape for e-acute: %q", result)
	}
}

func TestSanitizeContent_ControlCharsEscaped(t *testing.T) {
	input := []byte("line1\x00line2\x01line3")
	result := SanitizeContent(input)
	if bytes.Contains(result, []byte{0x00}) {
		t.Error("null byte should be escaped")
	}
}

func TestSanitizeContent_WrappedInMarkers(t *testing.T) {
	result := SanitizeContent([]byte("test"))
	s := string(result)
	if !strings.HasPrefix(s, "--- UNTRUSTED QUARANTINED CONTENT") {
		t.Error("missing header marker")
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "--- END UNTRUSTED CONTENT ---") {
		t.Error("missing footer marker")
	}
}

func TestSanitizeContent_Empty(t *testing.T) {
	result := SanitizeContent([]byte{})
	if !bytes.Contains(result, []byte("UNTRUSTED")) {
		t.Error("empty content should still produce valid wrapper")
	}
}

func TestSanitizeContent_SupplementaryPlaneChars(t *testing.T) {
	input := []byte("hello \xf0\x9f\x98\x80 world") // U+1F600
	result := SanitizeContent(input)
	if bytes.Contains(result, []byte("\xf0\x9f\x98\x80")) {
		t.Error("supplementary plane char should be escaped")
	}
}
