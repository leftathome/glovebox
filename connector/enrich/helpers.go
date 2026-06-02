package enrich

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/leftathome/glovebox/internal/staging"
)

// EnrichmentRecord is re-exported from internal/staging so callers that
// only import this package can name the type. It is the same canonical
// type defined in internal/staging.
type EnrichmentRecord = staging.EnrichmentRecord

// AppendRecords appends EnrichmentRecord entries for a single source
// file to the provided slice: ok records first (in artifact order),
// then failed records (in error order). This is the per-source
// building block used by the commit pipeline; the connector calls it
// once for the primary content file and once per attachment so each
// record carries the correct Source filename.
//
// See docs/specs/14-content-enrichment-design.md §4.6.
func AppendRecords(dst []EnrichmentRecord, source string, arts []Artifact, errs []EnricherError) []EnrichmentRecord {
	for _, a := range arts {
		dst = append(dst, EnrichmentRecord{
			Producer: a.Producer,
			Kind:     a.Kind,
			Source:   source,
			Filename: a.Filename,
			Status:   "ok",
		})
	}
	for _, e := range errs {
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		dst = append(dst, EnrichmentRecord{
			Producer: e.Producer,
			Source:   source,
			Status:   "failed",
			Error:    msg,
		})
	}
	return dst
}

// Summarize converts the artifacts and per-enricher errors returned by
// ApplyAll into the flat []EnrichmentRecord stored on ItemMetadata.
// Ordering is:
//  1. primary ok records (in artifact order)
//  2. primary failed records (in error order)
//  3. attachment records, grouped by attachment source in
//     attachmentSources order; within each group, ok records first, then
//     failed records
//
// This convenience helper is for callers that have already grouped
// attachment artifacts/errors by attachment in matching-length slices.
// The connector commit pipeline avoids that bookkeeping by calling
// AppendRecords directly per attachment; this entry point is exposed
// for tests and for any future caller that wants the one-shot shape.
//
// attachmentArtsByIdx and attachmentErrsByIdx must each be parallel to
// attachmentSources (same length, same order). Pass nil for either when
// there are no records of that kind.
func Summarize(
	primarySource string,
	primaryArts []Artifact,
	primaryErrs []EnricherError,
	attachmentSources []string,
	attachmentArts []Artifact,
	attachmentErrs []EnricherError,
) []EnrichmentRecord {
	var out []EnrichmentRecord
	out = AppendRecords(out, primarySource, primaryArts, primaryErrs)

	switch len(attachmentSources) {
	case 0:
		// No sources named. If the caller passed attachment records
		// anyway, emit them with empty Source rather than silently
		// dropping (the asymmetric "0 sources but N arts" shape is a
		// caller mistake; preserving the records means it surfaces in
		// metadata.json instead of vanishing).
		if len(attachmentArts) > 0 || len(attachmentErrs) > 0 {
			out = AppendRecords(out, "", attachmentArts, attachmentErrs)
		}
	case 1:
		out = AppendRecords(out, attachmentSources[0], attachmentArts, attachmentErrs)
	default:
		// Flat-slice shape across multiple attachments cannot be
		// reconstructed. Production callers (see
		// connector/staging.go) avoid this by calling AppendRecords
		// per attachment. We emit the records with an empty Source
		// to preserve shape rather than silently dropping them.
		out = AppendRecords(out, "", attachmentArts, attachmentErrs)
	}
	return out
}

// WriteErrorMarkers writes one content.<producer>.error.md marker file
// in outputDir for each EnricherError. The marker contents follow the
// WHAT/CHECK/FIX shape per docs/specs/14-content-enrichment-design.md
// §4.4 and §8. Both the primary and attachment error slices are
// processed; passing nil/empty slices is a no-op.
//
// Returns the first I/O error encountered. Callers in the commit
// pipeline log such errors but do not abort the commit (the
// in-metadata EnrichmentRecord already carries the failure detail).
func WriteErrorMarkers(outputDir string, primaryErrs, attachmentErrs []EnricherError) error {
	for _, e := range primaryErrs {
		if err := writeOneMarker(outputDir, e); err != nil {
			return err
		}
	}
	for _, e := range attachmentErrs {
		if err := writeOneMarker(outputDir, e); err != nil {
			return err
		}
	}
	return nil
}

func writeOneMarker(outputDir string, e EnricherError) error {
	filename := fmt.Sprintf("content.%s.error.md", e.Producer)
	msg := ""
	if e.Err != nil {
		msg = e.Err.Error()
	}
	body := fmt.Sprintf(
		"enrich/%s: extraction-failed: %s\n"+
			"  CHECK: re-run the %s enricher manually against this item's source file to reproduce.\n"+
			"  FIX:   inspect the source for corruption or unsupported format; if recoverable, fix at the connector and re-stage.\n",
		e.Producer, msg, e.Producer,
	)
	return os.WriteFile(filepath.Join(outputDir, filename), []byte(body), 0644)
}
