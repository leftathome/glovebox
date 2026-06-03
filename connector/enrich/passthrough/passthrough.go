// Package passthrough implements the identity-copy enricher: it reads a
// text source file and writes it byte-for-byte to a `.extracted.md`
// sidecar so downstream consumers can index a uniform `**/*.md` glob
// across all enriched content (HTML→text, PDF→text, OCR, office, and
// already-text).
//
// See docs/specs/14-content-enrichment-design.md §5 (passthrough row).
//
// # Applicability
//
// Applies() returns true when meta.ContentType matches any of:
//
//   - text/plain*   (with or without charset/params)
//   - text/markdown (with or without charset/params)
//
// or when meta.ContentType is empty AND the source file sniffs as text
// (printable ASCII + whitespace, examining the first sniffPrefix bytes).
// The empty-ContentType fallback is intentionally conservative — when
// upstream connectors lack a declared content-type, we'd rather not run
// passthrough on a binary blob than risk corrupting downstream indexes
// with raw bytes mislabeled as `.md`.
//
// # Output filename convention
//
// The output is the identity of the input, so naming is the only thing
// that varies. The convention chosen here (and pinned by the tests in
// this package):
//
//   - source basename `content.raw`   → `content.extracted.md`
//   - any other source basename `X.Y` → `content.X.extracted.md`
//     (the trailing extension is stripped from X.Y if present)
//
// Examples:
//
//   - content.raw                 → content.extracted.md
//   - attachment-2-report.txt     → content.attachment-2-report.extracted.md
//   - attachment-1-LICENSE        → content.attachment-1-LICENSE.extracted.md
//
// Rationale: a uniform `content.<scope>.extracted.md` shape lets
// downstream consumers glob with a single `content.*.extracted.md`
// pattern; stripping the source extension avoids the awkward
// `content.report.txt.extracted.md` form. The `content.raw → content.extracted.md`
// shortcut matches the spec's primary example in §4.3.
package passthrough

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

// Enricher is the pure-Go passthrough enricher. The zero value is ready
// to use; there is no configurable state.
type Enricher struct{}

// Name returns the enricher's stable identifier ("passthrough").
func (e *Enricher) Name() string { return "passthrough" }

// sniffPrefix is the number of leading bytes inspected when sniffing an
// untyped source. 512 matches Go's net/http.DetectContentType convention
// — large enough to clear most file headers, small enough to keep the
// fallback path cheap.
const sniffPrefix = 512

// Applies reports whether passthrough should run on the given source.
//
// Dispatch order:
//  1. Parse meta.ContentType's media type (strip params); match
//     text/plain*, text/markdown, or markdown's common variants.
//  2. If ContentType is empty, sniff up to sniffPrefix bytes of the
//     source; treat the file as text if every inspected byte is
//     printable ASCII or whitespace.
//  3. Otherwise, return false (let a more specific enricher handle it).
func (e *Enricher) Applies(meta staging.ItemMetadata, sourcePath string) bool {
	if mediaType := mediaTypeOnly(meta.ContentType); mediaType != "" {
		return isTextMediaType(mediaType)
	}
	return sniffsAsText(sourcePath)
}

// Enrich reads sourcePath in full and writes it byte-identical to the
// computed output filename in outputDir. Returns exactly one Artifact
// on success. Any I/O error is returned as-is (non-fatal at the
// pipeline level; the pipeline writes a marker file per §4.4).
func (e *Enricher) Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]enrich.Artifact, error) {
	// Honour context cancellation as a courtesy: passthrough's I/O is
	// small and synchronous, so this only matters if the pipeline
	// already cancelled before invoking us.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	srcFile, err := os.Open(sourcePath)
	if err != nil {
		// WHAT: open source. CHECK: file presence / permissions.
		// FIX: re-stage from connector if file is missing.
		return nil, fmt.Errorf("passthrough: open source %s: %w", sourcePath, err)
	}
	defer srcFile.Close()

	outName := outputFilename(filepath.Base(sourcePath))
	outPath := filepath.Join(outputDir, outName)

	dstFile, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		// WHAT: open destination. CHECK: outputDir exists and is writable.
		// FIX: ensure pipeline created the item dir before enrichment.
		return nil, fmt.Errorf("passthrough: open destination %s: %w", outPath, err)
	}
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		// WHAT: copy bytes. CHECK: disk space / I/O errors on outputDir.
		// FIX: free space on staging volume; re-run the commit.
		return nil, fmt.Errorf("passthrough: copy %s → %s: %w", sourcePath, outPath, err)
	}
	if err := dstFile.Close(); err != nil {
		// WHAT: close destination (flush). CHECK: disk space / I/O.
		// FIX: free space on staging volume; re-run the commit.
		return nil, fmt.Errorf("passthrough: close destination %s: %w", outPath, err)
	}

	return []enrich.Artifact{{
		Filename: outName,
		Kind:     "extracted-text",
		Producer: e.Name(),
	}}, nil
}

// outputFilename maps a source basename to the corresponding passthrough
// sidecar name per the convention documented at package scope.
func outputFilename(srcBase string) string {
	if srcBase == "content.raw" {
		return "content.extracted.md"
	}
	stem := stripExt(srcBase)
	return "content." + stem + ".extracted.md"
}

// stripExt removes the final extension (if any) from name. Unlike
// strings.TrimSuffix(name, filepath.Ext(name)), this is a no-op on
// names without an extension (filepath.Ext already returns "" in that
// case, so it's equivalent; we keep this thin wrapper for readability
// at the call site).
func stripExt(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return name
	}
	return strings.TrimSuffix(name, ext)
}

// mediaTypeOnly returns the lower-cased media type portion of a
// Content-Type header value, stripping any parameters (e.g.,
// "text/plain; charset=utf-8" → "text/plain"). Returns "" when the
// input is empty or malformed (no slash); this collapses the
// empty/malformed cases together for the sniff fallback.
func mediaTypeOnly(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return ""
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	if !strings.Contains(ct, "/") {
		return ""
	}
	return ct
}

// isTextMediaType reports whether mt is a media type the passthrough
// enricher recognises. mt must already be normalised (lowercase, no
// params) — see mediaTypeOnly.
func isTextMediaType(mt string) bool {
	switch mt {
	case "text/plain", "text/markdown", "text/x-markdown":
		return true
	}
	// Spec calls out `text/plain*` (the trailing wildcard covers any
	// future text/plain.* variant). Keep this as a narrow forward
	// match rather than a generic `text/*` allowlist to avoid grabbing
	// text/html (handled by the html enricher) or text/csv (no
	// downstream story yet).
	return strings.HasPrefix(mt, "text/plain")
}

// sniffsAsText reads up to sniffPrefix bytes of sourcePath and reports
// whether every inspected byte is a printable ASCII character or
// common whitespace (\t, \n, \r, \v, \f). Returns false on any I/O
// error, including ENOENT — callers treat an unreadable source as
// "not applicable" rather than escalating.
func sniffsAsText(sourcePath string) bool {
	f, err := os.Open(sourcePath)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, sniffPrefix)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false
	}
	for _, b := range buf[:n] {
		if !isTextByte(b) {
			return false
		}
	}
	return true
}

// isTextByte reports whether b is plausibly part of an ASCII text
// stream: printable ASCII (0x20-0x7E) or a common whitespace control.
// Bytes >= 0x80 are rejected to keep the sniffer conservative — UTF-8
// non-ASCII bodies whose ContentType is empty stay opaque to
// passthrough; the connector should label them.
func isTextByte(b byte) bool {
	switch b {
	case '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return b >= 0x20 && b < 0x7F
}

// init registers Enricher with the process-global Default registry on
// package import. Connectors opt in via blank import:
//
//	import _ "github.com/leftathome/glovebox/connector/enrich/passthrough"
func init() {
	enrich.Default.Register(&Enricher{})
}
