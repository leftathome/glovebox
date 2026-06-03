// importer.go -- concrete walhelmImporter satisfying importer.Importer.
//
// Delivered importer for walhelm staged archives (spec 15 sec 6). Import reads
// the finalize receipt (metadata.json), validates media type and data_subject,
// walks tree/, and stages one item per regular file via a bounded goroutine
// pool (stageAll). Provenance fields (subject, audience, identity) are stamped
// from the receipt on every item.
//
// Survey, LoadSurvey, LoadManifest, LoadFilter, and ClearState are
// intentional no-ops for v1: walhelm archives are one-shot imports and do not
// require resume, survey sidecars, or filter configs in the initial release.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/importer"
	"github.com/leftathome/glovebox/internal/ingest/archives"
)

// walhelmMediaType is the only media_type a walhelm staged archive may
// carry. The ingest finalize step stamps it (spec 15 sec 4.4); the
// importer refuses anything else so a misrouted archive (e.g. an mbox
// tree) fails loudly instead of being staged with the wrong provenance
// semantics.
const walhelmMediaType = "archive/walhelm-export"

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

// Import reads the finalized archive's metadata.json (a walhelm
// FinalizeReceipt), validates that it is a walhelm archive carrying a
// subject principal, then walks <path>/tree/ and stages one item per
// regular file. Provenance (subject, audience, identity) comes from the
// receipt; the matcher only selects the destination agent (see
// BuildItemOptions). Survey/filter/decision are unused in v1.
//
// Staging runs with bounded concurrency (m.concurrency). A failure to
// build options for, read, or commit any single file fails the whole
// import: the first such error is returned after all in-flight work has
// drained. The receipt-level validation errors (wrong media type, empty
// subject, missing/empty tree) are reported before any staging begins.
func (w *walhelmImporter) Import(
	ctx context.Context,
	path string,
	_ importer.SurveyFile,
	_ importer.FilterConfig,
	_ importer.ResumeDecision,
) error {
	log := w.fw.Logger.With("source", path)

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

	// --- 2) Validate provenance -----------------------------------------
	if receipt.MediaType != walhelmMediaType {
		return fmt.Errorf(
			"archive media_type %q is not %q; refusing to import as walhelm",
			receipt.MediaType, walhelmMediaType,
		)
	}
	if receipt.DataSubject == "" {
		return fmt.Errorf(
			"walhelm archive %q has empty data_subject; a walhelm archive must carry a subject principal",
			receipt.ArchiveID,
		)
	}

	// --- 3) Enumerate tree/ files ---------------------------------------
	treeRoot := filepath.Join(path, "tree")
	info, err := os.Stat(treeRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("walhelm archive %q has no tree/ directory; expected content under %s", receipt.ArchiveID, treeRoot)
		}
		return fmt.Errorf("stat tree dir %s: %w", treeRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("walhelm archive %q tree path %s is not a directory", receipt.ArchiveID, treeRoot)
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
		return fmt.Errorf("walhelm archive %q tree/ is empty; expected at least one content file", receipt.ArchiveID)
	}

	log.Info("staging walhelm archive",
		"archive_id", receipt.ArchiveID,
		"data_subject", receipt.DataSubject,
		"files", len(rels),
		"concurrency", w.concurrency)

	// --- 4) Stage each file with bounded concurrency --------------------
	// ctx is dropped here because the downstream calls (ReadFile, NewItem,
	// WriteContent, Commit) are not yet context-aware. HTTP-backend Commit
	// context-awareness is tracked by the existing TODO in
	// connector/staging.go; this closure will be updated once that lands.
	stage := func(_ context.Context, rel string) error {
		entry := walhelmEntry{RelPath: rel, ContentType: classifyContentType(rel)}
		opts, err := BuildItemOptions(entry, &receipt, w.fw.Matcher, w.sourceName, w.fixedTags)
		if err != nil {
			return fmt.Errorf("build_item_options %s: %w", rel, err)
		}
		content, err := os.ReadFile(filepath.Join(treeRoot, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("read tree file %s: %w", rel, err)
		}
		item, err := w.fw.Backend.NewItem(opts)
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

	results := stageAll(ctx, w.concurrency, rels, stage)

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

	log.Info("walhelm import finished",
		"archive_id", receipt.ArchiveID,
		"staged", staged,
		"failed", len(results)-staged)

	if firstErr != nil {
		return fmt.Errorf("stage walhelm archive %q: %w", receipt.ArchiveID, firstErr)
	}
	return nil
}

// stubSurvey is a no-op SurveyFile returned by Survey until T8
// implements the real survey type. IsStale always returns false so
// that RunOneShot does not re-run Survey on every invocation.
type stubSurvey struct{}

func (s *stubSurvey) IsStale(_ string) (bool, error) {
	return false, nil
}
