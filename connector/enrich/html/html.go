// Package html provides a pure-Go enricher that extracts the visible
// text of an HTML document into a content.extracted.md sidecar.
//
// It is a thin wrapper over the existing connector/content.HTMLToText
// helper: the helper owns HTML parsing (including entity decoding and
// dropping <script>/<style> raw text), and this package owns only the
// Enricher contract glue — dispatch on ContentType, file I/O, and the
// staging-directory output-naming convention from
// docs/specs/14-content-enrichment-design.md §5.
//
// Distroless-compatible: no exec, no cgo, no temp shells.
package html

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/leftathome/glovebox/connector/content"
	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// Enricher implements enrich.Enricher for HTML content.
type Enricher struct{}

// Name returns the stable identifier "html".
func (e *Enricher) Name() string { return "html" }

// Applies reports whether meta.ContentType matches text/html* (case-
// insensitive), tolerating trailing parameters like "; charset=utf-8".
func (e *Enricher) Applies(meta staging.ItemMetadata, _ string) bool {
	ct := strings.TrimSpace(strings.ToLower(meta.ContentType))
	if ct == "" {
		return false
	}
	// Drop any "; charset=..." parameters before comparing.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "text/html" || strings.HasPrefix(ct, "text/html")
}

// attachmentStem matches "attachment-<digits>" at the start of a
// basename. Per spec §4.3 / §5, attachment files land as
// attachment-<n>-<safe-filename>; the extracted-text sidecar is named
// content.attachment-<n>.extracted.md so all per-attachment artifacts
// align under a stable index.
var attachmentStem = regexp.MustCompile(`^(attachment-\d+)`)

// outputFilename returns the sidecar filename for a given source
// basename, per the spec's per-attachment naming rule.
func outputFilename(sourcePath string) string {
	base := filepath.Base(sourcePath)
	if base == "content.raw" {
		return "content.extracted.md"
	}
	if m := attachmentStem.FindStringSubmatch(base); m != nil {
		return "content." + m[1] + ".extracted.md"
	}
	// Fallback for unconventional source names (importers that don't
	// follow the content.raw / attachment-<n>-* pattern). Use the
	// source basename as the discriminator so we never clobber the
	// primary content.extracted.md.
	return "content." + base + ".extracted.md"
}

// Enrich reads sourcePath, runs the content.HTMLToText helper over it,
// and writes the resulting text to a sidecar in outputDir.
func (e *Enricher) Enrich(_ context.Context, sourcePath string, _ staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		// WHAT/CHECK/FIX shape per spec §8 error discipline.
		return nil, fmt.Errorf(
			"enrich/html: read-source-failed: %w. "+
				"CHECK: verify %s exists and is readable by the connector process. "+
				"FIX: re-run the connector to re-stage the item, or restore the file from backup",
			err, sourcePath,
		)
	}

	text := content.HTMLToText(data)

	outName := outputFilename(sourcePath)
	outPath := filepath.Join(outputDir, outName)
	if err := os.WriteFile(outPath, text, 0644); err != nil {
		return nil, fmt.Errorf(
			"enrich/html: write-sidecar-failed: %w. "+
				"CHECK: verify %s is writable and has space available. "+
				"FIX: ensure the staging directory permissions and disk space allow sidecar writes, then re-run the connector",
			err, outputDir,
		)
	}

	return []enrich.Artifact{{
		Filename: outName,
		Kind:     "extracted-text",
		Producer: e.Name(),
	}}, nil
}

func init() {
	enrich.Default.Register(&Enricher{})
}
