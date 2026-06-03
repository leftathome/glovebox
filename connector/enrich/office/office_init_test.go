// Tests in this file are intentionally NOT gated behind the
// `enrichruntime` build tag. They cover behaviour that does not depend
// on a real pandoc binary: the init-time LookPath check, Applies()
// dispatch, and the legacy-format Enrich error path (which returns
// before pandoc is invoked).
package office

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

func TestApplies_OOXML_True(t *testing.T) {
	e := New()
	cases := []struct {
		name        string
		contentType string
	}{
		{"docx", ContentTypeDocx},
		{"xlsx", ContentTypeXlsx},
		{"pptx", ContentTypePptx},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := staging.ItemMetadata{ContentType: tc.contentType}
			if !e.Applies(meta, "/tmp/whatever") {
				t.Fatalf("Applies(%s) = false, want true", tc.contentType)
			}
		})
	}
}

func TestApplies_Legacy_True(t *testing.T) {
	// Approach 1 in the task description: Applies returns true for the
	// legacy formats so the pipeline calls Enrich(), which then returns
	// a specific error and the marker writer emits
	// content.office.error.md. This test pins that contract.
	e := New()
	cases := []struct {
		name        string
		contentType string
	}{
		{"doc", ContentTypeDoc},
		{"xls", ContentTypeXls},
		{"ppt", ContentTypePpt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := staging.ItemMetadata{ContentType: tc.contentType}
			if !e.Applies(meta, "/tmp/whatever") {
				t.Fatalf("Applies(%s) = false, want true (legacy formats route to Enrich for the error marker)", tc.contentType)
			}
		})
	}
}

func TestApplies_NonOffice_False(t *testing.T) {
	e := New()
	cases := []struct {
		name        string
		contentType string
	}{
		{"pdf", "application/pdf"},
		{"text", "text/plain"},
		{"html", "text/html"},
		{"jpeg", "image/jpeg"},
		{"empty", ""},
		{"random", "application/x-thing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := staging.ItemMetadata{ContentType: tc.contentType}
			if e.Applies(meta, "/tmp/whatever") {
				t.Fatalf("Applies(%s) = true, want false", tc.contentType)
			}
		})
	}
}

func TestEnrich_Legacy_ReturnsErrLegacyFormat(t *testing.T) {
	// Even with no pandoc binary on the system, the legacy-format
	// branch must short-circuit cleanly with ErrLegacyFormat. The
	// shared marker machinery in connector/enrich will turn the error
	// into content.office.error.md.
	e := New()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "doc.doc")
	// A few bytes of a CFB/OLE header; Enrich shouldn't even read the
	// file for the legacy branch, but writing real-looking bytes makes
	// the test fixture self-documenting.
	if err := os.WriteFile(src, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta := staging.ItemMetadata{ContentType: ContentTypeDoc}
	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err == nil {
		t.Fatalf("Enrich(legacy) returned nil error; want ErrLegacyFormat")
	}
	if !errors.Is(err, ErrLegacyFormat) {
		t.Fatalf("Enrich(legacy) error = %v; want errors.Is ErrLegacyFormat", err)
	}
	if len(arts) != 0 {
		t.Errorf("Enrich(legacy) returned %d artifacts; want 0", len(arts))
	}

	// FIX hint must mention OOXML conversion so the marker is
	// actionable, per the spec §5 table.
	if !strings.Contains(err.Error(), "OOXML") {
		t.Errorf("legacy error missing OOXML hint: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "FIX") {
		t.Errorf("legacy error missing FIX: prefix: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("legacy error missing CHECK: prefix: %q", err.Error())
	}
}

func TestEnrich_MissingBinary_GracefulDegrade(t *testing.T) {
	// Simulate the "no pandoc in this image" scenario by clearing the
	// resolved binary path. Production avoids this case because init()
	// only registers the enricher when LookPath succeeds, but tests
	// that hand-construct the Enricher (or that ran on an image whose
	// pandoc disappeared mid-process) must not panic.
	saved := pandocPath
	t.Cleanup(func() { pandocPath = saved })
	pandocPath = ""

	e := New()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "doc.docx")
	if err := os.WriteFile(src, []byte("fake"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	meta := staging.ItemMetadata{ContentType: ContentTypeDocx}

	arts, err := e.Enrich(context.Background(), src, meta, tmp)
	if err == nil {
		t.Fatalf("Enrich without binary returned nil error; want non-nil")
	}
	if len(arts) != 0 {
		t.Errorf("Enrich without binary returned %d artifacts; want 0", len(arts))
	}
	if !strings.Contains(err.Error(), "pandoc") {
		t.Errorf("error must mention pandoc: %q", err.Error())
	}
}

func TestOutputFilename(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"/staging/abc/content.raw", "content.extracted.md"},
		{"/staging/abc/attachment-1-report.docx", "content.attachment-1.extracted.md"},
		{"/staging/abc/attachment-12-slides.pptx", "content.attachment-12.extracted.md"},
		{"/tmp/anything.docx", "content.extracted.md"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			got := outputFilename(tc.src)
			if got != tc.want {
				t.Errorf("outputFilename(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}
