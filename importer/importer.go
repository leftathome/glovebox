// Package importer defines the contract and orchestration runtime for
// one-shot archive importers (mbox, Google Chat JSON, WhatsApp export,
// Slack dump, and so on). It is the sibling of connector.RunPollLoop /
// connector.RunWatchLoop: where those drive long-lived pollers against
// live remote sources, importer.RunOneShot drives a single finite pass
// against a finished local archive.
//
// The package is deliberately thin. The Importer interface captures
// what every format handler must expose so RunOneShot can orchestrate
// survey-then-import, staleness detection, and resume-decision logic
// without knowing anything about the concrete file layout of mbox,
// Google Chat JSON, or any other format. Format-specific types --
// SurveyV1 schemas, filter config shapes, manifest fields -- live in
// the per-format packages under importers/<format>/.
//
// See docs/specs/09-mbox-importer-design.md for the runtime shape
// (§3.1) and the resume rule (§3.1.1) this package implements.
package importer

import "context"

// ManifestStatus is the life-cycle status field recorded in a format's
// import manifest. The four legal values (plus the empty zero value,
// which signals "no manifest present") drive the resume decision in
// Decide and appear verbatim in the on-disk manifest JSON. See spec
// §3.6.1 for the shared vocabulary.
type ManifestStatus string

const (
	// StatusInProgress: a run is mid-flight (or the process died before
	// writing a terminal status). Treated as "start fresh" by Decide
	// since an in_progress-at-load manifest means a prior crash.
	StatusInProgress ManifestStatus = "in_progress"

	// StatusComplete: the archive has been fully imported. Decide
	// short-circuits to ExitComplete.
	StatusComplete ManifestStatus = "complete"

	// StatusInterrupted: the run was cleanly interrupted (SIGTERM,
	// ctx cancel) and flushed its resume_state. Decide returns
	// Resume if the manifest records a non-zero byte offset.
	StatusInterrupted ManifestStatus = "interrupted"

	// StatusFailed: the run hit a terminal error worth investigating.
	// Decide returns RequireExplicitResume unless the operator
	// passes --resume=true.
	StatusFailed ManifestStatus = "failed"
)

// Importer is the contract a format-specific handler implements to
// plug into RunOneShot. The orchestration runtime calls these methods
// in a fixed order; implementations own all format-specific I/O
// (survey JSON layout, manifest JSON layout, checkpoint file format,
// filter config parsing).
//
// Implementations must be safe for a single-goroutine caller;
// RunOneShot never invokes two methods in parallel on the same
// Importer.
type Importer interface {
	// Survey performs a streaming pass over the archive at the given
	// path, aggregating whatever per-format report is useful for
	// filter authoring, and writes the sidecar survey file. See
	// spec §3.3 for the mbox example. The returned SurveyFile is
	// what a subsequent Import call receives; callers may safely
	// pass it to IsStale later to detect source changes.
	Survey(ctx context.Context, path string) (SurveyFile, error)

	// LoadSurvey returns the existing sidecar survey for the source,
	// or (nil, nil) if none exists. A non-nil error is reserved for
	// I/O or parse failures that should abort the run.
	LoadSurvey(path string) (SurveyFile, error)

	// LoadManifest returns the current import manifest for the
	// source, or (nil, nil) if none exists. A non-nil error is
	// reserved for I/O or parse failures.
	LoadManifest(path string) (Manifest, error)

	// LoadFilter parses the user-authored filter config at filterPath
	// and returns an opaque FilterConfig handed back into Import.
	// If filterPath is empty, implementations should return a
	// default-"include everything" filter (or nil, at the
	// implementation's discretion -- Import must accept whatever
	// LoadFilter returns).
	LoadFilter(filterPath string) (FilterConfig, error)

	// ClearState removes any pre-existing manifest and checkpoint
	// next to the archive so that a fresh import starts from zero.
	// Called when Decide returns StartFresh and prior state exists.
	ClearState(path string) error

	// Import streams the archive, applies the filter, and pushes
	// items to glovebox ingest. The decision argument tells the
	// implementation whether to start from offset 0 or resume from
	// a prior checkpoint; the implementation is free to re-read
	// its own manifest/checkpoint files for the full state.
	Import(ctx context.Context, path string, survey SurveyFile, filter FilterConfig, decision ResumeDecision) error
}

// SurveyFile is the minimal, format-agnostic view RunOneShot needs of
// a survey sidecar. Concrete survey schemas (SurveyV1 for mbox, etc.)
// live in format-specific packages and may be obtained by type-asserting
// this interface to the concrete type when a caller needs richer access.
type SurveyFile interface {
	// IsStale reports whether the source file at sourcePath differs
	// from what the survey was generated against (size / mtime
	// mismatch per spec §3.3.2). A stale survey must be regenerated
	// before import.
	IsStale(sourcePath string) (bool, error)
}

// FilterConfig is the opaque per-format pre-filter configuration.
// RunOneShot treats it as a black box and hands it straight to Import.
// The concrete type for mbox lives in importers/mbox/filter.go (a
// later bead).
type FilterConfig interface{}

// Manifest is the minimal contract RunOneShot needs to reason about
// a previous import run. Concrete per-format manifests (e.g. the
// mbox ImportManifestV1 in spec §3.6) implement this interface so
// RunOneShot can call Decide without knowing the format-specific
// schema.
type Manifest interface {
	// Status returns the manifest status field. One of StatusInProgress,
	// StatusComplete, StatusInterrupted, or StatusFailed per spec
	// §3.6.1. The empty ManifestStatus is reserved for "no manifest"
	// and implementations should never return it.
	Status() ManifestStatus

	// ByteOffset returns the resume offset recorded in the manifest's
	// resume_state. Zero if no resume state is present; a non-zero
	// value is also the canonical "checkpoint exists" signal that
	// Decide consults.
	ByteOffset() int64

	// MessageIDs returns the manifest's message_ids_ingested set
	// (or format equivalent) for dedup-preserving resume. Returns
	// nil if the manifest has no such set.
	MessageIDs() []string
}
