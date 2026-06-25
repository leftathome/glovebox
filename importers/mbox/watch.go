// watch.go -- archive-event watcher mode for the mbox importer (spec 13
// sec 5.3 importer pickup; spec 09 sec 6 V2). When the binary is started
// with --watch-archives, runWatch runs a long-lived loop that picks up
// archive/mbox archives finalized into staging/archives/, drives the
// existing per-mbox RunOneShot pipeline against each, and retires
// processed archives into archives/.done/. See
// docs/superpowers/specs/2026-06-25-mbox-archive-watcher-design.md.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/importer"
	"github.com/leftathome/glovebox/internal/watcher"
)

// defaultMediaType is the media_type the watcher claims when --media-types
// is not supplied. archive/imap-export shares the raw mbox shape and can be
// added by an operator via the flag without a code change (spec 13 sec 4.5).
const defaultMediaType = "archive/mbox"

// lockName is the mbox-importer's per-importer advisory lock file inside an
// archive dir (spec 13 sec 5.3 step 4). Per-importer naming lets a future
// Takeout importer coexist on the same archive tree without lock races.
const lockName = ".mbox-importer.lock"

// doneDir is the retention subdirectory processed archives are moved into.
const doneDir = ".done"

// defaultWatchPollInterval backs the fsnotify watch with a periodic poll.
// Archives are large and arrive infrequently, so a slow poll is plenty; it
// only needs to catch events fsnotify missed and retry backpressured items.
const defaultWatchPollInterval = 30 * time.Second

// parseMediaTypes turns a comma-separated --media-types value into a lookup
// set, trimming whitespace and dropping empty entries. An empty input yields
// the default set {archive/mbox}.
func parseMediaTypes(csv string) map[string]bool {
	out := make(map[string]bool)
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	if len(out) == 0 {
		out[defaultMediaType] = true
	}
	return out
}

// safeRawFilename verifies name is a single, safe path element: non-empty,
// not "." or "..", containing no path separators or NUL. The server already
// validates archive_filename at finalize (spec 13 sec 4.2); this is a second
// gate before we join it onto a filesystem path.
func safeRawFilename(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("raw_filename %q is not a valid file name", name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("raw_filename %q contains a path separator or NUL", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("raw_filename %q is not a single path element", name)
	}
	return nil
}

// archiveMeta is the minimal subset of the spec 13 sec 4.8 FinalizeReceipt
// (written to each archive's metadata.json) that the watcher consults. We do
// NOT import internal/ingest/archives.FinalizeReceipt: that would pull
// server-side dependencies into the importer binary. These field names are a
// stable subset of that struct's JSON.
type archiveMeta struct {
	ArchiveID   string `json:"archive_id"`
	MediaType   string `json:"media_type"`
	RawFilename string `json:"raw_filename"`
}

// readArchiveMeta loads and parses <dir>/metadata.json.
func readArchiveMeta(dir string) (archiveMeta, error) {
	var m archiveMeta
	b, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return m, fmt.Errorf("read metadata.json: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse metadata.json: %w", err)
	}
	return m, nil
}

// acquireLock creates the advisory lock with O_EXCL. A returned error for
// which os.IsExist is true means another importer/replica owns the archive.
func acquireLock(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// releaseLock removes the advisory lock. Missing-file is not an error.
func releaseLock(dir string) error {
	if err := os.Remove(filepath.Join(dir, lockName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// moveToDone retires a processed archive into archives/.done/<id> for
// retention (the spec 13 server cleanup goroutine deletes it after the
// retention window). archives/.done/ is not watched, so no re-pickup occurs.
func moveToDone(archivesDir, src, archiveID string) error {
	done := filepath.Join(archivesDir, doneDir)
	if err := os.MkdirAll(done, 0o700); err != nil {
		return fmt.Errorf("mkdir .done: %w", err)
	}
	if err := os.Rename(src, filepath.Join(done, archiveID)); err != nil {
		return fmt.Errorf("move to .done: %w", err)
	}
	return nil
}

// archiveMetrics holds the watcher's OTel counters. Registered against the
// framework's existing Prometheus exporter via fw.Metrics.Provider().
type archiveMetrics struct {
	processedTotal metric.Int64Counter
	failedTotal    metric.Int64Counter
	skippedTotal   metric.Int64Counter
}

func newArchiveMetrics(mp metric.MeterProvider) (*archiveMetrics, error) {
	meter := mp.Meter("mbox-importer")
	am := &archiveMetrics{}
	var err error
	if am.processedTotal, err = meter.Int64Counter("glovebox_mbox_archives_processed_total",
		metric.WithDescription("Archives processed and moved to .done")); err != nil {
		return nil, err
	}
	if am.failedTotal, err = meter.Int64Counter("glovebox_mbox_archives_failed_total",
		metric.WithDescription("Archive processing failures (left in place)")); err != nil {
		return nil, err
	}
	if am.skippedTotal, err = meter.Int64Counter("glovebox_mbox_archives_skipped_total",
		metric.WithDescription("Archives skipped, by reason")); err != nil {
		return nil, err
	}
	return am, nil
}

func (a *archiveMetrics) processed(ctx context.Context) { a.processedTotal.Add(ctx, 1) }
func (a *archiveMetrics) failed(ctx context.Context)    { a.failedTotal.Add(ctx, 1) }
func (a *archiveMetrics) skipped(ctx context.Context, reason string) {
	a.skippedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// archiveWatcher bundles the per-archive handler dependencies.
type archiveWatcher struct {
	ctx         context.Context
	fw          *connector.Framework
	imp         *mboxImporter
	archivesDir string
	mediaTypes  map[string]bool
	metrics     *archiveMetrics
}

// handle implements watcher.ItemHandler for one archives/<id>/ directory.
func (a *archiveWatcher) handle(dir string) {
	log := a.fw.Logger.With("archive_dir", dir)

	// Defense in depth: never act on a dot-named entry (.done, .tmp-*).
	if strings.HasPrefix(filepath.Base(dir), ".") {
		return
	}

	meta, err := readArchiveMeta(dir)
	if err != nil {
		log.Warn("skip archive: unreadable metadata.json", "err", err)
		return
	}
	log = log.With("archive_id", meta.ArchiveID, "media_type", meta.MediaType)

	if !a.mediaTypes[meta.MediaType] {
		log.Debug("skip archive: media_type not claimed")
		a.metrics.skipped(a.ctx, "media_type")
		return
	}

	if err := safeRawFilename(meta.RawFilename); err != nil {
		log.Error("skip archive: invalid raw_filename", "err", err)
		return
	}

	if err := acquireLock(dir); err != nil {
		if os.IsExist(err) {
			log.Debug("skip archive: lock held by another importer")
			a.metrics.skipped(a.ctx, "locked")
			return
		}
		log.Error("skip archive: lock error", "err", err)
		return
	}

	log.Info("processing archive")
	source := filepath.Join(dir, "raw", meta.RawFilename)
	cfg := importer.RunConfig{SourcePath: source}
	runErr := importer.RunOneShot(a.ctx, a.fw, a.imp, cfg)

	if runErr != nil {
		// Cancellation is a clean shutdown: RunOneShot left
		// manifest=interrupted; release the lock so the next pod resumes,
		// leave the archive in place. Do NOT count it as a failure.
		if a.ctx.Err() != nil {
			_ = releaseLock(dir)
			log.Warn("archive processing interrupted; left in place for resume", "err", runErr)
			return
		}
		_ = releaseLock(dir)
		a.metrics.failed(a.ctx)
		log.Error("archive processing failed; left in place", "err", runErr)
		return
	}

	if err := moveToDone(a.archivesDir, dir, meta.ArchiveID); err != nil {
		_ = releaseLock(dir)
		a.metrics.failed(a.ctx)
		log.Error("archive processed but move to .done failed; left in place", "err", err)
		return
	}
	a.metrics.processed(a.ctx)
	log.Info("archive processed and retired to .done")
}

// runWatch runs the long-lived archive watcher until ctx is cancelled. It
// ensures archivesDir exists (the importer pod may boot before the ingest
// server creates it) so the fsnotify watch attaches immediately instead of
// falling back to polling.
func runWatch(ctx context.Context, fw *connector.Framework, imp *mboxImporter,
	archivesDir string, mediaTypes map[string]bool, poll time.Duration) error {

	if err := os.MkdirAll(archivesDir, 0o700); err != nil {
		return fmt.Errorf("ensure archives dir %s: %w", archivesDir, err)
	}
	am, err := newArchiveMetrics(fw.Metrics.Provider())
	if err != nil {
		return fmt.Errorf("init archive metrics: %w", err)
	}
	aw := &archiveWatcher{
		ctx: ctx, fw: fw, imp: imp,
		archivesDir: archivesDir, mediaTypes: mediaTypes, metrics: am,
	}
	fw.Logger.Info("archive watcher started", "archives_dir", archivesDir,
		"media_types", mediaTypeKeys(mediaTypes), "poll", poll.String())
	watcher.New(archivesDir, poll, aw.handle).Run(ctx)
	return nil
}

// mediaTypeKeys returns the claimed media types as a sorted slice for logging.
func mediaTypeKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
