// Package pdf implements the PDF content enricher.
//
// It shells out to the poppler-utils `pdftotext` binary to extract a
// text-form sidecar from any item whose ContentType is application/pdf.
// The enricher is "binary-dependent" in spec terms: if pdftotext is not
// in PATH at process start, the package logs a structured warning and
// does NOT register with enrich.Default. See
// docs/specs/14-content-enrichment-design.md §5 + §5.1.
package pdf

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// pdftotextBinary is the command name we resolve via LookPath. Constant
// for now; if a future deployment ever requires a non-default path we'd
// expose a config knob rather than mutate this string.
const pdftotextBinary = "pdftotext"

// Enricher implements enrich.Enricher for application/pdf sources by
// shelling out to pdftotext.
type Enricher struct{}

// Name returns the stable identifier "pdf".
func (e *Enricher) Name() string { return "pdf" }

// Applies reports whether the source should be PDF-extracted. Dispatch
// is on meta.ContentType only (case-insensitive, MIME parameters
// stripped). We deliberately do NOT sniff bytes here: connectors are
// expected to set ContentType correctly upstream, and sniffing every
// staging item would re-open every source for an O(n) op the framework
// is already meant to avoid.
func (e *Enricher) Applies(meta staging.ItemMetadata, sourcePath string) bool {
	mt, _, err := mime.ParseMediaType(meta.ContentType)
	if err != nil {
		// Some sources stash a raw type without parameters; fall back to
		// a case-insensitive compare so we still match well-formed
		// inputs that ParseMediaType happens to reject.
		mt = strings.ToLower(strings.TrimSpace(meta.ContentType))
	}
	return strings.EqualFold(mt, "application/pdf")
}

// Enrich invokes `pdftotext -layout -enc UTF-8 - -` with the source file
// piped on stdin and captures the extracted text on stdout. The output
// is written to outputDir under a filename derived from sourcePath:
//
//   - content.raw                       -> content.extracted.md
//   - attachment-<n>-<safe-name>        -> content.attachment-<n>.extracted.md
//   - anything else (defensive default) -> <basename>.extracted.md
//
// Errors on non-zero exit or I/O failures carry the WHAT/CHECK/FIX
// discipline. Encrypted PDFs are detected by stderr substring and
// produce a more specific FIX hint.
func (e *Enricher) Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	src, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf(
			"enrich/pdf: open %s failed: %w.\n"+
				"  CHECK: ls -la %s\n"+
				"  FIX:   verify the source file exists and is readable by the connector process.",
			filepath.Base(sourcePath), err, sourcePath,
		)
	}
	defer src.Close()

	cmd := exec.CommandContext(ctx, pdftotextBinary, "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = src
	// Bounded: the source is a file an attacker chose, and an unbounded
	// bytes.Buffer here means a crafted PDF can expand until memory runs
	// out. stderr is bounded too so a chatty failure cannot do the same.
	stdout := enrich.NewLimitedWriter(0)
	stderr := enrich.NewLimitedWriter(enrich.MaxStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	if runErr != nil {
		return nil, classifyRunError(sourcePath, runErr, string(stderr.Bytes()))
	}
	if err := stdout.Err("pdf"); err != nil {
		return nil, err
	}

	outName := outputFilenameFor(sourcePath)
	outPath := filepath.Join(outputDir, outName)
	if err := os.WriteFile(outPath, stdout.Bytes(), 0644); err != nil {
		return nil, fmt.Errorf(
			"enrich/pdf: writing %s failed: %w.\n"+
				"  CHECK: ls -la %s\n"+
				"  FIX:   verify the staging item directory is writable by the connector process.",
			outName, err, outputDir,
		)
	}

	return []enrich.Artifact{{
		Filename: outName,
		Kind:     "extracted-text",
		Producer: "pdf",
	}}, nil
}

// outputFilenameFor turns a source filename into the sidecar filename
// per spec §4.3. See the Enrich() doc-comment for the mapping.
func outputFilenameFor(sourcePath string) string {
	base := filepath.Base(sourcePath)
	if base == "content.raw" {
		return "content.extracted.md"
	}
	// attachment-<n>-<rest> → content.attachment-<n>.extracted.md
	if strings.HasPrefix(base, "attachment-") {
		// Split into "attachment", "<n>", "<rest>" on the first two dashes.
		// We deliberately do NOT validate <n> as an integer; the staging
		// convention guarantees the shape, and a defensive parser here
		// would silently mis-name on the unusual case rather than
		// produce a debuggable filename.
		rest := strings.TrimPrefix(base, "attachment-")
		if dash := strings.IndexByte(rest, '-'); dash > 0 {
			n := rest[:dash]
			return "content.attachment-" + n + ".extracted.md"
		}
	}
	// Defensive default: keep the original basename as a prefix so the
	// produced sidecar is still correlatable to its source. Strips any
	// extension so we don't end up with "foo.pdf.extracted.md" for a
	// real-world source — though this branch should not be reached in
	// the normal staging pipeline.
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return stem + ".extracted.md"
}

// classifyRunError turns a non-zero exec.Run result into a WHAT/CHECK/FIX
// error. Encrypted PDFs are the one case we recognize specially because
// the FIX (decrypt at source) is materially different from the generic
// "this file is broken" FIX.
func classifyRunError(sourcePath string, runErr error, stderrText string) error {
	base := filepath.Base(sourcePath)
	stderrTrim := strings.TrimSpace(stderrText)
	lowerErr := strings.ToLower(stderrTrim)

	// Encrypted-PDF signal from pdftotext (poppler). The wording has
	// shifted across poppler releases — current 22.x emits "Incorrect
	// password"; older builds said "encrypted"; future wording could
	// say "password-protected" or "password required". Matching the
	// bare word "password" catches all current and plausible future
	// variants without producing false positives on the corrupt path
	// (poppler's "Syntax Error: ..." messages never contain "password").
	// Defense-in-depth: keep the other historically-observed substrings
	// so a future code reader can see the lineage at a glance.
	if strings.Contains(lowerErr, "password") ||
		strings.Contains(lowerErr, "encrypted") ||
		strings.Contains(lowerErr, "permission denied by") {
		return fmt.Errorf(
			"enrich/pdf: pdftotext refused %s as encrypted: %s.\n"+
				"  CHECK: pdfinfo %s   # confirm the PDF is password-protected\n"+
				"  FIX:   decrypt the PDF at the source connector before staging; the enricher does not hold credentials.",
			base, oneLine(stderrTrim), sourcePath,
		)
	}

	// Exit-code surface. exec.ExitError gives us the code; if even that
	// isn't present (e.g., context cancellation), fall back to the
	// underlying error string so callers still see WHY it failed.
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return fmt.Errorf(
			"enrich/pdf: pdftotext exited %d on %s: %s.\n"+
				"  Likely cause: corrupt or unsupported PDF.\n"+
				"  CHECK: pdfinfo %s   # surface the structural problem pdftotext rejected\n"+
				"  FIX:   if the PDF is operator-meaningful, re-fetch from source or repair with qpdf; otherwise this item's PDF stays un-extracted.",
			ee.ExitCode(), base, oneLine(stderrTrim), sourcePath,
		)
	}

	return fmt.Errorf(
		"enrich/pdf: pdftotext invocation failed on %s: %v.\n"+
			"  stderr: %s\n"+
			"  CHECK: which pdftotext && pdftotext -v\n"+
			"  FIX:   verify poppler-utils is installed in the running image and is executable by the connector user.",
		base, runErr, oneLine(stderrTrim),
	)
}

// oneLine collapses whitespace runs in stderr to keep error messages
// readable when the underlying tool wrote a multi-line diagnostic. If
// stderr was empty we substitute "(no stderr)" so the formatted error
// is still parseable.
func oneLine(s string) string {
	if s == "" {
		return "(no stderr)"
	}
	return strings.Join(strings.Fields(s), " ")
}
