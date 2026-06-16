// Package atomicfile provides a single helper for durable, crash-safe file
// writes. The sole operation is WriteFile, which mirrors the paranoid
// temp-file + fsync + rename dance used by the mbox importer's manifest,
// survey, and checkpoint writers: data is written to a uniquely-named
// sibling temp file, fsynced, closed, then renamed into place. On any
// failure the temp file is removed so a crashed process does not leave
// `.tmp-*` litter behind.
//
// Why centralize: five call sites across the codebase implement this same
// sequence. Two of them (connector/checkpoint.go and connector/token.go)
// historically skipped the fsync step, which means a power-cut or kernel
// panic between the write and the rename could leave the renamed target
// with a zero-byte or partial payload. Routing every atomic write through
// this helper eliminates that durability gap in one place.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteFile writes data to path atomically.
//
// Strategy: marshal the payload into `<dir>/<base>.tmp-<pid>-<unix-nanos>`,
// fsync it, close it, then os.Rename it into place. os.Rename is atomic on
// POSIX for same-filesystem renames, so a kill -9 at any point leaves
// either the old file (if any) or the new file, never a partially-written
// target. A kill during the temp-file phase may leave a stray `.tmp-*`
// sibling; this helper best-effort removes it from its own error paths.
//
// mode is applied to the temp file at create time via os.OpenFile, subject
// to the process umask. Callers that need to guarantee mode bits (e.g.
// 0o600 for token files) should verify the resulting file's mode after a
// successful return if strict-umask guarantees matter.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf("%s.tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("atomicfile: create temp: %w", err)
	}
	// From this point on, any error path must remove the temp file.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("atomicfile: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("atomicfile: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomicfile: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("atomicfile: rename: %w", err)
	}
	return nil
}
