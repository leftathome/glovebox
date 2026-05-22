// Package archives implements tar safety + streaming extract for archive
// uploads. See docs/specs/13-archive-delivery-design.md §4.7 for the
// closed-enum rejection rules this code enforces.
package archives

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Caps from spec §4.7.
const (
	untarPathMax       = 4096      // total resolved name <= PATH_MAX
	untarNameMax       = 255       // each path component <= NAME_MAX
	untarMaxEntries    = 1_000_000 // total entry count cap
	untarSizeFactor    = 2         // cumulative cap = factor * UploadLength
	untarFileMode      = 0o600     // extracted regular file mode
	untarDirMode       = 0o700     // extracted directory mode
)

// Sentinel errors. Each maps to a closed-enum `reason` label in spec §4.7.
// Wrapped with %w + an entry path / typeflag value where useful for logging.
var (
	ErrTarPaxOverride   = errors.New("pax path/linkpath override rejected")
	ErrTarTypeflag      = errors.New("disallowed typeflag")
	ErrTarNameInvalid   = errors.New("entry name invalid")
	ErrTarPathTraversal = errors.New("entry name path traversal")
	ErrTarTooLarge      = errors.New("entry or cumulative size exceeded cap")
	ErrTarTooMany       = errors.New("entry count exceeded cap")
)

// ClassifyReason maps an Untar error to its coarsest matching spec
// §4.7 rejection reason string. C1's finalize handler uses this to feed
// EventTarSafetyReject + RecordTarSafetyReject without re-running the
// validator.
//
// The mapping is COARSE: ErrTarNameInvalid collapses several spec
// reasons (name_invalid_utf8 / name_contains_nul / name_contains_control
// / name_too_long) into the umbrella "name_invalid"; ErrTarPathTraversal
// collapses name_absolute + name_traversal. Finer granularity is a
// follow-up — the validator helpers (validUntarName, hasPathTraversal)
// would need to return typed *TarError values carrying the precise
// sub-reason. Documented as a known limitation; the metric's `reason`
// label retains operational usefulness at the coarse level.
//
// Returns ok=false if err is nil or not a known Untar sentinel. In that
// case the caller should log the raw error and skip the metric emission
// (or use an "unknown" bucket).
func ClassifyReason(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrTarPaxOverride):
		return "pax_path_override", true
	case errors.Is(err, ErrTarTypeflag):
		return "typeflag_disallowed", true
	case errors.Is(err, ErrTarNameInvalid):
		return "name_invalid", true
	case errors.Is(err, ErrTarPathTraversal):
		return "name_traversal", true
	case errors.Is(err, ErrTarTooLarge):
		return "size_too_large", true
	case errors.Is(err, ErrTarTooMany):
		return "entry_count_too_large", true
	default:
		return "", false
	}
}

// errExceededCap is the internal signal from writeCapped indicating the
// per-entry stream produced more bytes than the remaining cumulative budget.
// Surfaced to callers as ErrTarTooLarge.
var errExceededCap = errors.New("exceeded cumulative cap")

// UntarConfig parameterizes Untar.
//
// DestDir MUST already exist (caller mkdir'd) and is treated as the root of
// extraction. UploadLength is the declared total upload size, used to
// compute the per-entry cap (Size <= UploadLength) and the cumulative cap
// (sum of bytes written <= 2 * UploadLength). MaxEntries optionally
// overrides the 1,000,000 default cap; tests use this to verify the
// entry-count rejection path without producing a million-entry tar.
type UntarConfig struct {
	DestDir      string
	UploadLength int64
	// MaxEntries, when > 0, overrides untarMaxEntries. Tests set this to a
	// small value to exercise the entry-count cap; production code leaves
	// it zero and gets the spec default.
	MaxEntries int
}

// Untar streams src (a tar archive body) into cfg.DestDir, applying every
// spec §4.7 safety rule. Returns the number of entries extracted plus the
// cumulative bytes written. On any rule violation returns (n, w, err)
// where err is one of the Err* sentinels and the caller is expected to
// delete DestDir.
//
// The cumulative-size cap is evaluated against BYTES ACTUALLY WRITTEN to
// disk, incrementally during the per-entry write loop, per spec §4.7
// step 6's iteration-3 wording.
func Untar(src io.Reader, cfg UntarConfig) (entries int, written int64, err error) {
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = untarMaxEntries
	}

	tr := tar.NewReader(src)
	for {
		// Step (entry-count cap) — evaluated BEFORE per-entry processing so
		// a tar that exceeds the count cap reports entry_count_too_large
		// rather than the first bad entry's reason.
		if entries >= maxEntries {
			return entries, written, ErrTarTooMany
		}

		h, terr := tr.Next()
		if errors.Is(terr, io.EOF) {
			return entries, written, nil
		}
		if terr != nil {
			return entries, written, fmt.Errorf("tar read: %w", terr)
		}

		// Step 1: reject pax overrides on key `path` or `linkpath`. Go's
		// archive/tar merges other pax records (mtime, uid, ...) silently;
		// the spec explicitly forbids the two name-override keys.
		if h.PAXRecords != nil {
			if _, ok := h.PAXRecords["path"]; ok {
				return entries, written, ErrTarPaxOverride
			}
			if _, ok := h.PAXRecords["linkpath"]; ok {
				return entries, written, ErrTarPaxOverride
			}
		}

		// Step 2: typeflag allow-list — only regular files and directories.
		if err := validateTypeflag(h.Typeflag); err != nil {
			return entries, written, err
		}

		name := h.Name

		// Step 3: name validity (UTF-8, NUL, control chars, length caps).
		if !validUntarName(name) {
			return entries, written, ErrTarNameInvalid
		}

		// Step 4: path safety (absolute, traversal, double-slash, trailing
		// slash on TypeReg).
		if hasPathTraversal(name, h.Typeflag) {
			return entries, written, ErrTarPathTraversal
		}

		dst := filepath.Join(cfg.DestDir, filepath.Clean(name))

		// Defense in depth: confirm dst is still under DestDir after
		// filepath.Clean resolved any `.` segments (`..` is already
		// rejected by hasPathTraversal but verify again).
		rel, rerr := filepath.Rel(cfg.DestDir, dst)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return entries, written, ErrTarPathTraversal
		}

		if h.Typeflag == tar.TypeDir {
			// Step 5: directory mode is 0700 regardless of tar header bits.
			if mkerr := os.MkdirAll(dst, untarDirMode); mkerr != nil {
				return entries, written, fmt.Errorf("mkdir: %w", mkerr)
			}
			// Force mode in case the directory already existed with a
			// different mode (MkdirAll is no-op if dir exists).
			if cherr := os.Chmod(dst, untarDirMode); cherr != nil {
				return entries, written, fmt.Errorf("chmod dir: %w", cherr)
			}
			entries++
			continue
		}

		// Regular file path.
		// Step 6 (per-entry size cap): declared Size <= UploadLength.
		if h.Size < 0 || h.Size > cfg.UploadLength {
			return entries, written, ErrTarTooLarge
		}

		// Ensure parent directory exists with 0700 mode (covers nested
		// files whose parent directory header was not explicitly listed).
		if mkerr := os.MkdirAll(filepath.Dir(dst), untarDirMode); mkerr != nil {
			return entries, written, fmt.Errorf("mkdir parent: %w", mkerr)
		}

		// Step 5: file mode is 0600 regardless of tar header bits; never
		// honor set-uid / set-gid / sticky. O_EXCL prevents overwriting an
		// existing path (each entry name must be unique within the
		// archive — duplicates indicate a malformed/malicious tar).
		f, oerr := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, untarFileMode)
		if oerr != nil {
			return entries, written, fmt.Errorf("create file: %w", oerr)
		}

		// Step 6 (cumulative cap): writeCapped reads from tr (which is
		// bounded by h.Size at the tar layer) and stops at the remaining
		// cumulative budget, returning errExceededCap if the entry's
		// stream would push the running total over 2 * UploadLength.
		remaining := cfg.UploadLength*untarSizeFactor - written
		n, werr := writeCapped(f, tr, remaining)
		closeErr := f.Close()
		written += n
		if werr != nil {
			if errors.Is(werr, errExceededCap) {
				return entries, written, ErrTarTooLarge
			}
			return entries, written, fmt.Errorf("write entry: %w", werr)
		}
		if closeErr != nil {
			return entries, written, fmt.Errorf("close entry: %w", closeErr)
		}

		// Belt-and-suspenders: enforce 0600 mode after write even though
		// OpenFile already set it, in case umask or filesystem semantics
		// produced a wider mode.
		if cherr := os.Chmod(dst, untarFileMode); cherr != nil {
			return entries, written, fmt.Errorf("chmod file: %w", cherr)
		}

		entries++
	}
}

// writeCapped writes from src to dst stopping at limit bytes. Returns
// errExceededCap if src produced more bytes than limit (detected via a
// trailing 1-byte probe after io.Copy returns).
func writeCapped(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		// Cumulative budget already exhausted — any further bytes are
		// over-cap. Probe directly.
		var probe [1]byte
		if m, _ := src.Read(probe[:]); m > 0 {
			return int64(m), errExceededCap
		}
		return 0, nil
	}
	n, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return n, err
	}
	// If src still has more bytes, the entry blew the cumulative cap.
	var probe [1]byte
	if m, _ := src.Read(probe[:]); m > 0 {
		// Best-effort write the probe byte so `written` reflects what we
		// actually attempted to read off the stream; ignore the write
		// error because we are about to reject anyway.
		if m > 0 {
			_, _ = dst.Write(probe[:m])
		}
		return n + int64(m), errExceededCap
	}
	return n, nil
}

// validUntarName implements spec §4.7 step 3: UTF-8 valid, non-empty, no
// NUL, no C0 control or DEL, total length <= PATH_MAX, each component
// <= NAME_MAX.
func validUntarName(name string) bool {
	if name == "" || len(name) > untarPathMax {
		return false
	}
	if strings.ContainsRune(name, 0) {
		return false
	}
	if !utf8.ValidString(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	for _, comp := range strings.Split(name, "/") {
		if len(comp) > untarNameMax {
			return false
		}
	}
	return true
}

// validateTypeflag implements spec §4.7 step 2: only TypeReg and TypeDir
// are permitted. Every other typeflag (symlink, hardlink, char/block/fifo
// device, GNU sparse, GNU long-name/long-link, etc.) is rejected and the
// offending value is wrapped onto ErrTarTypeflag for log surfaces.
//
// Note: Go's archive/tar reader pre-processes TypeGNULongName and
// TypeGNULongLink internally (their bodies are merged into the following
// entry's Name/Linkname), so those typeflag bytes are never observed by
// callers of tar.Reader.Next(). This validator still rejects them as
// defense-in-depth in case a future Go release stops auto-merging them.
func validateTypeflag(t byte) error {
	switch t {
	case tar.TypeReg, tar.TypeDir:
		return nil
	default:
		return fmt.Errorf("%w: %v", ErrTarTypeflag, t)
	}
}

// hasPathTraversal implements spec §4.7 step 4: absolute paths, `..`
// components, Windows drive prefixes, backslashes, double-slashes, and
// (for TypeReg) trailing slashes are rejected.
func hasPathTraversal(name string, typeflag byte) bool {
	if strings.HasPrefix(name, "/") {
		return true
	}
	if strings.Contains(name, "\\") {
		return true
	}
	if strings.Contains(name, "//") {
		return true
	}
	if strings.HasSuffix(name, "/") && typeflag == tar.TypeReg {
		return true
	}
	// Windows drive letter check (e.g. "C:foo" or "C:/foo"). Letter +
	// colon at the start of any component triggers.
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
