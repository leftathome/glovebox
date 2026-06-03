// importer.go -- concrete walhelmImporter satisfying importer.Importer.
//
// This is a scaffold for the walhelm archive importer (spec 15 sec 6).
// Survey, LoadSurvey, LoadManifest, LoadFilter, and ClearState are
// stubs returning zero values or nil. Import is also a stub returning
// nil; real logic for tree-walking and item construction arrives in T8/T9.
package main

import (
	"context"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/importer"
)

// walhelmImporter implements importer.Importer for walhelm staged
// archive directories. A staged archive is a directory produced by the
// ingest pipeline (e.g. archives/<id>/raw/ + archives/<id>/tree/).
type walhelmImporter struct {
	fw          *connector.Framework
	sourceName  string
	concurrency int
	// fixedTags are appended to every item's tag map, operator-set
	// via --fixed-tags. An empty slice means no additional tags.
	fixedTags []string
}

// Compile-time check: *walhelmImporter satisfies importer.Importer.
var _ importer.Importer = (*walhelmImporter)(nil)

// Survey performs a streaming pass over the archive directory at path
// and writes a sidecar survey file.
//
// TODO(T8): implement -- scan tree entries, aggregate counts, write sidecar.
func (w *walhelmImporter) Survey(_ context.Context, _ string) (importer.SurveyFile, error) {
	// not implemented for v1; returns a no-op survey so RunOneShot can proceed.
	return &stubSurvey{}, nil
}

// LoadSurvey returns the existing sidecar survey for the archive, or
// (nil, nil) if none exists.
//
// TODO(T8): implement -- read sidecar from disk.
func (w *walhelmImporter) LoadSurvey(_ string) (importer.SurveyFile, error) {
	return nil, nil
}

// LoadManifest returns the current import manifest for the archive, or
// (nil, nil) if none exists.
//
// TODO(T8): implement -- read manifest JSON from disk.
func (w *walhelmImporter) LoadManifest(_ string) (importer.Manifest, error) {
	return nil, nil
}

// LoadFilter parses the filter config at filterPath. An empty path
// means no filter; all items pass.
//
// TODO(T8): implement -- parse filter JSON.
func (w *walhelmImporter) LoadFilter(_ string) (importer.FilterConfig, error) {
	return nil, nil
}

// ClearState removes any pre-existing manifest and checkpoint files
// next to the archive so that a fresh import starts from zero.
//
// TODO(T8): implement -- remove sidecar files.
func (w *walhelmImporter) ClearState(_ string) error {
	return nil
}

// Import streams the archive directory, applies the filter, and pushes
// items to glovebox ingest.
//
// TODO(T9): implement -- tree-walk raw/, apply filter, build ItemOptions,
// call fw.Backend.NewItem / WriteContent / Commit per item.
func (w *walhelmImporter) Import(
	_ context.Context,
	_ string,
	_ importer.SurveyFile,
	_ importer.FilterConfig,
	_ importer.ResumeDecision,
) error {
	return nil
}

// stubSurvey is a no-op SurveyFile returned by Survey until T8
// implements the real survey type. IsStale always returns false so
// that RunOneShot does not re-run Survey on every invocation.
type stubSurvey struct{}

func (s *stubSurvey) IsStale(_ string) (bool, error) {
	return false, nil
}
