package ocr

import (
	"bytes"
	"errors"
	"log/slog"
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

// TestRegisterEnricher_BinaryMissing exercises the §5.1 graceful-
// degradation path: when LookPath cannot find tesseract, the package
// MUST log the WHAT/CHECK/FIX warning AND MUST NOT register an enricher.
//
// We swap the package-level lookPath, registerWith, and warnLogger
// hooks so the test does not depend on the real PATH or the
// process-global enrich.Default.
func TestRegisterEnricher_BinaryMissing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	registeredCalls := 0
	defer swapHooks(
		func(string) (string, error) { return "", errors.New("not found in $PATH") },
		func(enrich.Enricher) { registeredCalls++ },
		func() *slog.Logger { return logger },
	)()

	if registered := registerEnricher(); registered {
		t.Errorf("registerEnricher() returned true, want false (binary absent)")
	}
	if registeredCalls != 0 {
		t.Errorf("registerWith called %d times, want 0", registeredCalls)
	}

	logText := buf.String()
	// Warning must contain the §5.1 template lines verbatim. Grepping
	// for the lead WHAT line + the CHECK + the FIX is enough.
	for _, want := range []string{
		"enrich/ocr: tesseract not found in PATH",
		"CHECK: docker inspect",
		"FIX:   rebase this connector on glovebox-enricher-runtime",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("warning log missing %q\n--- log ---\n%s", want, logText)
		}
	}
}

// TestRegisterEnricher_BinaryPresent confirms the happy path: when
// LookPath succeeds, the enricher is registered exactly once and no
// warning is emitted.
func TestRegisterEnricher_BinaryPresent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var registered []enrich.Enricher
	defer swapHooks(
		func(name string) (string, error) { return "/usr/bin/" + name, nil },
		func(e enrich.Enricher) { registered = append(registered, e) },
		func() *slog.Logger { return logger },
	)()

	if ok := registerEnricher(); !ok {
		t.Errorf("registerEnricher() returned false, want true (binary present)")
	}
	if len(registered) != 1 {
		t.Fatalf("registerWith called %d times, want 1", len(registered))
	}
	if got := registered[0].Name(); got != "ocr" {
		t.Errorf("registered enricher Name() = %q, want %q", got, "ocr")
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("unexpected warning logged on happy path:\n%s", buf.String())
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

// swapHooks installs test versions of the package-level lookPath,
// registerWith, and warnLogger hooks. The returned restore function
// reinstalls the originals; callers defer it.
func swapHooks(
	lp func(string) (string, error),
	rw func(enrich.Enricher),
	wl func() *slog.Logger,
) func() {
	origLP := lookPath
	origRW := registerWith
	origWL := warnLogger
	lookPath = lp
	registerWith = rw
	warnLogger = wl
	return func() {
		lookPath = origLP
		registerWith = origRW
		warnLogger = origWL
	}
}
