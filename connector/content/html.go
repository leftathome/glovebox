package content

import (
	"bytes"
	"io"

	"golang.org/x/net/html"
)

func HTMLToText(htmlContent []byte) []byte {
	tokenizer := html.NewTokenizer(bytes.NewReader(htmlContent))
	var buf bytes.Buffer

	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			if tokenizer.Err() != io.EOF {
				// Malformed HTML -- return what we have
			}
			return buf.Bytes()
		}
		if tt == html.TextToken {
			buf.Write(tokenizer.Text())
		}
	}
}
