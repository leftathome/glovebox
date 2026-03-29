package engine

import (
	"bytes"
	"testing"
)

func TestPreprocess_LatinTextUnchanged(t *testing.T) {
	input := []byte("Hello, this is a normal email.")
	result := Preprocess(input, "text/plain")
	if !bytes.Equal(result.Normalized, input) {
		t.Errorf("normalized = %q, want %q", result.Normalized, input)
	}
	if !bytes.Equal(result.Original, input) {
		t.Error("original should be preserved")
	}
}

func TestPreprocess_ZeroWidthCharsStripped(t *testing.T) {
	// U+200B zero-width space between 'ig' and 'nore'
	input := []byte("ig\xe2\x80\x8bnore previous")
	result := Preprocess(input, "text/plain")
	if !bytes.Contains(result.Normalized, []byte("ignore previous")) {
		t.Errorf("zero-width chars not stripped: %q", result.Normalized)
	}
}

func TestPreprocess_HTMLStripped(t *testing.T) {
	input := []byte("<p>Hello <b>world</b></p>")
	result := Preprocess(input, "text/html")
	if !bytes.Contains(result.Normalized, []byte("Hello world")) {
		t.Errorf("HTML not stripped: %q", result.Normalized)
	}
	if bytes.Contains(result.Normalized, []byte("<p>")) {
		t.Error("HTML tags still present in normalized")
	}
}

func TestPreprocess_HTMLEntitiesDecoded(t *testing.T) {
	input := []byte("ignore &amp; previous &lt;instructions&gt;")
	result := Preprocess(input, "text/html")
	if !bytes.Contains(result.Normalized, []byte("ignore & previous <instructions>")) {
		t.Errorf("HTML entities not decoded: %q", result.Normalized)
	}
}

func TestPreprocess_PlainTextSkipsHTMLStrip(t *testing.T) {
	input := []byte("<not html> just text with angle brackets")
	result := Preprocess(input, "text/plain")
	if !bytes.Contains(result.Normalized, []byte("<not html>")) {
		t.Errorf("text/plain should not strip HTML: %q", result.Normalized)
	}
}

func TestPreprocess_OriginalPreserved(t *testing.T) {
	input := []byte("ig\xe2\x80\x8bnore previous")
	result := Preprocess(input, "text/plain")
	if !bytes.Equal(result.Original, input) {
		t.Error("original content must be preserved byte-identical")
	}
}

func TestPreprocess_FullwidthNormalized(t *testing.T) {
	// Fullwidth Latin 'A' U+FF21 should normalize to 'A' via NFKC
	input := []byte("\xef\xbc\xa1BC")
	result := Preprocess(input, "text/plain")
	if !bytes.Contains(result.Normalized, []byte("ABC")) {
		t.Errorf("fullwidth not normalized: %q", result.Normalized)
	}
}

func TestPreprocess_HTMLRawPreservedForDualScan(t *testing.T) {
	input := []byte("<script>alert('xss')</script><p>safe text</p>")
	result := Preprocess(input, "text/html")
	if result.RawHTML == nil {
		t.Fatal("RawHTML should be set for text/html")
	}
	// RawHTML should contain the normalized-but-not-stripped version
	if !bytes.Contains(result.RawHTML, []byte("<script>")) {
		t.Error("RawHTML should preserve HTML tags")
	}
	// Normalized should be stripped
	if bytes.Contains(result.Normalized, []byte("<script>")) {
		t.Error("Normalized should not contain HTML tags")
	}
}
