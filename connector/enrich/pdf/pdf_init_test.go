package pdf

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// TestApplies_PDF_True confirms the enricher claims any application/pdf
// content type (case-insensitive, parameters tolerated).
func TestApplies_PDF_True(t *testing.T) {
	e := &Enricher{}
	cases := []string{
		"application/pdf",
		"Application/PDF",
		"application/pdf; charset=binary",
		"APPLICATION/PDF;name=foo.pdf",
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			meta := staging.ItemMetadata{ContentType: ct}
			if !e.Applies(meta, "anything") {
				t.Errorf("Applies(%q) = false, want true", ct)
			}
		})
	}
}

// TestApplies_NonPDF_False confirms the enricher declines other content
// types. We do NOT sniff bytes — dispatch is content-type only per spec §5.
func TestApplies_NonPDF_False(t *testing.T) {
	e := &Enricher{}
	cases := []string{
		"text/html",
		"text/plain",
		"image/png",
		"application/json",
		"application/octet-stream",
		"",
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			meta := staging.ItemMetadata{ContentType: ct}
			if e.Applies(meta, "anything") {
				t.Errorf("Applies(%q) = true, want false", ct)
			}
		})
	}
}

// TestName confirms the stable identifier.
func TestName(t *testing.T) {
	e := &Enricher{}
	if got := e.Name(); got != "pdf" {
		t.Errorf("Name() = %q, want %q", got, "pdf")
	}
}

// TestRegisterIfAvailable_BinaryPresent registers when LookPath returns
// success.
func TestRegisterIfAvailable_BinaryPresent(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	reg := enrich.NewRegistry()

	fakeLook := func(name string) (string, error) {
		if name != "pdftotext" {
			t.Fatalf("LookPath called with %q, want pdftotext", name)
		}
		return "/usr/bin/pdftotext", nil
	}

	registered := registerIfAvailable(reg, fakeLook, logger)
	if !registered {
		t.Fatalf("registerIfAvailable returned false, want true")
	}

	// Confirm the registry has the enricher: build a dispatch we know
	// will match.
	arts, errs := reg.ApplyAll(testCtx(t), "irrelevant.pdf", staging.ItemMetadata{ContentType: "application/pdf"}, t.TempDir())
	// We can't actually run pdftotext in this test environment without
	// the binary; we just confirm the registry HAS a member that
	// matches. The Enrich call will exec and fail (because the path is
	// fake), so we expect an errs slice with one entry OR an empty arts
	// slice. Either confirms the registration took.
	if len(arts) == 0 && len(errs) == 0 {
		t.Errorf("registry contains no matching enricher; arts=%d errs=%d", len(arts), len(errs))
	}

	if buf.Len() != 0 {
		t.Errorf("expected no log output when binary present, got: %s", buf.String())
	}
}

// TestRegisterIfAvailable_BinaryMissing_GracefulDegrade asserts that when
// LookPath fails, no registration occurs and a structured warning is
// emitted in the WHAT/CHECK/FIX shape.
func TestRegisterIfAvailable_BinaryMissing_GracefulDegrade(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	reg := enrich.NewRegistry()

	fakeLook := func(name string) (string, error) {
		return "", errors.New("exec: \"pdftotext\": executable file not found in $PATH")
	}

	registered := registerIfAvailable(reg, fakeLook, logger)
	if registered {
		t.Fatalf("registerIfAvailable returned true with missing binary, want false")
	}

	// Registry should be empty.
	arts, errs := reg.ApplyAll(testCtx(t), "x.pdf", staging.ItemMetadata{ContentType: "application/pdf"}, t.TempDir())
	if len(arts) != 0 || len(errs) != 0 {
		t.Errorf("registry should be empty after missing-binary graceful degrade; arts=%d errs=%d", len(arts), len(errs))
	}

	// Warning text follows WHAT/CHECK/FIX shape per spec §5.1.
	logText := buf.String()
	wantSubstrs := []string{
		"enrich/pdf: pdftotext not found in PATH",
		"PDF enricher is disabled",
		"CHECK:",
		"docker inspect",
		"/usr/bin/pdftotext",
		"FIX:",
		"glovebox-enricher-runtime",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(logText, want) {
			t.Errorf("warning missing substring %q\nfull warning:\n%s", want, logText)
		}
	}
}
