// Package enrich implements the content enrichment pipeline.
//
// See docs/specs/14-content-enrichment-design.md for the design.
// This package defines the Enricher interface, the Artifact value type,
// the Registry that holds enrichers active in the process, and the
// process-global Default registry.
//
// Per the spec (§4.4), per-enricher errors do NOT abort iteration in
// ApplyAll. Each error is captured as an EnricherError and ApplyAll
// continues running the remaining enrichers. The caller (StagingItem
// commit pipeline) is responsible for writing per-failure marker files.
package enrich

import (
	"context"
	"sync"

	"github.com/leftathome/glovebox/internal/staging"
)

// Enricher produces sidecar artifacts from a source file + its metadata.
type Enricher interface {
	// Name is a stable identifier used in metadata and error markers.
	// Convention: lowercase, hyphen-separated, package-aligned
	// (e.g., "pdf", "html", "ocr").
	Name() string

	// Applies reports whether this enricher should run on a given source
	// file. Implementations dispatch primarily on meta.ContentType, with
	// optional file-sniffing fallback for ambiguous cases.
	Applies(meta staging.ItemMetadata, sourcePath string) bool

	// Enrich produces sidecar artifacts. Implementations write files into
	// outputDir (the item directory) and return the artifacts they
	// produced. Errors are non-fatal for the item commit (see §4.4).
	Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error)
}

// Artifact describes one sidecar file produced by an enricher.
type Artifact struct {
	// Filename is relative to outputDir, e.g. "content.extracted.md".
	Filename string
	// Kind is the artifact category, e.g.
	// "extracted-text" | "image-caption" | "ocr-text" | "transcript".
	Kind string
	// Producer is the Name() of the Enricher that produced this artifact.
	Producer string
}

// EnricherError pairs an Enricher.Name() with the error it returned.
// Returned in the second slice of Registry.ApplyAll so the caller can
// emit per-enricher failure markers without losing which enricher failed.
type EnricherError struct {
	Producer string
	Err      error
}

// Error implements the error interface so EnricherError values can be
// passed through error-handling helpers when needed.
func (e EnricherError) Error() string {
	if e.Err == nil {
		return e.Producer + ": <nil>"
	}
	return e.Producer + ": " + e.Err.Error()
}

// Unwrap exposes the wrapped error for errors.Is / errors.As.
func (e EnricherError) Unwrap() error { return e.Err }

// Registry holds the set of enrichers active in the process.
//
// Registry is safe for concurrent use: Register and ApplyAll may be
// called from multiple goroutines, although in practice Register is
// only called from package init() functions during process startup.
type Registry struct {
	mu        sync.RWMutex
	enrichers []Enricher
}

// NewRegistry returns a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an Enricher to the registry. Subsequent ApplyAll calls
// will consult e for every source file.
//
// Register does not deduplicate by Name(); a caller registering two
// enrichers with the same Name() will see both consulted. This matches
// the spec, which treats Name() as an identifier for error reporting
// rather than as a uniqueness key.
func (r *Registry) Register(e Enricher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enrichers = append(r.enrichers, e)
}

// ApplyAll runs every Enricher whose Applies() returns true on the given
// source file. Returns the artifacts produced (across all enrichers, in
// registration order) and a per-enricher error slice. Errors do NOT
// abort iteration: a failing enricher's error is appended to errs and
// the loop continues with the next enricher.
//
// Concurrency: Register() calls made while an ApplyAll is in flight
// will NOT be observed by that in-flight call; the newly registered
// enricher takes effect on subsequent ApplyAll invocations. This
// follows from the snapshot-under-RLock pattern below.
//
// Both return slices are nil-safe to range over when no enrichers
// matched or no errors occurred.
func (r *Registry) ApplyAll(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) (artifacts []Artifact, errs []EnricherError) {
	// Snapshot the enrichers under the read lock so we don't hold it
	// during user-supplied Applies/Enrich calls (which may be slow).
	r.mu.RLock()
	snapshot := make([]Enricher, len(r.enrichers))
	copy(snapshot, r.enrichers)
	r.mu.RUnlock()

	for _, e := range snapshot {
		if !e.Applies(meta, sourcePath) {
			continue
		}
		arts, err := e.Enrich(ctx, sourcePath, meta, outputDir)
		if err != nil {
			errs = append(errs, EnricherError{Producer: e.Name(), Err: err})
			// Per §4.4: per-enricher errors do NOT abort iteration.
			continue
		}
		artifacts = append(artifacts, arts...)
	}
	return artifacts, errs
}

// Default is the process-global registry. Subpackages register their
// enrichers via init() into Default. The pipeline (StagingItem.Commit)
// calls Default.ApplyAll for each item being committed.
var Default = NewRegistry()
