package enrich

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSniffContentType_ByExtension covers the common case: attachments
// are staged as "attachment-<n>-<original-filename>", so the original
// extension is recoverable and authoritative for the formats the v1
// enrichers dispatch on. Paths need not exist for the extension path.
func TestSniffContentType_ByExtension(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"attachment-1-report.pdf", "application/pdf"},
		{"attachment-2-photo.jpg", "image/jpeg"},
		{"attachment-3-photo.jpeg", "image/jpeg"},
		{"attachment-4-scan.PNG", "image/png"}, // case-insensitive
		{"attachment-5-fax.tiff", "image/tiff"},
		{"attachment-6-fax.tif", "image/tiff"},
		{"attachment-7-snap.heic", "image/heic"},
		{"attachment-8-sheet.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"attachment-9-deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
		{"attachment-10-memo.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"attachment-11-old.doc", "application/msword"},
		{"attachment-12-old.xls", "application/vnd.ms-excel"},
		{"attachment-13-old.ppt", "application/vnd.ms-powerpoint"},
		{"attachment-14-notes.txt", "text/plain"},
		{"attachment-15-page.html", "text/html"},
		{"attachment-16-page.htm", "text/html"},
		{"attachment-17-readme.md", "text/markdown"},
		// No attachment prefix: a bare path still resolves by extension.
		{"/some/dir/report.pdf", "application/pdf"},
		// Multi-hyphen original filename keeps everything after the ordinal.
		{"attachment-3-my-vacation-photo.jpg", "image/jpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := SniffContentType(tc.path); got != tc.want {
				t.Errorf("SniffContentType(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestSniffContentType_MagicBytesFallback covers attachments whose
// extension is unknown or absent: the function reads the file header and
// falls back to content sniffing.
func TestSniffContentType_MagicBytesFallback(t *testing.T) {
	dir := t.TempDir()

	pdf := filepath.Join(dir, "attachment-1-noext")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4\n1 0 obj\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := SniffContentType(pdf); got != "application/pdf" {
		t.Errorf("magic-byte PDF: got %q, want application/pdf", got)
	}

	text := filepath.Join(dir, "attachment-2-plain")
	if err := os.WriteFile(text, []byte("just some ascii text\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := SniffContentType(text); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("magic-byte text: got %q, want text/plain prefix", got)
	}
}

// TestSniffContentType_Unknown returns empty so the pipeline leaves the
// per-source ContentType blank, letting the passthrough enricher's own
// empty-ContentType sniff decide. Synthesizing a bogus type would risk
// dispatching the wrong enricher.
func TestSniffContentType_Unknown(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "attachment-1-blob")
	// Random non-text, non-magic bytes -> octet-stream -> "".
	if err := os.WriteFile(bin, []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x7f, 0x13}, 0644); err != nil {
		t.Fatal(err)
	}
	if got := SniffContentType(bin); got != "" {
		t.Errorf("unknown binary: got %q, want empty", got)
	}

	// Unreadable / nonexistent path with no known extension -> "".
	if got := SniffContentType("/no/such/attachment-1-mystery"); got != "" {
		t.Errorf("nonexistent: got %q, want empty", got)
	}
}
