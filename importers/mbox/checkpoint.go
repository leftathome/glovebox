package main

// Byte-offset checkpoint file for the mbox importer.
//
// Design references:
//   - docs/specs/09-mbox-importer-design.md §3.6.1 (checkpoint removed on
//     clean completion), §3.8 (on-disk shape and behavior).
//
// A checkpoint is a tiny sidecar next to the source archive. It holds the
// byte offset the next resumed run should seek to, plus the Message-ID of
// the last successfully ingested message (for a cross-boundary dedup
// sanity check). Written every N messages during an import and on clean
// shutdown; removed when the run reaches `status == complete`.
//
// Concurrency: checkpoint writers are serialized by the importer; this
// file contains no locking.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// checkpointSuffix is appended to the source path to form the checkpoint
// path. Kept in one place so CheckpointPath and any future cleanup code
// agree on the convention.
const checkpointSuffix = ".checkpoint"

// Checkpoint is the on-disk shape of the byte-offset checkpoint file.
//
// The fields mirror ResumeState in the manifest (spec §3.6): the two
// sources of truth are kept aligned by the importer so a resume can be
// driven from either one. The checkpoint is the authoritative "where do
// we pick up" file during an in-flight run; the manifest carries the
// same values as a durability backstop and for post-run inspection.
type Checkpoint struct {
	ByteOffset            int64  `json:"byte_offset"`
	LastIngestedMessageID string `json:"last_ingested_message_id"`
}

// CheckpointPath returns the conventional checkpoint path for a given
// mbox source. The convention is `<sourcePath>.checkpoint` (spec §3.8).
func CheckpointPath(sourcePath string) string {
	return sourcePath + checkpointSuffix
}

// LoadCheckpoint reads and parses the checkpoint at path.
//
// If the file does not exist, the returned error satisfies
// errors.Is(err, fs.ErrNotExist) so callers can branch on "no checkpoint
// yet" without caring whether the underlying error is *PathError or
// syscall.ENOENT.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("mbox checkpoint: parse %s: %w", path, err)
	}
	return &c, nil
}

// Write serializes the checkpoint to path atomically.
//
// Strategy mirrors ImportManifestV1.Write: marshal, write to
// `<path>.tmp-<pid>-<unix-nanos>`, fsync, then os.Rename into place.
// os.Rename is atomic on POSIX for same-filesystem renames, so a kill
// at any point leaves either the old checkpoint or the new one, never
// a half-written file. A kill during the tmp-file phase may leave a
// stray `.tmp-*` sibling which we best-effort clean up on our own
// error paths.
//
// Output is compact (no indentation) -- a checkpoint is a few hundred
// bytes and is rewritten every N messages during an in-flight import.
//
// NOTE: This duplicates the temp+rename dance in manifest.go.Write.
// Spec §3.8 plus manifest §3.6 are the only two callers in V1; a
// follow-up bead may promote a shared helper once a third caller
// appears. Keeping the duplication inline here keeps each file's
// atomic-write invariant readable in one place.
func (c *Checkpoint) Write(path string) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("mbox checkpoint: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf("%s.tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("mbox checkpoint: create temp: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("mbox checkpoint: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("mbox checkpoint: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("mbox checkpoint: close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("mbox checkpoint: rename: %w", err)
	}
	return nil
}

// RemoveCheckpoint deletes the checkpoint file at path. Missing-file is
// not an error: per spec §3.6.1 the checkpoint is removed on clean
// completion, and a re-invocation of the same cleanup path must be safe
// (so must a fresh-start that never wrote a checkpoint in the first
// place).
func RemoveCheckpoint(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mbox checkpoint: remove %s: %w", path, err)
	}
	return nil
}
