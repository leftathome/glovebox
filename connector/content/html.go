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
