// Package office implements the office-document enricher.
//
// It shells out to the `pandoc` CLI to render OOXML documents
// (.docx, .xlsx, .pptx) as markdown into a sidecar
// `content.extracted.md` (or `content.attachment-<n>.extracted.md`)
// next to the source file.
//
// Spec: docs/specs/14-content-enrichment-design.md §5 (table row "office")
// and §5.1 (init() LookPath check).
//
// Build tag: real-binary scenarios are exercised by tests gated behind
// the `enrichruntime` build tag. The implementation itself is always
// compiled so that the registry can include this enricher in any image
// that ships pandoc. Connectors that do not want to depend on pandoc
// simply do not blank-import this package from their main.go.
//
// Legacy formats (.doc, .xls, .ppt — application/msword,
// application/vnd.ms-excel, application/vnd.ms-powerpoint) are NOT
// supported by pandoc directly. Per the spec, this enricher's Applies()
// returns true for those content-types too, and Enrich() returns a
// specific error so that the shared error-marker machinery writes a
// `content.office.error.md` marker explaining the limitation and the
// FIX (convert to OOXML at the source or wait for a future
// libreoffice-headless enricher).
package office

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// Name is the producer name embedded in EnrichmentRecord.Producer and in
// the marker filename `content.<name>.error.md`. Matches spec §5.
const Name = "office"

// OOXML content-types. Spec §5.
const (
	ContentTypeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	ContentTypeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	ContentTypePptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// Legacy (non-OOXML) office content-types. Pandoc cannot read these
// directly; Enrich returns a specific error for them so that the
// pipeline writes a `content.office.error.md` marker explaining the
// limitation. Spec §5.
const (
	ContentTypeDoc = "application/msword"
	ContentTypeXls = "application/vnd.ms-excel"
	ContentTypePpt = "application/vnd.ms-powerpoint"
)

// pandocPath holds the resolved absolute path to the pandoc binary
// (populated by init via exec.LookPath). Empty when pandoc is not
// available; in that case init() also does NOT register this enricher
// with enrich.Default, so the path is never consulted from production.
// Kept as a package variable rather than const so tests can stub it.
var pandocPath string

// lookPath is indirected through a variable so tests can simulate the
// "binary not found" scenario without mutating $PATH. initLogger is the
// init-time warning sink.
var (
	lookPath   = exec.LookPath
	initLogger = log.New(os.Stderr, "", 0)
)

// officeDisabledMsg is the WHAT/CHECK/FIX warning logged when pandoc is
// absent (spec §5.1 template); enrich.RegisterIfAvailable appends the
// LookPath error.
const officeDisabledMsg = "enrich/office: pandoc not found in PATH; the Office enricher is disabled for this connector.\n" +
	"  CHECK: docker inspect <image> for /usr/bin/pandoc\n" +
	"  FIX:   rebase this connector on glovebox-enricher-runtime, or accept that office docs will not be rendered."

// registerIfAvailable is the shared binary-enricher gate (glovebox-afq4.14),
// identical in shape to the pdf/ocr siblings. The constructor captures the
// resolved pandoc path before registering.
func registerIfAvailable(reg *enrich.Registry, look func(string) (string, error), logger *log.Logger) bool {
	return enrich.RegisterIfAvailable(reg, "pandoc",
		func(p string) enrich.Enricher { pandocPath = p; return New() }, look, logger, officeDisabledMsg)
}

func init() {
	registerIfAvailable(enrich.Default, lookPath, initLogger)
}

// Enricher implements enrich.Enricher for OOXML office documents.
type Enricher struct{}

// New returns a ready-to-register *Enricher. Exposed for tests and for
// any caller that wants to instantiate the enricher into a custom
// (non-Default) Registry.
func New() *Enricher { return &Enricher{} }

// Name implements enrich.Enricher.
func (e *Enricher) Name() string { return Name }

// Applies reports whether the enricher should run for the given item.
//
// Returns true for the three OOXML content-types AND for the legacy
// formats (.doc/.xls/.ppt). For legacy formats, Enrich returns a
// specific error so the existing marker machinery writes
// `content.office.error.md` explaining the gap. This is the recommended
// "Approach 1" from the task description and matches the spec outcome.
func (e *Enricher) Applies(meta staging.ItemMetadata, sourcePath string) bool {
	switch meta.ContentType {
	case ContentTypeDocx, ContentTypeXlsx, ContentTypePptx,
		ContentTypeDoc, ContentTypeXls, ContentTypePpt:
		return true
	}
	return false
}

// ErrLegacyFormat is returned by Enrich for legacy (non-OOXML) office
// content-types so callers (and tests) can distinguish that case from
// genuine extraction failures via errors.Is.
var ErrLegacyFormat = errors.New("legacy office format not supported by pandoc")

// Enrich invokes pandoc against the source file and writes a markdown
// sidecar. For legacy formats it returns ErrLegacyFormat wrapped in a
// WHAT/CHECK/FIX message; the shared marker machinery turns that into a
// `content.office.error.md`.
func (e *Enricher) Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	// Legacy formats: short-circuit with a specific error before we
	// even open the file. The marker writer in connector/enrich uses
	// the Producer name ("office") to derive the marker filename, so
	// this naturally produces `content.office.error.md`.
	if isLegacyContentType(meta.ContentType) {
		return nil, fmt.Errorf(
			"enrich/office: %w (content-type=%s).\n"+
				"  CHECK: file <source> | head -c 8  (a legacy .doc/.xls/.ppt is a CFB/OLE compound document, not a zip).\n"+
				"  FIX:   convert the document to OOXML (.docx/.xlsx/.pptx) at the source connector, or wait for a future libreoffice-headless enricher.",
			ErrLegacyFormat, meta.ContentType,
		)
	}

	format, ok := pandocFormatFor(meta.ContentType)
	if !ok {
		// Should be unreachable because Applies has already filtered;
		// guard anyway so a future change to Applies cannot silently
		// regress into an empty/garbage pandoc invocation.
		return nil, fmt.Errorf("enrich/office: unsupported content-type %q (Applies regression)", meta.ContentType)
	}

	if pandocPath == "" {
		// Defensive: init() should have prevented registration. Tests
		// that bypass Default can still hit this if they construct an
		// Enricher manually without a binary. Keep the message
		// CHECK/FIX shaped for symmetry with the init warning.
		return nil, fmt.Errorf(
			"enrich/office: pandoc binary not located.\n" +
				"  CHECK: docker inspect <image> for /usr/bin/pandoc\n" +
				"  FIX:   rebase this connector on glovebox-enricher-runtime.",
		)
	}

	src, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("enrich/office: open source: %w", err)
	}
	defer src.Close()

	// Bounded: pandoc runs on a document an attacker chose, and an
	// unbounded bytes.Buffer means a crafted file can expand until memory
	// runs out.
	stdout := enrich.NewLimitedWriter(0)
	stderr := enrich.NewLimitedWriter(enrich.MaxStderrBytes)
	cmd := exec.CommandContext(ctx, pandocPath, "-f", format, "-t", "markdown")
	cmd.Stdin = src
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// Surface pandoc's stderr in the error: this is the most
		// useful single piece of information for the CHECK step in a
		// downstream marker.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf(
			"enrich/office: pandoc -f %s -t markdown failed: %s.\n"+
				"  CHECK: re-run `pandoc -f %s -t markdown < %s` and inspect stderr.\n"+
				"  FIX:   if the file is corrupt or password-protected, fix at the source connector; otherwise file an issue.",
			format, msg, format, filepath.Base(sourcePath),
		)
	}

	if err := stdout.Err("office"); err != nil {
		return nil, err
	}

	outName := outputFilename(sourcePath)
	outPath := filepath.Join(outputDir, outName)
	if err := os.WriteFile(outPath, stdout.Bytes(), 0644); err != nil {
		return nil, fmt.Errorf("enrich/office: write %s: %w", outName, err)
	}

	return []enrich.Artifact{{
		Filename: outName,
		Kind:     "extracted-text",
		Producer: Name,
	}}, nil
}

func isLegacyContentType(ct string) bool {
	switch ct {
	case ContentTypeDoc, ContentTypeXls, ContentTypePpt:
		return true
	}
	return false
}

func pandocFormatFor(ct string) (string, bool) {
	switch ct {
	case ContentTypeDocx:
		return "docx", true
	case ContentTypeXlsx:
		return "xlsx", true
	case ContentTypePptx:
		return "pptx", true
	}
	return "", false
}

// attachmentRE matches the connector's attachment-N-... pattern (spec
// §5.2). Captures N for use in the per-attachment extracted-md filename
// (`content.attachment-<n>.extracted.md`).
var attachmentRE = regexp.MustCompile(`^attachment-(\d+)-`)

// outputFilename derives the extracted-md filename next to sourcePath.
// Primary content (`content.raw` or anything not matching the
// attachment pattern) maps to `content.extracted.md`; attachment-N
// sources map to `content.attachment-<N>.extracted.md` so they sit
// alongside the corresponding attachment per spec §4.3.
func outputFilename(sourcePath string) string {
	base := filepath.Base(sourcePath)
	if m := attachmentRE.FindStringSubmatch(base); m != nil {
		return fmt.Sprintf("content.attachment-%s.extracted.md", m[1])
	}
	return "content.extracted.md"
}
