// Package ocr implements the binary-dependent OCR enricher.
//
// It shells out to the `tesseract` CLI (poppler/tesseract-ocr-eng,
// shipped by glovebox-enricher-runtime; see Dockerfile.enricher-runtime
// and docs/specs/14-content-enrichment-design.md §5 + §6.1) to extract
// English text from image content.
//
// The enricher Applies() to image/jpeg, image/png, image/heic, and
// image/tiff content (matched case-insensitively, parameters stripped).
//
// Registration is gated on `exec.LookPath("tesseract")` at init() time:
// when the binary is absent the package logs a WHAT/CHECK/FIX warning
// and does NOT register, so connector images that did not rebase on
// glovebox-enricher-runtime degrade gracefully rather than failing
// every image item at Commit() time. See §5.1 of the spec.
package ocr

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// Name returned by the OCR enricher. Package-aligned per the
// Enricher.Name() convention in connector/enrich/enrich.go.
const enricherName = "ocr"

// tesseractBin is the binary name resolved via exec.LookPath at init()
// time. It is also the argv[0] used by Enrich(); kept as a package var
// to keep the rest of the code DRY if we ever need to support a
// versioned binary name.
const tesseractBin = "tesseract"

// supportedTypes is the set of MIME types this enricher claims. Compared
// case-insensitively, with any parameter suffix (e.g. "; charset=...")
// stripped, per Applies().
//
// Keep this in sync with the §5 spec row for ocr.
var supportedTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/heic": {},
	"image/tiff": {},
}

// Enricher is the OCR enricher implementation. It is value-stateless;
// all per-call state lives on the stack. A single zero-value instance
// is registered with enrich.Default at init() time.
type Enricher struct{}

// Name implements enrich.Enricher.
func (e *Enricher) Name() string { return enricherName }

// Applies reports whether the source file is an image type tesseract
// can OCR. Dispatch is purely on meta.ContentType so the same predicate
// works for primary content.raw and per-attachment sources.
//
// The content_type is normalised:
//   - lower-cased
//   - parameter suffix stripped at the first ";"
//   - trimmed of surrounding whitespace
//
// This matches the §5 spec row and the case-insensitivity guidance in
// the task description.
func (e *Enricher) Applies(meta staging.ItemMetadata, _ string) bool {
	ct := normalizeContentType(meta.ContentType)
	_, ok := supportedTypes[ct]
	return ok
}

// Enrich runs `tesseract - - -l eng`, piping the source bytes on stdin
// and capturing OCR'd UTF-8 text from stdout. Per the §5 spec row the
// output filename is content.extracted.md (or its content.<attachment>
// variant for attachment sources).
//
// Empty output is NOT an error: per §5 "OCR'd text; may be empty if
// image has no text". A blank/decorative image produces an artifact
// with a zero-byte sidecar.
//
// On non-zero exit the enricher returns an error wrapping the stderr
// transcript in the WHAT/CHECK/FIX shape (see docs/specs/14 §8). The
// caller (the registry's ApplyAll) records this in
// meta.Enrichments[].Status="failed" and writes a marker file; it does
// NOT abort the item commit.
//
// The tesseract output is passed through verbatim; downstream
// consumers judge signal quality (see task description).
func (e *Enricher) Enrich(ctx context.Context, sourcePath string, _ staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	src, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("enrich/ocr: open %s: %w\n"+
			"  CHECK: ls -l %s\n"+
			"  FIX:   re-stage the item; if the file is unreadable, fix at the connector",
			filepath.Base(sourcePath), err, sourcePath)
	}
	defer src.Close()

	// "tesseract - - -l eng": stdin -> stdout, English language pack.
	// CommandContext so the parent ctx (item commit deadline) can cancel
	// a runaway tesseract invocation.
	cmd := exec.CommandContext(ctx, tesseractBin, "-", "-", "-l", "eng")
	cmd.Stdin = src

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Surface the stderr transcript in the error so the WHAT line
		// carries the specific tesseract complaint (image too small,
		// unsupported format, etc.) rather than just an exit code.
		stderrSnippet := strings.TrimSpace(stderr.String())
		if stderrSnippet == "" {
			stderrSnippet = "(no stderr output)"
		}
		return nil, fmt.Errorf("enrich/ocr: tesseract failed on %s: %v: %s\n"+
			"  CHECK: tesseract %s - -l eng < %s\n"+
			"  FIX:   if the image is too small (< ~70 px tall), pre-scale at the connector; "+
			"if corrupt, fix at the source",
			filepath.Base(sourcePath), err, stderrSnippet,
			tesseractBin, sourcePath)
	}

	outFilename := outputFilename(sourcePath)
	outPath := filepath.Join(outputDir, outFilename)
	// Pass through verbatim: no post-processing of tesseract output.
	// Even on noisy images tesseract may emit garbage; the downstream
	// triage step decides whether the signal is useful.
	if err := os.WriteFile(outPath, stdout.Bytes(), 0644); err != nil {
		return nil, fmt.Errorf("enrich/ocr: write %s: %w\n"+
			"  CHECK: ls -ld %s\n"+
			"  FIX:   confirm the staging tmp dir is writable by uid %d",
			outFilename, err, outputDir, os.Getuid())
	}

	return []enrich.Artifact{{
		Filename: outFilename,
		Kind:     "ocr-text",
		Producer: enricherName,
	}}, nil
}

// outputFilename maps a source path under the item dir to the sidecar
// filename per §4.3 of the spec.
//
//   - "content.raw"            -> "content.extracted.md"
//   - "attachment-<n>-<name>"  -> "content.attachment-<n>.extracted.md"
//   - any other basename       -> "<basename>.extracted.md" (defensive
//     fallback; the connector pipeline only feeds the two shapes above)
func outputFilename(sourcePath string) string {
	base := filepath.Base(sourcePath)
	switch {
	case base == "content.raw":
		return "content.extracted.md"
	case strings.HasPrefix(base, "attachment-"):
		// "attachment-2-photo.jpg" -> ["attachment", "2", "photo.jpg"]
		// We want "content.attachment-2.extracted.md", i.e. the
		// numbered prefix without the trailing original filename so
		// the sidecar name is stable regardless of attachment renames.
		parts := strings.SplitN(base, "-", 3)
		if len(parts) >= 2 {
			return fmt.Sprintf("content.attachment-%s.extracted.md", parts[1])
		}
		return base + ".extracted.md"
	default:
		return base + ".extracted.md"
	}
}

// normalizeContentType lower-cases, strips parameters at the first ";",
// and trims surrounding whitespace from a MIME content-type string.
// "Image/PNG; charset=binary" -> "image/png".
func normalizeContentType(ct string) string {
	ct = strings.ToLower(ct)
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}

// lookPath and registerWith are package-level hooks so init() can be
// exercised without depending on the real exec.LookPath or the
// process-global registry. Tests swap these, invoke registerEnricher(),
// and assert the resulting calls.
var (
	lookPath     = exec.LookPath
	registerWith = func(e enrich.Enricher) { enrich.Default.Register(e) }
	warnLogger   = func() *slog.Logger { return slog.Default() }
)

// registerEnricher performs the init()-time LookPath check and either
// registers the enricher or logs a WHAT/CHECK/FIX warning. Returns true
// when registration happened (used by tests; ignored by init()).
//
// The warning text follows the §5.1 template verbatim so operators can
// grep for it across connector start-up logs.
func registerEnricher() bool {
	if _, err := lookPath(tesseractBin); err != nil {
		warnLogger().Warn(
			"enrich/ocr: tesseract not found in PATH; the OCR enricher is disabled for this connector.\n"+
				"  CHECK: docker inspect <image> for /usr/bin/tesseract\n"+
				"  FIX:   rebase this connector on glovebox-enricher-runtime, or accept that images will not be OCR'd.",
			"binary", tesseractBin,
			"lookpath_error", err.Error(),
		)
		return false
	}
	registerWith(&Enricher{})
	return true
}

func init() {
	registerEnricher()
}
