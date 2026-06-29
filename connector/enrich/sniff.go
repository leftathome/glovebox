package enrich

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// attachmentOrdinalRE strips the connector's "attachment-<n>-" prefix so
// the original filename (and therefore its extension) is recoverable.
// Connectors stage attachments as "attachment-<n>-<original-filename>"
// (see docs/specs/14 §4.3 and the enrichers' filename mapping).
var attachmentOrdinalRE = regexp.MustCompile(`^attachment-\d+-`)

// extContentType maps a lower-cased file extension to the canonical MIME
// type the v1 enrichers dispatch on. It is intentionally explicit for
// the formats glovebox enriches — the OOXML office types in particular,
// which http.DetectContentType reports as application/zip. Extensions not
// listed here fall through to magic-byte detection.
//
// Keep the office types in sync with connector/enrich/office and the
// image types in sync with connector/enrich/ocr's supportedTypes.
var extContentType = map[string]string{
	".pdf":  "application/pdf",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".heic": "image/heic",
	".html": "text/html",
	".htm":  "text/html",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".doc":  "application/msword",
	".xls":  "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
}

// SniffContentType returns a best-effort MIME content type for a staging
// source file. It is used by the commit pipeline to dispatch enrichers
// per attachment: the item-level ContentType describes only the primary
// body (e.g. text/html for a newsletter), so attachments must be typed
// from their own filename/bytes or they would all inherit the body's
// type (spec 14 §7.3 / glovebox-afq4.17).
//
// Resolution order:
//  1. Known extension (after stripping any "attachment-<n>-" prefix).
//  2. Magic-byte detection via http.DetectContentType.
//
// Returns "" when the type cannot be determined. An empty result is
// deliberate: it lets the passthrough enricher's own empty-ContentType
// text sniff decide, and avoids dispatching the wrong enricher on a
// guessed type.
func SniffContentType(path string) string {
	name := attachmentOrdinalRE.ReplaceAllString(filepath.Base(path), "")
	ext := strings.ToLower(filepath.Ext(name))
	if ct, ok := extContentType[ext]; ok {
		return ct
	}
	return sniffMagic(path)
}

// sniffMagic reads the file header and classifies it with
// http.DetectContentType. It returns "" for unreadable files and for the
// application/octet-stream catch-all (i.e. "unknown").
func sniffMagic(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// 512 bytes is the window http.DetectContentType inspects.
	var buf [512]byte
	n, _ := f.Read(buf[:])
	ct := http.DetectContentType(buf[:n])
	if ct == "application/octet-stream" {
		return ""
	}
	return ct
}
