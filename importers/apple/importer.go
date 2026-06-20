// importer.go -- concrete appleImporter satisfying importer.Importer.
//
// One-shot importer for finalized Apple-export staged archives. Import reads
// the finalize receipt (metadata.json), validates that it is an Apple delivery
// (media_type=archive/generic-tarball AND matcher_id prefixed "apple/"), walks
// tree/, and stages one item per regular file via a bounded goroutine pool
// (stageAll).
//
// This is the skeleton importer: every tree/ file is passthrough-staged as-is.
// CSV parsing, nested-zip expansion, and media-services normalization are
// deliberately out of scope here (later tasks glovebox-x2xf / glovebox-yea7 /
// glovebox-ot7v).
//
// Unlike walhelm, Apple deliveries carry no producer-asserted data_subject:
// the generic tarball has no subject principal. The owner's subject is
// supplied by config (data_subject_default, wired by connector.NewFramework
// via SetConfigDataSubject) and resolved in the staging writer's merge chain.
//
// Survey, LoadSurvey, LoadManifest, LoadFilter, and ClearState are intentional
// no-ops for v1: Apple archives are one-shot imports and do not require resume,
// survey sidecars, or filter configs in the initial release.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/importer"
	"github.com/leftathome/glovebox/internal/ingest/archives"
)

// appleMediaType is the only media_type a finalized Apple-export archive may
// carry. Apple deliveries arrive as generic tarballs; the importer refuses
// anything else so a misrouted archive fails loudly instead of being staged
// with the wrong provenance semantics.
const appleMediaType = "archive/generic-tarball"

// appleMatcherPrefix is the matcher_id namespace that identifies an Apple
// delivery. Requiring this prefix is how the apple importer claims only Apple
// deliveries among the generic-tarball population (other producers also emit
// generic tarballs but under different matcher_id namespaces).
const appleMatcherPrefix = "apple/"

// appleImporter implements importer.Importer for finalized Apple-export staged
// archive directories. A staged archive is a directory produced by the ingest
// pipeline (e.g. archives/<id>/metadata.json + archives/<id>/tree/).
type appleImporter struct {
	fw          *connector.Framework
	sourceName  string
	concurrency int
	// fixedTags are appended to every item's tag map, operator-set via
	// --fixed-tags. An empty slice means no additional tags.
	fixedTags []string
}

// Compile-time check: *appleImporter satisfies importer.Importer.
var _ importer.Importer = (*appleImporter)(nil)

// Survey performs a streaming pass over the archive directory at path and
// writes a sidecar survey file.
//
// TODO: implement -- scan tree entries, aggregate counts, write sidecar.
func (a *appleImporter) Survey(_ context.Context, _ string) (importer.SurveyFile, error) {
	// not implemented for v1; returns a no-op survey so RunOneShot can proceed.
	return &stubSurvey{}, nil
}

// LoadSurvey returns the existing sidecar survey for the archive, or
// (nil, nil) if none exists.
//
// TODO: implement -- read sidecar from disk.
func (a *appleImporter) LoadSurvey(_ string) (importer.SurveyFile, error) {
	return nil, nil
}

// LoadManifest returns the current import manifest for the archive, or
// (nil, nil) if none exists.
//
// TODO: implement -- read manifest JSON from disk.
func (a *appleImporter) LoadManifest(_ string) (importer.Manifest, error) {
	return nil, nil
}

// LoadFilter parses the filter config at filterPath. An empty path means no
// filter; all items pass.
//
// TODO: implement -- parse filter JSON.
func (a *appleImporter) LoadFilter(_ string) (importer.FilterConfig, error) {
	return nil, nil
}

// ClearState removes any pre-existing manifest and checkpoint files next to
// the archive so that a fresh import starts from zero.
//
// TODO: implement -- remove sidecar files.
func (a *appleImporter) ClearState(_ string) error {
	return nil
}

// Import reads the finalized archive's metadata.json (a FinalizeReceipt),
// validates that it is an Apple delivery (generic tarball under the apple/
// matcher namespace), then walks <path>/tree/ and passthrough-stages one item
// per regular file. The subject is supplied by config (not the receipt); the
// matcher selects the destination agent (see BuildItemOptions).
// Survey/filter/decision are unused in v1.
//
// Staging runs with bounded concurrency (a.concurrency). A failure to build
// options for, read, or commit any single file fails the whole import: the
// first such error is returned after all in-flight work has drained. The
// receipt-level validation errors (wrong media type, non-apple matcher_id,
// missing/empty tree) are reported before any staging begins.
func (a *appleImporter) Import(
	ctx context.Context,
	path string,
	_ importer.SurveyFile,
	_ importer.FilterConfig,
	_ importer.ResumeDecision,
) error {
	log := a.fw.Logger.With("source", path)

	// --- 1) Read + parse the receipt ------------------------------------
	metaPath := filepath.Join(path, "metadata.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read archive metadata %s: %w", metaPath, err)
	}
	var receipt archives.FinalizeReceipt
	if err := json.Unmarshal(metaBytes, &receipt); err != nil {
		return fmt.Errorf("parse archive metadata %s: %w", metaPath, err)
	}

	// --- 2) Validate that this is an Apple delivery ---------------------
	// media_type must be the generic tarball shape AND matcher_id must claim
	// the apple/ namespace. This is how the apple importer claims only Apple
	// deliveries; anything else is refused so a misrouted archive fails
	// loudly instead of being staged with the wrong provenance semantics.
	if receipt.MediaType != appleMediaType {
		return fmt.Errorf(
			"archive media_type %q is not %q; refusing to import as apple",
			receipt.MediaType, appleMediaType,
		)
	}
	if !strings.HasPrefix(receipt.MatcherID, appleMatcherPrefix) {
		return fmt.Errorf(
			"archive matcher_id %q is not in the %q namespace; refusing to import as apple",
			receipt.MatcherID, appleMatcherPrefix,
		)
	}

	// --- 3) Enumerate tree/ files ---------------------------------------
	treeRoot := filepath.Join(path, "tree")
	info, err := os.Stat(treeRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("apple archive %q has no tree/ directory; expected content under %s", receipt.ArchiveID, treeRoot)
		}
		return fmt.Errorf("stat tree dir %s: %w", treeRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("apple archive %q tree path %s is not a directory", receipt.ArchiveID, treeRoot)
	}

	var rels []string
	walkErr := filepath.WalkDir(treeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Skip symlinks/devices/etc.; only regular files are content.
			return nil
		}
		rel, err := filepath.Rel(treeRoot, p)
		if err != nil {
			return fmt.Errorf("relativize tree path %s: %w", p, err)
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk tree dir %s: %w", treeRoot, walkErr)
	}
	if len(rels) == 0 {
		return fmt.Errorf("apple archive %q tree/ is empty; expected at least one content file", receipt.ArchiveID)
	}

	log.Info("staging apple archive",
		"archive_id", receipt.ArchiveID,
		"matcher_id", receipt.MatcherID,
		"files", len(rels),
		"concurrency", a.concurrency)

	// --- 4) Stage each file with bounded concurrency --------------------
	// ctx is dropped here because the downstream calls (ReadFile, NewItem,
	// WriteContent, Commit) are not yet context-aware. HTTP-backend Commit
	// context-awareness is tracked by the existing TODO in
	// connector/staging.go; this closure will be updated once that lands.
	stage := func(_ context.Context, rel string) error {
		entry := appleEntry{RelPath: rel, ContentType: classifyContentType(rel)}
		opts, err := BuildItemOptions(entry, &receipt, a.fw.Matcher, a.sourceName, a.fixedTags)
		if err != nil {
			return fmt.Errorf("build_item_options %s: %w", rel, err)
		}
		content, err := os.ReadFile(filepath.Join(treeRoot, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("read tree file %s: %w", rel, err)
		}
		item, err := a.fw.Backend.NewItem(opts)
		if err != nil {
			return fmt.Errorf("backend_new_item %s: %w", rel, err)
		}
		if err := item.WriteContent(content); err != nil {
			return fmt.Errorf("write_content %s: %w", rel, err)
		}
		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", rel, err)
		}
		return nil
	}

	results := stageAll(ctx, a.concurrency, rels, stage)

	var (
		staged   int
		firstErr error
	)
	for _, r := range results {
		if r.Err != nil {
			if firstErr == nil {
				firstErr = r.Err
			}
			log.Error("stage item failed", "rel_path", r.RelPath, "error", r.Err)
			continue
		}
		staged++
	}

	// --- 5) media-services: additively normalize purchase-history CSVs -----
	// The passthrough above keeps every raw file; for the media-services
	// bucket we additionally emit queryable markdown items so OpenClaw can
	// answer purchase questions (openclaw-e6f). Only attempted if passthrough
	// staging succeeded so a partial failure is not masked.
	if firstErr == nil && receipt.MatcherID == mediaServicesBucket {
		normalized, mErr := a.stageMediaServicesPurchases(treeRoot, &receipt)
		staged += normalized
		if mErr != nil {
			firstErr = mErr
			log.Error("media-services normalization failed", "error", mErr)
		} else {
			log.Info("media-services purchases normalized", "items", normalized)
		}
	}

	log.Info("apple import finished",
		"archive_id", receipt.ArchiveID,
		"staged", staged,
		"failed", len(results)-staged)

	if firstErr != nil {
		return fmt.Errorf("stage apple archive %q: %w", receipt.ArchiveID, firstErr)
	}
	return nil
}

// stubSurvey is a no-op SurveyFile returned by Survey until the real survey
// type lands. IsStale always returns false so that RunOneShot does not re-run
// Survey on every invocation.
type stubSurvey struct{}

func (s *stubSurvey) IsStale(_ string) (bool, error) {
	return false, nil
}
