package content

import (
	"bytes"
	"testing"
)

func TestHTMLToText_StripsTags(t *testing.T) {
	result := HTMLToText([]byte("<p>Hello <b>world</b></p>"))
	if !bytes.Contains(result, []byte("Hello world")) {
		t.Errorf("got %q, want 'Hello world'", result)
	}
}

func TestHTMLToText_DecodesEntities(t *testing.T) {
	result := HTMLToText([]byte("&amp; &lt; &gt;"))
	if !bytes.Contains(result, []byte("& < >")) {
		t.Errorf("got %q, want '& < >'", result)
	}
}

func TestHTMLToText_PreservesText(t *testing.T) {
	result := HTMLToText([]byte("plain text no tags"))
	if !bytes.Equal(result, []byte("plain text no tags")) {
		t.Errorf("got %q", result)
	}
}

func TestHTMLToText_MalformedHTML(t *testing.T) {
	result := HTMLToText([]byte("<p>unclosed <b>tag"))
	if !bytes.Contains(result, []byte("unclosed")) {
		t.Errorf("should handle malformed HTML: got %q", result)
	}
}
