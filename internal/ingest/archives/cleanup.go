// Package archives -- orphan cleanup goroutine (spec 13 §5.5).
//
// On startup AND on a 60-minute interval, the cleanup loop walks the
// staging tree and deletes:
//
//   - .tmp-archives/<upload-id>          older than TmpAge          (default 72h)
//   - .tmp-archives/<upload-id>.finalize older than FinalizeAge     (default 1h)
//   - archives/.done/<archive_id>/       older than DoneRetention   (default 7d)
//
// Each pass emits a single EventCleanup log line with the counts and
// freed bytes so an operator can verify retention is doing its job.
//
// The cleanup is best-effort: a single ENOENT (the file was deleted
// between the stat and the unlink, e.g. by a concurrent finalize) is
// not an error. Errors that DO indicate a problem (EPERM, EIO) are
// logged at WARN but do not stop the pass; the next tick retries.
package archives

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupConfig parameterizes StartCleanupGoroutine. Zero values pick
// the spec §5.5 production defaults.
type CleanupConfig struct {
	// StagingRoot is the absolute path under which archives/ and
	// .tmp-archives/ live.
	StagingRoot string

	// Interval is the tick interval (default 60min per spec §5.5).
	Interval time.Duration

	// TmpAge is the max age of an in-progress upload before its tmp
	// file is reaped (default 72h).
	TmpAge time.Duration

	// FinalizeAge is the max age of a .finalize/ dir before reaping
	// (default 1h).
	FinalizeAge time.Duration

	// DoneRetention is the max age of an archives/.done/<id>/ dir
	// before reaping (default 7d).
	DoneRetention time.Duration

	// Logger is the goroutine's slog. Defaults to slog.Default()
	// when nil.
	Logger *slog.Logger

	// Telemetry is hooked in case future revisions want counters for
	// "items reaped"; currently the EventCleanup log line is the only
	// observable.
	Telemetry *Telemetry
}

// cleanupStats is a single tick's removal counters.
type cleanupStats struct {
	removedUploads      int
	removedFinalizeDirs int
	removedDoneArchives int
	freedBytes          int64
}

// StartCleanupGoroutine runs the cleanup loop until ctx is cancelled.
// The function blocks; callers MUST invoke it via `go`. One pass runs
// synchronously on startup BEFORE the first tick so a long-running
// pod restart cleans up wreckage promptly.
//
// TODO(c9zt): the archives/.done/ retention path is exercised here via
// a manually-staged fixture in tests, but in production nothing
// populates .done/ until the spec 09 watcher mode (glovebox-c9zt)
// ships. Once c9zt lands, integrate the importer pickup test path.
func StartCleanupGoroutine(ctx context.Context, cfg CleanupConfig) {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Minute
	}
	if cfg.TmpAge <= 0 {
		cfg.TmpAge = 72 * time.Hour
	}
	if cfg.FinalizeAge <= 0 {
		cfg.FinalizeAge = time.Hour
	}
	if cfg.DoneRetention <= 0 {
		cfg.DoneRetention = 7 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	runCleanupOnce(cfg)

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			runCleanupOnce(cfg)
		}
	}
}

// runCleanupOnce executes a single pass over the three retention
// targets and logs the EventCleanup summary. Exported indirectly via
// the goroutine; tests call this directly to assert effects without
// driving a real ticker. Defaults the logger when nil so direct
// callers (tests) don't have to remember to set it.
func runCleanupOnce(cfg CleanupConfig) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	now := time.Now()
	stats := cleanupStats{}

	tmpRoot := filepath.Join(cfg.StagingRoot, ".tmp-archives")
	cleanupTmpArea(cfg, tmpRoot, now, &stats)

	doneRoot := filepath.Join(cfg.StagingRoot, "archives", ".done")
	cleanupDoneArea(cfg, doneRoot, now, &stats)

	cfg.Logger.Info(EventCleanup,
		"removed_uploads", stats.removedUploads,
		"removed_finalize_dirs", stats.removedFinalizeDirs,
		"removed_done_archives", stats.removedDoneArchives,
		"freed_bytes", stats.freedBytes,
	)
}

// cleanupTmpArea reaps stale .tmp-archives/ entries. The directory
// listing distinguishes two cases by suffix:
//
//   - "<id>.finalize" (a directory) -- subject to FinalizeAge.
//   - any other entry  -- subject to TmpAge (typically the <id> tmp file).
//
// A missing tmpRoot is not an error; the directory is created lazily
// by the handler on first upload.
func cleanupTmpArea(cfg CleanupConfig, tmpRoot string, now time.Time, stats *cleanupStats) {
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			cfg.Logger.Warn("cleanup: read .tmp-archives failed",
				"path", tmpRoot, "err", err)
		}
		return
	}

	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(tmpRoot, name)
		info, ierr := e.Info()
		if ierr != nil {
			cfg.Logger.Warn("cleanup: stat entry failed",
				"path", path, "err", ierr)
			continue
		}
		age := now.Sub(info.ModTime())

		switch {
		case strings.HasSuffix(name, ".finalize") && e.IsDir():
			if age < cfg.FinalizeAge {
				continue
			}
			dirSize := dirBytes(path)
			if err := os.RemoveAll(path); err != nil {
				cfg.Logger.Warn("cleanup: remove .finalize failed",
					"path", path, "err", err)
				continue
			}
			stats.removedFinalizeDirs++
			stats.freedBytes += dirSize
		case !e.IsDir():
			if age < cfg.TmpAge {
				continue
			}
			fileSize := info.Size()
			if err := os.Remove(path); err != nil {
				cfg.Logger.Warn("cleanup: remove tmp upload failed",
					"path", path, "err", err)
				continue
			}
			stats.removedUploads++
			stats.freedBytes += fileSize
		default:
			// Unexpected directory under .tmp-archives that isn't a
			// .finalize sibling -- leave alone to avoid clobbering
			// state we don't recognize.
		}
	}
}

// cleanupDoneArea reaps archives/.done/<archive_id>/ dirs older than
// DoneRetention. A missing .done root is not an error: spec 09's
// watcher mode (c9zt) is the producer and that hasn't landed in Wave 1
// of this plan.
func cleanupDoneArea(cfg CleanupConfig, doneRoot string, now time.Time, stats *cleanupStats) {
	entries, err := os.ReadDir(doneRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			cfg.Logger.Warn("cleanup: read archives/.done failed",
				"path", doneRoot, "err", err)
		}
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(doneRoot, e.Name())
		info, ierr := e.Info()
		if ierr != nil {
			cfg.Logger.Warn("cleanup: stat .done entry failed",
				"path", path, "err", ierr)
			continue
		}
		if now.Sub(info.ModTime()) < cfg.DoneRetention {
			continue
		}
		dirSize := dirBytes(path)
		if err := os.RemoveAll(path); err != nil {
			cfg.Logger.Warn("cleanup: remove .done entry failed",
				"path", path, "err", err)
			continue
		}
		stats.removedDoneArchives++
		stats.freedBytes += dirSize
	}
}
