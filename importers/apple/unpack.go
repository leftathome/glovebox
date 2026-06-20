// unpack.go -- zip-slip-safe nested-zip extraction for Apple buckets.
//
// Apple consumer exports arrive as a tarball whose tree/ contains nested zips
// (e.g. Apple_Media_Services.zip, each holding 100+ CSVs). This file provides
// the standalone extractor that those nested zips are run through. It is a pure
// utility: a later task (glovebox-ot7v) wires it into the importer.
//
// Security posture mirrors internal/ingest/archives/untar.go: each entry name
// is rejected if, after filepath.Clean + Join, the destination escapes DestDir
// (absolute paths, backslashes, "//" runs, or any ".." component). Only regular
// files are extracted; directory entries are realized via MkdirAll and every
// other entry kind (symlinks, etc.) is skipped. Entries stream to disk one at a
// time so a large nested zip never lands wholesale in memory.
package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrZipSlip is returned when a zip entry name resolves to a path outside the
// destination directory (the "zip slip" traversal attack).
var ErrZipSlip = errors.New("zip entry escapes destination directory")

const (
	zipFileMode = 0o600 // extracted regular file mode
	zipDirMode  = 0o700 // created directory mode
)

// UnpackZip opens the zip archive at zipPath and extracts its regular-file
// entries into destDir, returning the slash-separated relative paths that were
// written (in archive order). destDir is created if it does not exist. See
// UnpackZipReader for the safety and streaming guarantees.
func UnpackZip(zipPath, destDir string) ([]string, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat zip %s: %w", zipPath, err)
	}
	return UnpackZipReader(f, info.Size(), destDir)
}

// UnpackZipReader extracts the zip whose contents are addressed by r (size
// bytes long) into destDir. It returns the slash-separated relative paths of
// every regular file written, in archive order.
//
// Behavior:
//   - destDir is created (MkdirAll, 0700) if absent.
//   - Directory entries are realized with MkdirAll; non-regular entries
//     (symlinks, devices) are skipped.
//   - Regular files are streamed entry-by-entry via io.Copy, so the whole
//     archive is never buffered in memory.
//   - Any entry whose cleaned name is absolute, contains a ".." component, a
//     backslash, or a "//" run -- or that resolves outside destDir after
//     filepath.Join+Clean -- is rejected with ErrZipSlip and extraction stops.
func UnpackZipReader(r io.ReaderAt, size int64, destDir string) ([]string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("open zip reader: %w", err)
	}

	if err := os.MkdirAll(destDir, zipDirMode); err != nil {
		return nil, fmt.Errorf("mkdir dest %s: %w", destDir, err)
	}

	var extracted []string
	for _, zf := range zr.File {
		name := zf.Name

		// Reject obviously hostile names before touching the filesystem.
		if hasZipTraversal(name) {
			return extracted, fmt.Errorf("%w: %q", ErrZipSlip, name)
		}

		dst := filepath.Join(destDir, filepath.Clean(name))

		// Defense in depth: confirm dst is still under destDir after Clean
		// resolved any "." segments. Mirrors untar.go's filepath.Rel guard.
		rel, rerr := filepath.Rel(destDir, dst)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return extracted, fmt.Errorf("%w: %q", ErrZipSlip, name)
		}

		if zf.FileInfo().IsDir() {
			if mkerr := os.MkdirAll(dst, zipDirMode); mkerr != nil {
				return extracted, fmt.Errorf("mkdir entry %s: %w", name, mkerr)
			}
			continue
		}

		// Skip non-regular entries (symlinks, devices); only regular files
		// are content.
		if !zf.Mode().IsRegular() {
			continue
		}

		if err := extractZipFile(zf, dst); err != nil {
			return extracted, err
		}
		extracted = append(extracted, filepath.ToSlash(name))
	}

	return extracted, nil
}

// extractZipFile streams a single regular-file entry to dst, creating any
// missing parent directories. The entry's reader is closed before returning.
func extractZipFile(zf *zip.File, dst string) error {
	if mkerr := os.MkdirAll(filepath.Dir(dst), zipDirMode); mkerr != nil {
		return fmt.Errorf("mkdir parent for %s: %w", zf.Name, mkerr)
	}

	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("open entry %s: %w", zf.Name, err)
	}
	defer rc.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, zipFileMode)
	if err != nil {
		return fmt.Errorf("create file for %s: %w", zf.Name, err)
	}

	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return fmt.Errorf("write entry %s: %w", zf.Name, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close entry %s: %w", zf.Name, err)
	}
	return nil
}

// hasZipTraversal rejects entry names that are absolute, use a Windows drive
// prefix, contain backslashes or "//" runs, or include a ".." path component.
// Mirrors hasPathTraversal in internal/ingest/archives/untar.go.
func hasZipTraversal(name string) bool {
	if name == "" {
		return true
	}
	if strings.HasPrefix(name, "/") {
		return true
	}
	if strings.Contains(name, "\\") {
		return true
	}
	if strings.Contains(name, "//") {
		return true
	}
	// Windows drive letter check (e.g. "C:foo" or "C:/foo").
	if len(name) >= 2 && name[1] == ':' {
		return true
	}
	for _, comp := range strings.Split(name, "/") {
		if comp == ".." {
			return true
		}
	}
	return false
}
