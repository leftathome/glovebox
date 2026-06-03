package content

import (
	"bytes"
	"io"

	"golang.org/x/net/html"
)

// HTMLToText returns the visible text content of an HTML document.
//
// It uses the standard golang.org/x/net/html tokenizer. Text inside
// <script> and <style> elements is intentionally dropped: these are
// raw-text contexts whose content is code/CSS, never reader-facing
// prose, so including them would just pollute downstream search and
// embedding pipelines. All other element text (including attribute-less
// text like alt fragments rendered as ordinary text) is preserved as-is,
// with HTML entity decoding handled by the tokenizer.
//
// Malformed input is tolerated: on a tokenizer error other than io.EOF
// the function returns whatever text it has already accumulated, so
// callers never have to wrap this in a panic guard.
func HTMLToText(htmlContent []byte) []byte {
	tokenizer := html.NewTokenizer(bytes.NewReader(htmlContent))
	var buf bytes.Buffer

	// skipDepth > 0 means we are inside one or more nested <script> or
	// <style> elements and must drop their TextTokens. Nesting is
	// invalid in real HTML but the tokenizer does not validate, so the
	// counter keeps us robust against tag soup.
	skipDepth := 0

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			if tokenizer.Err() != io.EOF {
				// Malformed HTML -- return what we have so far.
			}
			return buf.Bytes()
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			if bytes.Equal(name, []byte("script")) || bytes.Equal(name, []byte("style")) {
				skipDepth++
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			if bytes.Equal(name, []byte("script")) || bytes.Equal(name, []byte("style")) {
				if skipDepth > 0 {
					skipDepth--
				}
			}
		case html.TextToken:
			if skipDepth == 0 {
				buf.Write(tokenizer.Text())
			}
		}
	}
}
