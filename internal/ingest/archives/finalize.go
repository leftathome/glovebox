// Package archives -- finalize step (spec 13 §4.6).
//
// This file owns the post-PATCH sequence that turns an
// .tmp-archives/<upload-id> file + an UploadState into a published
// archives/<archive_id>/ directory tree. The single atomic publish
// event is the os.Rename at the bottom of Finalize; everything before
// it stages content into a sibling .tmp-archives/<upload-id>.finalize/
// dir and every failure path tears that dir down before returning.
//
// Behavior per spec §4.6 (in order):
//  1. sha256 verify -- constant-time compare against the claimed hex.
//  2. size verify  -- on-disk file size == declared Upload-Length.
//  3. Build .finalize/ dir at mode 0700 (spec §5.2 permissions).
//  4. Media-shape dispatch:
//     - MediaRaw: rename the tmp file under .finalize/raw/<archive_filename>.
//     - MediaTar: stream-untar through B3's Untar into .finalize/tree/;
//       remove the tmp file on success.
//  5. Write metadata.json LAST inside .finalize/ via atomic temp+rename.
//  6. os.Rename(.finalize, archives/<archive_id>) -- single atomic publish.
//
// Every error path (sha256 mismatch, size mismatch, mkdir failure,
// untar failure, metadata write failure, final rename collision) MUST
// remove BOTH .tmp-archives/<upload-id> AND .tmp-archives/<upload-id>.finalize/
// before returning, so a retried upload from the same client starts
// from a clean slate. cleanupTmp() centralizes this for symmetry.
//
// The rename-collision case (archives/<archive_id>/ already exists) is
// surfaced as ErrArchiveExists; spec §4.3's "archive_id_conflict"
// 409 is the caller's translation (the C2b PATCH handler maps the
// sentinel to the HTTP response).
package archives

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/internal/ingest"
)

// FinalizeReceipt is the spec §4.8 receipt JSON shape. It is the
// content of the staged archive's metadata.json AND the body of
// GET /v1/archives/<archive_id>. The Identity block (spec 06 §5.2)
// is embedded under "identity" per the iteration-3 contract pulled
// in from Wave A3.
//
// RawFilename is present only for raw-file media shapes; the
// omitempty tag suppresses it for tarball deliveries.
//
// Acquisition, DataSubject, and Audience carry producer-asserted
// provenance per spec 15 §4.4. They are omitted when empty so
// existing media types (mbox, google-takeout-subtree, etc.) that do
// not supply provenance are unaffected.
type FinalizeReceipt struct {
	ArchiveID        string           `json:"archive_id"`
	ReceivedAt       time.Time        `json:"received_at"`
	DeliveredBy      string           `json:"delivered_by"`
	Identity         *ingest.Identity `json:"identity"`
	MediaType        string           `json:"media_type"`
	SizeBytes        int64            `json:"size_bytes"`
	SHA256           string           `json:"sha256"`
	SHA256Verified   bool             `json:"sha256_verified"`
	StagedPath       string           `json:"staged_path"`
	EntriesExtracted int              `json:"entries_extracted"`
	RawFilename      string           `json:"raw_filename,omitempty"`
	Acquisition      *ingest.Identity `json:"acquisition,omitempty"`
	DataSubject      string           `json:"data_subject,omitempty"`
	Audience         []string         `json:"audience,omitempty"`
}

// FinalizeConfig parameterizes Finalize. StagingRoot is the
// <staging_root> path under which archives/ and .tmp-archives/ live;
// in production this is GLOVEBOX_STAGING_DIR. Tests use t.TempDir().
type FinalizeConfig struct {
	StagingRoot string
}

// Sentinel errors returned by Finalize. The C2b handler inspects each
// with errors.Is to decide the HTTP response shape:
//   - ErrSHAMismatch   -> 400 + {"error":"sha256_mismatch", ...}
//   - ErrSizeMismatch  -> 400 + {"error":"size_mismatch", ...}
//   - ErrArchiveExists -> 409 + {"error":"archive_id_conflict"} per §4.3
//
// Tar-safety violations from Untar are returned with %w-wrapping of the
// B3 sentinels (ErrTarPathTraversal, ErrTarTypeflag, etc.); callers
// classify via ClassifyReason.
var (
	ErrSHAMismatch   = errors.New("sha256 mismatch")
	ErrSizeMismatch  = errors.New("size mismatch")
	ErrArchiveExists = errors.New("archive_id already exists")
)

// Mode constants per spec §5.2. Directories at 0700, files at 0600.
// Explicit os.Chmod is applied after creation because the process
// umask may widen the permissions otherwise.
const (
	finalizeDirMode  os.FileMode = 0o700
	finalizeFileMode os.FileMode = 0o600
)

// Finalize takes a fully-uploaded UploadState whose tmp file lives at
// <StagingRoot>/.tmp-archives/<upload-id> and produces a published
// <StagingRoot>/archives/<archive_id>/ tree. Returns the receipt on
// success or one of the sentinel errors on failure. Every failure path
// cleans up both .tmp-archives/<upload-id> and .tmp-archives/<upload-id>.finalize/.
//
// ctx is consumed to build the Identity block via ingest.BuildIdentity;
// production callers route the auth middleware's WithDeliveredBy through
// here so the staged metadata.json carries the spec 06 §5.2 block. A
// ctx without delivered_by produces a nil Identity field; this is
// permitted at the type level so tests can exercise finalize without
// plumbing auth state, but production callers MUST set delivered_by
// before reaching Finalize (the auth middleware does this in C2).
func Finalize(ctx context.Context, st *UploadState, cfg FinalizeConfig) (*FinalizeReceipt, error) {
	tmpPath := filepath.Join(cfg.StagingRoot, ".tmp-archives", st.ID)
	finalizeDir := filepath.Join(cfg.StagingRoot, ".tmp-archives", st.ID+".finalize")

	// Step 1: sha256 verify. Constant-time compare so attacker-controlled
	// PATCH bodies cannot derive the hash via timing.
	computed := st.Hasher.Sum(nil)
	claimedBytes, decErr := hex.DecodeString(st.Meta.SHA256)
	if decErr != nil {
		// Metadata parser already validates sha256 is 64 lowercase hex
		// chars, so a decode error here is an internal invariant
		// violation. Surface it as a wrapped sha mismatch (the simpler
		// caller mapping) AFTER cleanup.
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("%w: invalid claimed hex: %v", ErrSHAMismatch, decErr)
	}
	if subtle.ConstantTimeCompare(computed, claimedBytes) != 1 {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, ErrSHAMismatch
	}

	// Step 2: size verify.
	fi, err := os.Stat(tmpPath)
	if err != nil {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("stat tmp: %w", err)
	}
	if fi.Size() != st.UploadLength {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, ErrSizeMismatch
	}

	// Step 3: build .finalize/ dir (mode 0700, defended against umask).
	if err := os.MkdirAll(finalizeDir, finalizeDirMode); err != nil {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("mkdir finalize: %w", err)
	}
	if err := os.Chmod(finalizeDir, finalizeDirMode); err != nil {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("chmod finalize: %w", err)
	}

	// Build the receipt skeleton; per-shape branches fill EntriesExtracted
	// / RawFilename in step 4.
	receipt := &FinalizeReceipt{
		ArchiveID:      st.ArchiveID,
		ReceivedAt:     time.Now().UTC(),
		DeliveredBy:    st.SourceID,
		Identity:       ingest.BuildIdentity(ctx),
		MediaType:      st.Meta.MediaType,
		SizeBytes:      st.UploadLength,
		SHA256:         st.Meta.SHA256,
		SHA256Verified: true,
		StagedPath:     filepath.Join("archives", st.ArchiveID),
	}
	// Spec 15 §4.4: carry producer-asserted provenance into the receipt.
	// Acquisition is only populated when acq_provider is set; the other
	// two acq_* fields are always present for validated provenance media
	// types, so we include them unconditionally when the block is written.
	if st.Meta.AcqProvider != "" {
		receipt.Acquisition = &ingest.Identity{
			Provider:   st.Meta.AcqProvider,
			AuthMethod: st.Meta.AcqAuthMethod,
			AccountID:  st.Meta.AcqAccountID,
		}
	}
	receipt.DataSubject = st.Meta.DataSubject
	receipt.Audience = st.Meta.Audience

	// Step 4: dispatch on media shape.
	switch st.Meta.Shape() {
	case MediaRaw:
		rawDir := filepath.Join(finalizeDir, "raw")
		if err := os.MkdirAll(rawDir, finalizeDirMode); err != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("mkdir raw: %w", err)
		}
		if err := os.Chmod(rawDir, finalizeDirMode); err != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("chmod raw: %w", err)
		}
		dst := filepath.Join(rawDir, st.Meta.ArchiveFilename)
		if err := os.Rename(tmpPath, dst); err != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("rename tmp -> raw: %w", err)
		}
		// Force 0600 on the staged file; the rename preserves the source
		// file's mode bits which may have been wider than we want.
		if err := os.Chmod(dst, finalizeFileMode); err != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("chmod raw file: %w", err)
		}
		receipt.RawFilename = st.Meta.ArchiveFilename
		receipt.EntriesExtracted = 0
	case MediaTar:
		treeDir := filepath.Join(finalizeDir, "tree")
		if err := os.MkdirAll(treeDir, finalizeDirMode); err != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("mkdir tree: %w", err)
		}
		if err := os.Chmod(treeDir, finalizeDirMode); err != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("chmod tree: %w", err)
		}
		// Stream the tmp file through Untar (B3). Open + defer-close so
		// the descriptor is released regardless of which branch we exit
		// through.
		f, oerr := os.Open(tmpPath)
		if oerr != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("open tmp for untar: %w", oerr)
		}
		entries, _, uerr := Untar(f, UntarConfig{
			DestDir:      treeDir,
			UploadLength: st.UploadLength,
		})
		// Close BEFORE removing the tmp file on Linux so we never hold a
		// stale descriptor against a deleted inode while attempting
		// further work on finalize state.
		if cerr := f.Close(); cerr != nil && uerr == nil {
			uerr = fmt.Errorf("close tmp after untar: %w", cerr)
		}
		if uerr != nil {
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("untar: %w", uerr)
		}
		// Untar succeeded; the tmp file is no longer needed.
		if rerr := os.Remove(tmpPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			// Best-effort: log via the wrapped error and still proceed
			// to finalize. We do not want to undo a successful untar
			// just because the tmp removal hiccuped, but we DO want to
			// surface the anomaly. The caller can decide whether to
			// treat this as fatal; v1 chooses fatal so leftover files
			// don't accumulate.
			cleanupTmp(cfg.StagingRoot, st.ID)
			return nil, fmt.Errorf("remove tmp after untar: %w", rerr)
		}
		receipt.EntriesExtracted = entries
	default:
		// Defense-in-depth: B1's allow-list rejects unknown media types
		// at POST, so this branch is unreachable in production. If we
		// reach it, treat as an internal invariant violation.
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("internal: unknown media shape for %q", st.Meta.MediaType)
	}

	// Step 5: write metadata.json LAST inside .finalize/. Atomic write
	// via temp+rename within the same dir keeps the publish-step rename
	// (step 6) seeing a complete, sealed metadata.json.
	metaPath := filepath.Join(finalizeDir, "metadata.json")
	metaBytes, mErr := json.MarshalIndent(receipt, "", "  ")
	if mErr != nil {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("marshal receipt: %w", mErr)
	}
	if err := atomicWriteSibling(metaPath, metaBytes, finalizeFileMode); err != nil {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("write metadata: %w", err)
	}

	// Step 6: atomic publish via os.Rename. If the target archives/<id>/
	// already exists, the rename FAILS with EEXIST/ENOTEMPTY (Linux
	// rename(2) refuses to overwrite a non-empty target). We surface
	// this as ErrArchiveExists so the C2b handler can map to 409 per
	// spec §4.3 while preserving the pre-existing archives/<id>/
	// (we only delete OUR finalize dir, never the conflicting one).
	finalPath := filepath.Join(cfg.StagingRoot, "archives", st.ArchiveID)
	// Ensure the parent archives/ dir exists; production wires create
	// this at boot, but tests using t.TempDir() may not pre-create it.
	if err := os.MkdirAll(filepath.Dir(finalPath), finalizeDirMode); err != nil {
		cleanupTmp(cfg.StagingRoot, st.ID)
		return nil, fmt.Errorf("mkdir archives parent: %w", err)
	}
	if _, statErr := os.Stat(finalPath); statErr == nil {
		// Pre-existing archive at the target. Do NOT call os.Rename --
		// on Linux it would refuse with ENOTEMPTY, but we want a
		// deterministic sentinel here regardless of the kernel-level
		// rename(2) behavior (some filesystems/OSes overwrite empty
		// targets). Explicit pre-check is the contract.
		_ = os.RemoveAll(finalizeDir)
		// Do NOT cleanupTmp() the tmp file here -- it has already been
		// renamed (raw) or removed (tar) earlier in step 4, so there
		// is nothing left to clean at the tmp path. cleanupTmp() is
		// idempotent so calling it would also be safe; we skip it for
		// clarity.
		return nil, ErrArchiveExists
	}
	if err := os.Rename(finalizeDir, finalPath); err != nil {
		_ = os.RemoveAll(finalizeDir)
		return nil, fmt.Errorf("rename to final: %w", err)
	}

	return receipt, nil
}

// cleanupTmp removes both the tmp upload file and the finalize-staging
// dir for upload-id. Idempotent: missing paths are not errors. Called
// from every Finalize failure path AND from external callers (DELETE,
// orphan cleanup goroutine) that need the same teardown semantics.
//
// The two paths are removed in fixed order (file first, then dir)
// because on filesystems where the dir's parent inode is briefly
// inconsistent between the two RemoveAlls, an external observer (the
// importer fsnotify watcher) sees the file disappear before the dir
// transitions -- but the importer never watches .tmp-archives/, so
// ordering is immaterial here. Keeping it deterministic helps tests.
func cleanupTmp(stagingRoot, uploadID string) {
	_ = os.Remove(filepath.Join(stagingRoot, ".tmp-archives", uploadID))
	_ = os.RemoveAll(filepath.Join(stagingRoot, ".tmp-archives", uploadID+".finalize"))
}

// atomicWriteSibling writes data to path via a sibling temp file +
// rename so an observer never sees a partial file at path.
//
// TODO: switch to internal/atomicfile.WriteFile once that package is
// tracked. The semantics here are equivalent (temp+fsync+rename) but
// inlined to avoid depending on an untracked package per the plan's
// guidance.
func atomicWriteSibling(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpPath := filepath.Join(dir, base+".tmp")

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	// Belt-and-suspenders chmod in case umask widened the mode bits.
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

