package content

import (
	"strings"
	"testing"
)

func TestDecodeMIME_SimplePlainText(t *testing.T) {
	raw := "From: alice@test.com\r\nTo: bob@test.com\r\nContent-Type: text/plain\r\n\r\nHello world"
	parts, err := DecodeMIME([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].ContentType != "text/plain" {
		t.Errorf("content type = %q", parts[0].ContentType)
	}
	if string(parts[0].Body) != "Hello world" {
		t.Errorf("body = %q", parts[0].Body)
	}
}

func TestDecodeMIME_MultipartAlternative(t *testing.T) {
	raw := "From: alice@test.com\r\n" +
		"Content-Type: multipart/alternative; boundary=boundary123\r\n\r\n" +
		"--boundary123\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"Plain text body\r\n" +
		"--boundary123\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<p>HTML body</p>\r\n" +
		"--boundary123--\r\n"

	parts, err := DecodeMIME([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].ContentType != "text/plain" {
		t.Errorf("part 0 type = %q", parts[0].ContentType)
	}
	if parts[1].ContentType != "text/html" {
		t.Errorf("part 1 type = %q", parts[1].ContentType)
	}
}

func TestDecodeMIME_Base64Encoded(t *testing.T) {
	raw := "From: alice@test.com\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"SGVsbG8gd29ybGQ="

	parts, err := DecodeMIME([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(parts[0].Body) != "Hello world" {
		t.Errorf("decoded body = %q, want 'Hello world'", parts[0].Body)
	}
}

func TestDecodeMIME_QuotedPrintable(t *testing.T) {
	raw := "From: alice@test.com\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Hello=20world"

	parts, err := DecodeMIME([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(parts[0].Body) != "Hello world" {
		t.Errorf("decoded body = %q, want 'Hello world'", parts[0].Body)
	}
}

func TestDecodeMIME_MultipartWithAttachment(t *testing.T) {
	raw := "From: alice@test.com\r\n" +
		"Content-Type: multipart/mixed; boundary=mixbound\r\n\r\n" +
		"--mixbound\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"Message body\r\n" +
		"--mixbound\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n\r\n" +
		"pdf-content-here\r\n" +
		"--mixbound--\r\n"

	parts, err := DecodeMIME([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[1].Filename != "doc.pdf" {
		t.Errorf("filename = %q, want doc.pdf", parts[1].Filename)
	}
}

func TestDecodeMIME_MalformedReturnsError(t *testing.T) {
	_, err := DecodeMIME([]byte("not a valid email at all"))
	// Should either parse as plain text or return error, but not panic
	if err != nil && !strings.Contains(err.Error(), "parse") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDecodeMIME_NoContentType(t *testing.T) {
	raw := "From: alice@test.com\r\n\r\nJust a body"
	parts, err := DecodeMIME([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
}
