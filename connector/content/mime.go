package content

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

type Part struct {
	ContentType string
	Body        []byte
	Filename    string
}

func DecodeMIME(raw []byte) ([]Part, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		body, _ := io.ReadAll(msg.Body)
		return []Part{{ContentType: "text/plain", Body: body}}, nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return parseMultipart(msg.Body, params["boundary"])
	}

	body, err := decodeBody(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return nil, err
	}

	return []Part{{ContentType: mediaType, Body: body}}, nil
}

func parseMultipart(r io.Reader, boundary string) ([]Part, error) {
	if boundary == "" {
		return nil, fmt.Errorf("multipart message with no boundary")
	}

	reader := multipart.NewReader(r, boundary)
	var parts []Part

	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return parts, fmt.Errorf("read part: %w", err)
		}

		ct := p.Header.Get("Content-Type")
		if ct == "" {
			ct = "text/plain"
		}
		mediaType, params, _ := mime.ParseMediaType(ct)

		if strings.HasPrefix(mediaType, "multipart/") {
			nested, err := parseMultipart(p, params["boundary"])
			if err != nil {
				return parts, err
			}
			parts = append(parts, nested...)
			continue
		}

		body, err := decodeBody(p, p.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return parts, err
		}

		filename := ""
		if cd := p.Header.Get("Content-Disposition"); cd != "" {
			_, cdParams, _ := mime.ParseMediaType(cd)
			filename = cdParams["filename"]
		}

		parts = append(parts, Part{
			ContentType: mediaType,
			Body:        body,
			Filename:    filename,
		})
	}

	return parts, nil
}

func decodeBody(r io.Reader, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, r))
		if err != nil {
			return nil, fmt.Errorf("decode base64: %w", err)
		}
		return decoded, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(r))
		if err != nil {
			return nil, fmt.Errorf("decode quoted-printable: %w", err)
		}
		return decoded, nil
	default:
		return io.ReadAll(r)
	}
}
