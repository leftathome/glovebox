package ocr

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// TestApplies covers the Applies() predicate matrix from the task
// description: every supported image type returns true; unrelated
// types return false; case-insensitivity and parameter stripping work.
func TestApplies(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"png-lowercase", "image/png", true},
		{"jpeg-lowercase", "image/jpeg", true},
		{"tiff-lowercase", "image/tiff", true},
		{"heic-lowercase", "image/heic", true},

		// Case-insensitivity per the task spec.
		{"png-mixed-case", "Image/PNG", true},
		{"jpeg-upper", "IMAGE/JPEG", true},

		// Parameter stripping ("; charset=...").
		{"png-with-charset", "image/png; charset=binary", true},
		{"jpeg-with-params", "image/jpeg;name=photo.jpg", true},
		{"heic-padded", "  image/heic  ", true},

		// Non-image types must NOT match.
		{"pdf-false", "application/pdf", false},
		{"text-false", "text/plain", false},
		{"html-false", "text/html", false},
		{"empty-false", "", false},
		{"image-gif-unsupported", "image/gif", false},
		{"image-webp-unsupported", "image/webp", false},
	}

	e := &Enricher{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := e.Applies(staging.ItemMetadata{ContentType: tc.contentType}, "ignored-path")
			if got != tc.want {
				t.Errorf("Applies(content_type=%q) = %v, want %v",
					tc.contentType, got, tc.want)
			}
		})
	}
}

func TestName(t *testing.T) {
	e := &Enricher{}
	if got := e.Name(); got != "ocr" {
		t.Errorf("Name() = %q, want %q", got, "ocr")
	}
}

// TestRegisterIfAvailable_BinaryMissing exercises the §5.1 graceful-
// degradation path: when LookPath cannot find tesseract, the package MUST
// log the WHAT/CHECK/FIX warning AND MUST NOT register an enricher. We
// drive registerIfAvailable directly with a fresh registry + fake LookPath
// + captured logger, so the test never touches $PATH or enrich.Default.
func TestRegisterIfAvailable_BinaryMissing(t *testing.T) {
	reg := enrich.NewRegistry()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	if registered := registerIfAvailable(reg, func(string) (string, error) {
		return "", errors.New("not found in $PATH")
	}, logger); registered {
		t.Errorf("registerIfAvailable() returned true, want false (binary absent)")
	}

	logText := buf.String()
	for _, want := range []string{
		"enrich/ocr: tesseract not found in PATH",
		"CHECK: docker inspect",
		"FIX:   rebase this connector on glovebox-enricher-runtime",
		"not found in $PATH",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("warning log missing %q\n--- log ---\n%s", want, logText)
		}
	}
}

// TestRegisterIfAvailable_BinaryPresent confirms the happy path: when
// LookPath succeeds, the enricher is registered exactly once and nothing
// is logged.
func TestRegisterIfAvailable_BinaryPresent(t *testing.T) {
	reg := enrich.NewRegistry()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	if ok := registerIfAvailable(reg, func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}, logger); !ok {
		t.Errorf("registerIfAvailable() returned false, want true (binary present)")
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log on happy path:\n%s", buf.String())
	}
}

// TestNormalizeContentType exercises the helper directly so a future
// refactor of the case/parameter-stripping rules surfaces here, not in
// the Applies() table.
func TestNormalizeContentType(t *testing.T) {
	cases := map[string]string{
		"image/png":                 "image/png",
		"Image/PNG":                 "image/png",
		"image/png; charset=binary": "image/png",
		"  image/heic  ":            "image/heic",
		"":                          "",
	}
	for in, want := range cases {
		if got := normalizeContentType(in); got != want {
			t.Errorf("normalizeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOutputFilename pins the §4.3 sidecar-naming rules so a future
// refactor of the helper doesn't silently break the connector pipeline.
func TestOutputFilename(t *testing.T) {
	cases := map[string]string{
		"content.raw":                  "content.extracted.md",
		"/tmp/item/content.raw":        "content.extracted.md",
		"attachment-1-photo.jpg":       "content.attachment-1.extracted.md",
		"/tmp/item/attachment-3-x.png": "content.attachment-3.extracted.md",
		"weird-name.png":               "weird-name.png.extracted.md",
	}
	for in, want := range cases {
		if got := outputFilename(in); got != want {
			t.Errorf("outputFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

