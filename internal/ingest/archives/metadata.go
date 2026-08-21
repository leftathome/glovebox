// Package archives implements the tus.io archive-delivery endpoint per
// spec 13. This file holds the Upload-Metadata parser and validator
// (§4.2). All field validation happens BEFORE any logging or metric
// emission; the parser returns either a fully-validated *Metadata or a
// sentinel error, leaving log/metric decisions to the caller.
package archives

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/leftathome/glovebox/internal/staging"
)

// Metadata is the parsed + validated Upload-Metadata block per spec 13
// §4.2. Server-set fields (delivered_by, delivered_at) are NOT carried
// here -- they are written by the finalize path after authentication
// resolves the source-id and after the receipt is built.
//
// Spec 15 §4.2 adds producer-asserted provenance fields (AcqProvider,
// AcqAccountID, AcqAuthMethod, DataSubject, Audience). These are always
// copied from the header when present; they are ENFORCED (required) only
// for media types in requiredProvenance.
type Metadata struct {
	ArchiveID           string
	ArchiveFilename     string
	SubtreeRelativePath string
	MediaType           string
	MatcherID           string
	Provider            string
	SHA256              string // 64 lowercase hex chars
	SizeBytes           int64

	// Provenance fields (spec 15 §4.2). Required for requiredProvenance
	// media types; optional/informational for all others.
	AcqProvider   string
	AcqAccountID  string
	AcqAuthMethod string
	DataSubject   string
	Audience      []string
}

// MediaShape is "raw" or "tar"; the dispatch table in finalize.go uses
// this to choose between rename and untar.
type MediaShape string

const (
	MediaRaw MediaShape = "raw"
	MediaTar MediaShape = "tar"
)

// mediaAllowList per spec 13 §4.5. Hard-coded; adding an entry
// REQUIRES a code change. Operators cannot override this map at
// runtime.
//
// Importer coverage (downstream watcher dispatch, separate beads):
//   - archive/mbox: spec 9 mbox-importer (glovebox-c9zt, watcher
//     mode in progress).
//   - archive/google-takeout-subtree: no dedicated importer yet;
//     staged tarballs sit under archives/<id>/tree/ awaiting a
//     future Takeout-aware consumer.
//   - archive/generic-tarball: no dedicated importer yet; same
//     "staged but unrouted" status as Takeout subtrees. Added to
//     unblock recognizer's Meta/Facebook export delivery
//     (glovebox-4enb 2026-05-23) -- semantically "any uncompressed
//     tar we don't have a more specific media type for."
//   - archive/imap-export: no dedicated importer yet; reuses the
//     MediaRaw shape (the body is an mbox-shaped IMAP dump). A
//     future spec-9-style watcher can opt to handle this media_type
//     identically to archive/mbox.
//
// Earlier-draft note that adding an entry "REQUIRES ... a
// corresponding importer landing in the same release" was relaxed
// for the recognizer-shipping case: glovebox accepts + stages the
// data; the importer-side gap is tracked separately so the data
// doesn't get lost at the boundary while the watcher catches up.
var mediaAllowList = map[string]MediaShape{
	"archive/mbox":                   MediaRaw,
	"archive/google-takeout-subtree": MediaTar,
	"archive/generic-tarball":        MediaTar,
	"archive/imap-export":            MediaRaw,
	"archive/walhelm-export":         MediaTar,
	// archive/recognizer-scan (glovebox-9s60): a tar from recognizer's
	// document scanner. Finalize GATES this media type to the authenticated
	// recognizer-scanner source (fail-closed) and renders the bundled
	// pre-extracted OCR text (tree/ocr.txt) to content.extracted.md.
	"archive/recognizer-scan": MediaTar,
}

// requiredProvenance lists media types that MUST carry producer-asserted
// provenance keys (spec 15 sec 4.2). Other media types ignore these keys.
var requiredProvenance = map[string]bool{"archive/walhelm-export": true}

// Validation regexes per spec 13 §4.2.
var (
	archiveIDRe       = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)
	archiveFilenameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	matcherIDRe       = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,256}$`)
	providerRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	sha256Re          = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Sentinel errors. Callers map these to HTTP status codes:
//   - ErrMetadataMissing, ErrMetadataInvalid, ErrMetadataReservedKey,
//     ErrMetadataUnknownMediaType, ErrMetadataSizeMismatch -> 400
//   - ErrMetadataTooLong -> 431 (request header too large)
var (
	ErrMetadataMissing          = errors.New("metadata missing required key")
	ErrMetadataInvalid          = errors.New("metadata value invalid")
	ErrMetadataReservedKey      = errors.New("metadata contains server-set key")
	ErrMetadataUnknownMediaType = errors.New("metadata media_type not in allow-list")
	ErrMetadataSizeMismatch     = errors.New("metadata size_bytes != Upload-Length")
	ErrMetadataTooLong          = errors.New("upload-metadata header exceeds 4 KiB")
)

// uploadMetadataMaxBytes caps the Upload-Metadata header length per
// spec 13 §4.2. Exceeding returns 431.
const uploadMetadataMaxBytes = 4096

// archiveFilenameMaxBytes caps the archive_filename byte length per
// spec 13 §4.2 ("Max 256 bytes").
const archiveFilenameMaxBytes = 256

// subtreeRelativePathMaxBytes caps subtree_relative_path per spec 13
// §4.2 ("UTF-8, max 1024 bytes").
const subtreeRelativePathMaxBytes = 1024

// provenanceStringMaxBytes caps acq_account_id and data_subject per spec 15
// §4.2 ("max 256 bytes, no control characters").
const provenanceStringMaxBytes = 256

// ParseUploadMetadata parses the Upload-Metadata header and validates
// every field per spec 13 §4.2. uploadLength is the declared
// Upload-Length; size_bytes MUST equal it (verified at POST per the
// iteration-3 fix in §4.2, NOT deferred to finalize).
//
// Order of operations is load-bearing:
//  1. Header length cap (cheapest check, runs before decode allocates).
//  2. base64 decode of all pairs.
//  3. Reserved-key check (delivered_by / delivered_at).
//  4. Per-field assignment and size_bytes parse.
//  5. size_bytes vs uploadLength comparison.
//  6. Per-field regex / format checks.
//  7. media_type allow-list lookup.
//
// The function never logs and never emits metrics; the caller decides
// what to do with the returned error.
func ParseUploadMetadata(header string, uploadLength int64) (*Metadata, error) {
	if len(header) > uploadMetadataMaxBytes {
		return nil, ErrMetadataTooLong
	}
	raw, err := decodeMetadataPairs(header)
	if err != nil {
		return nil, err
	}
	if _, has := raw["delivered_by"]; has {
		return nil, fmt.Errorf("%w: delivered_by", ErrMetadataReservedKey)
	}
	if _, has := raw["delivered_at"]; has {
		return nil, fmt.Errorf("%w: delivered_at", ErrMetadataReservedKey)
	}

	m := &Metadata{
		ArchiveID:           raw["archive_id"],
		ArchiveFilename:     raw["archive_filename"],
		SubtreeRelativePath: raw["subtree_relative_path"],
		MediaType:           raw["media_type"],
		MatcherID:           raw["matcher_id"],
		Provider:            raw["provider"],
		SHA256:              raw["sha256"],
		AcqProvider:         raw["acq_provider"],
		AcqAccountID:        raw["acq_account_id"],
		AcqAuthMethod:       raw["acq_auth_method"],
		DataSubject:         raw["data_subject"],
	}

	sizeStr, hasSize := raw["size_bytes"]
	if !hasSize || sizeStr == "" {
		return nil, fmt.Errorf("%w: size_bytes", ErrMetadataMissing)
	}
	sz, perr := strconv.ParseInt(sizeStr, 10, 64)
	if perr != nil || sz < 0 {
		return nil, fmt.Errorf("%w: size_bytes %q", ErrMetadataInvalid, sizeStr)
	}
	m.SizeBytes = sz
	if m.SizeBytes != uploadLength {
		return nil, fmt.Errorf("%w (size_bytes=%d, Upload-Length=%d)",
			ErrMetadataSizeMismatch, m.SizeBytes, uploadLength)
	}

	if !archiveIDRe.MatchString(m.ArchiveID) {
		return nil, fmt.Errorf("%w: archive_id", ErrMetadataInvalid)
	}
	// The archive_id character class admits "." and "..", which are joined
	// onto the archive root a few frames later. No path separator is
	// allowed, so today a dot-only id resolves back to the root and the
	// pre-existing-target check happens to reject it -- but that makes an
	// os.Stat the thing standing between a crafted id and the root, which
	// is not where a traversal check belongs. archive_filename already
	// rejects "..";  archive_id now refuses an all-dots value outright.
	if strings.Trim(m.ArchiveID, ".") == "" {
		return nil, fmt.Errorf("%w: archive_id must not consist only of dots", ErrMetadataInvalid)
	}
	if !archiveFilenameRe.MatchString(m.ArchiveFilename) ||
		strings.Contains(m.ArchiveFilename, "..") ||
		len(m.ArchiveFilename) > archiveFilenameMaxBytes {
		return nil, fmt.Errorf("%w: archive_filename", ErrMetadataInvalid)
	}
	if !utf8.ValidString(m.SubtreeRelativePath) ||
		strings.ContainsRune(m.SubtreeRelativePath, '\x00') ||
		len(m.SubtreeRelativePath) > subtreeRelativePathMaxBytes ||
		hasControlChar(m.SubtreeRelativePath) {
		return nil, fmt.Errorf("%w: subtree_relative_path", ErrMetadataInvalid)
	}
	// matcherIDRe already excludes all control bytes; hasControlChar is
	// a belt-and-suspenders guard for the test plan's "matcher_id with
	// control chars" case (a control char would already fail the regex).
	if !matcherIDRe.MatchString(m.MatcherID) || hasControlChar(m.MatcherID) {
		return nil, fmt.Errorf("%w: matcher_id", ErrMetadataInvalid)
	}
	if !providerRe.MatchString(m.Provider) {
		return nil, fmt.Errorf("%w: provider", ErrMetadataInvalid)
	}
	if !sha256Re.MatchString(m.SHA256) {
		return nil, fmt.Errorf("%w: sha256", ErrMetadataInvalid)
	}
	if _, ok := mediaAllowList[m.MediaType]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrMetadataUnknownMediaType, m.MediaType)
	}

	// Spec 15 §4.2: enforce producer-asserted provenance for media types
	// in requiredProvenance. Other media types copy fields when present
	// (for round-trip fidelity) but do not require them.
	if requiredProvenance[m.MediaType] {
		if m.AcqProvider == "" {
			return nil, fmt.Errorf("%w: acq_provider", ErrMetadataMissing)
		}
		if !providerRe.MatchString(m.AcqProvider) {
			return nil, fmt.Errorf("%w: acq_provider", ErrMetadataInvalid)
		}
		if m.AcqAccountID == "" {
			return nil, fmt.Errorf("%w: acq_account_id", ErrMetadataMissing)
		}
		if len(m.AcqAccountID) > provenanceStringMaxBytes || hasControlChar(m.AcqAccountID) {
			return nil, fmt.Errorf("%w: acq_account_id", ErrMetadataInvalid)
		}
		if m.AcqAuthMethod == "" {
			return nil, fmt.Errorf("%w: acq_auth_method", ErrMetadataMissing)
		}
		if m.AcqAuthMethod != "browser_session" {
			return nil, fmt.Errorf("%w: acq_auth_method", ErrMetadataInvalid)
		}
		if m.DataSubject == "" {
			return nil, fmt.Errorf("%w: data_subject", ErrMetadataMissing)
		}
		if len(m.DataSubject) > provenanceStringMaxBytes || hasControlChar(m.DataSubject) {
			return nil, fmt.Errorf("%w: data_subject", ErrMetadataInvalid)
		}
	}

	// Parse optional audience. Split on comma; nil when absent or empty.
	// ValidateAudience enforces spec 11 §3.5 token rules.
	if audRaw, hasAud := raw["audience"]; hasAud && audRaw != "" {
		tokens := strings.Split(audRaw, ",")
		hasDataSubject := m.DataSubject != ""
		if err := staging.ValidateAudience(tokens, hasDataSubject); err != nil {
			return nil, fmt.Errorf("%w: audience: %s", ErrMetadataInvalid, err.Error())
		}
		m.Audience = tokens
	}

	return m, nil
}

// Shape returns the media shape for the parsed metadata. Safe to call
// only after a successful ParseUploadMetadata; on a Metadata whose
// MediaType is not in the allow-list it returns the zero MediaShape.
func (m *Metadata) Shape() MediaShape { return mediaAllowList[m.MediaType] }

// hasControlChar reports whether s contains any C0 control byte other
// than '\t'. '\n', '\r', NUL, and other sub-0x20 bytes are rejected;
// horizontal tab is permitted because some legitimate provider strings
// historically include it (we still reject for matcher_id etc., where
// the regex already excludes tab). DEL (0x7F) is NOT a C0 control and
// is not rejected here -- the regex allow-lists already exclude it for
// every field that uses this helper.
func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 && s[i] != '\t' {
			return true
		}
	}
	return false
}

// decodeMetadataPairs parses a tus.io Upload-Metadata header into a
// key->decoded-value map. The header is `key1 value1,key2 value2,...`
// where each value is base64-encoded. Duplicate keys: the last value
// wins (matches tus.io reference implementations); the caller's
// reserved-key check still fires if either copy carries a reserved
// name.
func decodeMetadataPairs(header string) (map[string]string, error) {
	out := make(map[string]string)
	for _, kv := range strings.Split(header, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		sp := strings.IndexByte(kv, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("%w: pair missing space", ErrMetadataInvalid)
		}
		key := kv[:sp]
		val := kv[sp+1:]
		if key == "" {
			return nil, fmt.Errorf("%w: empty key", ErrMetadataInvalid)
		}
		decoded, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			return nil, fmt.Errorf("%w: %s base64", ErrMetadataInvalid, key)
		}
		out[key] = string(decoded)
	}
	return out, nil
}
