package archives

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// enc is the per-test base64 encoder for Upload-Metadata values per
// tus.io. Standard base64 (with padding) is the tus.io reference.
func enc(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// validHeader returns an Upload-Metadata header with every required
// field populated by spec §4.2-compliant placeholder values. Tests
// mutate one field at a time to exercise rejection paths.
func validHeader(overrides map[string]string) string {
	base := map[string]string{
		"archive_id":            "abc123-takeout-001",
		"archive_filename":      "mail.mbox",
		"subtree_relative_path": ".",
		"media_type":            "archive/mbox",
		"matcher_id":            "google-takeout/mail",
		"provider":              "google",
		"sha256":                "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"size_bytes":            "1024",
	}
	for k, v := range overrides {
		if v == "" {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	// Deterministic order so any failure prints a stable header.
	keys := []string{
		"archive_id", "archive_filename", "subtree_relative_path",
		"media_type", "matcher_id", "provider", "sha256", "size_bytes",
	}
	parts := make([]string, 0, len(base))
	for _, k := range keys {
		v, ok := base[k]
		if !ok {
			continue
		}
		parts = append(parts, k+" "+enc(v))
	}
	// Append any keys not in the canonical order list (used by reserved-
	// key tests that add delivered_by / delivered_at).
	for k, v := range base {
		found := false
		for _, kk := range keys {
			if kk == k {
				found = true
				break
			}
		}
		if !found {
			parts = append(parts, k+" "+enc(v))
		}
	}
	return strings.Join(parts, ",")
}

// ---- happy path ----

func TestParseUploadMetadata_HappyPath(t *testing.T) {
	header := validHeader(nil)
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ArchiveID != "abc123-takeout-001" {
		t.Errorf("ArchiveID=%q want %q", m.ArchiveID, "abc123-takeout-001")
	}
	if m.ArchiveFilename != "mail.mbox" {
		t.Errorf("ArchiveFilename=%q", m.ArchiveFilename)
	}
	if m.SubtreeRelativePath != "." {
		t.Errorf("SubtreeRelativePath=%q", m.SubtreeRelativePath)
	}
	if m.MediaType != "archive/mbox" {
		t.Errorf("MediaType=%q", m.MediaType)
	}
	if m.MatcherID != "google-takeout/mail" {
		t.Errorf("MatcherID=%q", m.MatcherID)
	}
	if m.Provider != "google" {
		t.Errorf("Provider=%q", m.Provider)
	}
	if m.SHA256 != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("SHA256=%q", m.SHA256)
	}
	if m.SizeBytes != 1024 {
		t.Errorf("SizeBytes=%d", m.SizeBytes)
	}
	if got := m.Shape(); got != MediaRaw {
		t.Errorf("Shape()=%q want %q", got, MediaRaw)
	}
}

func TestParseUploadMetadata_HappyPath_TarShape(t *testing.T) {
	header := validHeader(map[string]string{
		"media_type":            "archive/google-takeout-subtree",
		"subtree_relative_path": "Takeout/Mail",
		"archive_filename":      "takeout.tar",
	})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Shape() != MediaTar {
		t.Errorf("Shape()=%q want %q", m.Shape(), MediaTar)
	}
}

// Recognizer-shipping media types added 2026-05-23 (glovebox-4enb).
// Both share the existing MediaRaw / MediaTar finalize dispatch; no
// new untar / rename code path is required. A future per-type
// importer (downstream watcher) may further specialize handling.

func TestParseUploadMetadata_HappyPath_GenericTarball(t *testing.T) {
	header := validHeader(map[string]string{
		"media_type":            "archive/generic-tarball",
		"subtree_relative_path": "meta-facebook",
		"archive_filename":      "fb-export.tar",
	})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Shape() != MediaTar {
		t.Errorf("Shape()=%q want %q", m.Shape(), MediaTar)
	}
}

// archive/recognizer-scan (glovebox-9s60) is a tar shape; finalize gates it
// to the authenticated recognizer-scanner source and produces
// content.extracted.md. Parsing only needs to accept the media type.
func TestParseUploadMetadata_HappyPath_RecognizerScan(t *testing.T) {
	header := validHeader(map[string]string{
		"media_type":       "archive/recognizer-scan",
		"archive_filename": "scan-0001.tar",
	})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Shape() != MediaTar {
		t.Errorf("Shape()=%q want %q", m.Shape(), MediaTar)
	}
}

func TestParseUploadMetadata_HappyPath_ImapExport(t *testing.T) {
	header := validHeader(map[string]string{
		"media_type":       "archive/imap-export",
		"archive_filename": "inbox.mbox",
	})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Shape() != MediaRaw {
		t.Errorf("Shape()=%q want %q", m.Shape(), MediaRaw)
	}
}

// ---- archive_id rejection cases ----

func TestParseUploadMetadata_RejectsArchiveIDWithNewline(t *testing.T) {
	header := validHeader(map[string]string{"archive_id": "abc\nxyz"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsArchiveIDWithNUL(t *testing.T) {
	header := validHeader(map[string]string{"archive_id": "abc\x00xyz"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_AcceptsArchiveIDWithUppercase(t *testing.T) {
	// archive_id regex is ^[a-zA-Z0-9._-]{1,128}$ -- uppercase IS allowed.
	// The plan's test list says "rejects archive_id with newline/NUL/uppercase"
	// but §4.2's regex explicitly admits A-Z. We follow the spec regex,
	// which is the source of truth, and document the conflict here.
	header := validHeader(map[string]string{"archive_id": "ABC123"})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("uppercase rejected unexpectedly: %v", err)
	}
	if m.ArchiveID != "ABC123" {
		t.Errorf("ArchiveID=%q", m.ArchiveID)
	}
}

func TestParseUploadMetadata_RejectsArchiveIDTooLong(t *testing.T) {
	header := validHeader(map[string]string{"archive_id": strings.Repeat("a", 129)})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsArchiveIDEmpty(t *testing.T) {
	header := validHeader(map[string]string{"archive_id": " "}) // post-trim empty stays "" via decode
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- archive_filename rejection cases ----

func TestParseUploadMetadata_RejectsArchiveFilenameSlash(t *testing.T) {
	header := validHeader(map[string]string{"archive_filename": "sub/mail.mbox"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsArchiveFilenameDotDot(t *testing.T) {
	header := validHeader(map[string]string{"archive_filename": "mail..mbox"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsArchiveFilenameNUL(t *testing.T) {
	header := validHeader(map[string]string{"archive_filename": "mail\x00.mbox"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsArchiveFilenameBackslash(t *testing.T) {
	header := validHeader(map[string]string{"archive_filename": "mail\\backup.mbox"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsArchiveFilenameTooLong(t *testing.T) {
	// 257 bytes is one over the 256-byte cap.
	header := validHeader(map[string]string{"archive_filename": strings.Repeat("a", 257)})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- media_type rejection ----

func TestParseUploadMetadata_RejectsUnknownMediaType(t *testing.T) {
	header := validHeader(map[string]string{"media_type": "archive/zip"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataUnknownMediaType) {
		t.Fatalf("err=%v, want ErrMetadataUnknownMediaType", err)
	}
}

func TestParseUploadMetadata_RejectsMediaTypeEmpty(t *testing.T) {
	header := validHeader(map[string]string{"media_type": " "}) // decodes to ""
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataUnknownMediaType) {
		t.Fatalf("err=%v, want ErrMetadataUnknownMediaType", err)
	}
}

// ---- size_bytes rejection ----

func TestParseUploadMetadata_RejectsSizeBytesMismatch(t *testing.T) {
	header := validHeader(map[string]string{"size_bytes": "2048"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataSizeMismatch) {
		t.Fatalf("err=%v, want ErrMetadataSizeMismatch", err)
	}
}

func TestParseUploadMetadata_RejectsSizeBytesNonNumeric(t *testing.T) {
	header := validHeader(map[string]string{"size_bytes": "abc"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsSizeBytesNegative(t *testing.T) {
	header := validHeader(map[string]string{"size_bytes": "-1"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- sha256 rejection ----

func TestParseUploadMetadata_RejectsSHA256Uppercase(t *testing.T) {
	header := validHeader(map[string]string{
		"sha256": "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
	})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsSHA256TooShort(t *testing.T) {
	header := validHeader(map[string]string{
		"sha256": "0123456789abcdef",
	})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsSHA256NonHex(t *testing.T) {
	header := validHeader(map[string]string{
		"sha256": "g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- matcher_id rejection ----

func TestParseUploadMetadata_RejectsMatcherIDWithControlChar(t *testing.T) {
	header := validHeader(map[string]string{"matcher_id": "google-takeout\nmail"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsMatcherIDTooLong(t *testing.T) {
	header := validHeader(map[string]string{"matcher_id": strings.Repeat("a", 257)})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- provider rejection ----

func TestParseUploadMetadata_RejectsProviderUppercase(t *testing.T) {
	header := validHeader(map[string]string{"provider": "Google"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsProviderLeadingDigit(t *testing.T) {
	header := validHeader(map[string]string{"provider": "1google"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- subtree_relative_path rejection ----

func TestParseUploadMetadata_RejectsSubtreeRelativePathNUL(t *testing.T) {
	header := validHeader(map[string]string{"subtree_relative_path": "Takeout/\x00Mail"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsSubtreeRelativePathControlChar(t *testing.T) {
	header := validHeader(map[string]string{"subtree_relative_path": "Takeout/\nMail"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsSubtreeRelativePathTooLong(t *testing.T) {
	header := validHeader(map[string]string{"subtree_relative_path": strings.Repeat("a", 1025)})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsSubtreeRelativePathInvalidUTF8(t *testing.T) {
	header := validHeader(map[string]string{"subtree_relative_path": "Take\xffout"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- reserved-key rejection (spec §3.3) ----

func TestParseUploadMetadata_RejectsDeliveredBy(t *testing.T) {
	header := validHeader(map[string]string{"delivered_by": "recognizer"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataReservedKey) {
		t.Fatalf("err=%v, want ErrMetadataReservedKey", err)
	}
}

func TestParseUploadMetadata_RejectsDeliveredAt(t *testing.T) {
	header := validHeader(map[string]string{"delivered_at": "2026-05-21T19:32:14Z"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataReservedKey) {
		t.Fatalf("err=%v, want ErrMetadataReservedKey", err)
	}
}

// ---- missing required keys ----

func TestParseUploadMetadata_RejectsMissingSizeBytes(t *testing.T) {
	header := validHeader(map[string]string{"size_bytes": ""}) // drop it
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataMissing) {
		t.Fatalf("err=%v, want ErrMetadataMissing", err)
	}
}

func TestParseUploadMetadata_RejectsMissingArchiveID(t *testing.T) {
	header := validHeader(map[string]string{"archive_id": ""}) // drop it
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsMissingMediaType(t *testing.T) {
	header := validHeader(map[string]string{"media_type": ""}) // drop it
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataUnknownMediaType) {
		t.Fatalf("err=%v, want ErrMetadataUnknownMediaType", err)
	}
}

// ---- header length cap (§4.2) ----

func TestParseUploadMetadata_RejectsHeaderTooLong(t *testing.T) {
	// One pair, base64 value padded to push the header past 4 KiB.
	big := strings.Repeat("x", 5000)
	header := "archive_id " + enc(big)
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataTooLong) {
		t.Fatalf("err=%v, want ErrMetadataTooLong", err)
	}
}

func TestParseUploadMetadata_HeaderLengthCapRunsBeforeDecode(t *testing.T) {
	// Even an otherwise well-formed header is rejected if its bytes
	// exceed the cap. We deliberately make the value undecodable to
	// confirm the length check fires FIRST.
	header := "archive_id " + strings.Repeat("@", 5000)
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataTooLong) {
		t.Fatalf("err=%v, want ErrMetadataTooLong (length cap must beat base64-decode)", err)
	}
}

// ---- malformed pair structure ----

func TestParseUploadMetadata_RejectsPairMissingSpace(t *testing.T) {
	header := "archive_idabc," + "size_bytes " + enc("1024")
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_RejectsBase64Garbage(t *testing.T) {
	header := "archive_id !@#$,size_bytes " + enc("1024")
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- validation order ----

// TestParseUploadMetadata_ReservedKeyBeatsFieldValidation confirms that
// a header carrying delivered_by is rejected as a reserved-key error
// EVEN IF other fields are invalid. This locks in the spec §3.3
// "reserved keys reject at parse" ordering: the reserved-key check
// fires before per-field regex checks.
func TestParseUploadMetadata_ReservedKeyBeatsFieldValidation(t *testing.T) {
	header := validHeader(map[string]string{
		"delivered_by": "recognizer",
		"archive_id":   "INVALID\nNEWLINE",
	})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataReservedKey) {
		t.Fatalf("err=%v, want ErrMetadataReservedKey (reserved-key check must run before per-field checks)", err)
	}
}

// TestParseUploadMetadata_SizeMismatchBeatsRegexChecks confirms that
// size_bytes vs Upload-Length comparison fires BEFORE per-field regex
// validation. Per the plan's validation-order spec, the order is:
// max-length -> decode -> reserved-key -> size_bytes parse + compare ->
// per-field regex checks.
func TestParseUploadMetadata_SizeMismatchBeatsRegexChecks(t *testing.T) {
	header := validHeader(map[string]string{
		"size_bytes": "2048",
		"archive_id": "INVALID\nNEWLINE",
	})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataSizeMismatch) {
		t.Fatalf("err=%v, want ErrMetadataSizeMismatch (size check must beat regex check)", err)
	}
}

// ---- archive/walhelm-export happy path (spec 15 sec 4.2) ----

// walhelmHeader builds a valid walhelm-export header. The base validHeader
// fixture uses archive/mbox; this helper overrides media_type and injects
// all required provenance keys. Extra overrides are merged on top.
func walhelmHeader(overrides map[string]string) string {
	base := map[string]string{
		"media_type":       "archive/walhelm-export",
		"archive_filename": "health.tar",
		"acq_provider":     "apple-health",
		"acq_account_id":   "user@example.com",
		"acq_auth_method":  "browser_session",
		"data_subject":     "subject-opaque-id-001",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return validHeader(base)
}

func TestParseUploadMetadata_WalhelmExport_HappyPath(t *testing.T) {
	header := walhelmHeader(nil)
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.MediaType != "archive/walhelm-export" {
		t.Errorf("MediaType=%q", m.MediaType)
	}
	if m.AcqProvider != "apple-health" {
		t.Errorf("AcqProvider=%q want %q", m.AcqProvider, "apple-health")
	}
	if m.AcqAccountID != "user@example.com" {
		t.Errorf("AcqAccountID=%q want %q", m.AcqAccountID, "user@example.com")
	}
	if m.AcqAuthMethod != "browser_session" {
		t.Errorf("AcqAuthMethod=%q want %q", m.AcqAuthMethod, "browser_session")
	}
	if m.DataSubject != "subject-opaque-id-001" {
		t.Errorf("DataSubject=%q want %q", m.DataSubject, "subject-opaque-id-001")
	}
	if m.Audience != nil {
		t.Errorf("Audience=%v want nil (not set)", m.Audience)
	}
	if m.Shape() != MediaTar {
		t.Errorf("Shape()=%q want %q", m.Shape(), MediaTar)
	}
}

func TestParseUploadMetadata_WalhelmExport_HappyPath_WithAudience(t *testing.T) {
	// "subject,guardians" is valid: both are household-subset tokens;
	// neither is "household" itself, so the subset-exclusion rule does not fire.
	// data_subject is set (required for walhelm), satisfying the
	// subject-relative rule for "subject".
	header := walhelmHeader(map[string]string{"audience": "subject,guardians"})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Audience) != 2 || m.Audience[0] != "subject" || m.Audience[1] != "guardians" {
		t.Errorf("Audience=%v want [subject guardians]", m.Audience)
	}
}

// ---- archive/walhelm-export missing required provenance keys ----

func TestParseUploadMetadata_WalhelmExport_MissingAcqProvider(t *testing.T) {
	header := walhelmHeader(map[string]string{"acq_provider": ""})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataMissing) {
		t.Fatalf("err=%v, want ErrMetadataMissing", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_MissingAcqAccountID(t *testing.T) {
	header := walhelmHeader(map[string]string{"acq_account_id": ""})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataMissing) {
		t.Fatalf("err=%v, want ErrMetadataMissing", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_MissingAcqAuthMethod(t *testing.T) {
	header := walhelmHeader(map[string]string{"acq_auth_method": ""})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataMissing) {
		t.Fatalf("err=%v, want ErrMetadataMissing", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_MissingDataSubject(t *testing.T) {
	header := walhelmHeader(map[string]string{"data_subject": ""})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataMissing) {
		t.Fatalf("err=%v, want ErrMetadataMissing", err)
	}
}

// ---- archive/walhelm-export invalid provenance field values ----

func TestParseUploadMetadata_WalhelmExport_InvalidAcqProvider(t *testing.T) {
	// acq_provider must match providerRe: ^[a-z][a-z0-9-]{0,63}$
	header := walhelmHeader(map[string]string{"acq_provider": "Apple-Health"}) // uppercase
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_InvalidAcqAuthMethod(t *testing.T) {
	// only "browser_session" is valid for v1
	header := walhelmHeader(map[string]string{"acq_auth_method": "oauth2"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_AcqAccountIDTooLong(t *testing.T) {
	// provenanceStringMaxBytes is 256; 257 bytes must be rejected.
	header := walhelmHeader(map[string]string{"acq_account_id": strings.Repeat("a", 257)})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_AcqAccountIDControlChar(t *testing.T) {
	// acq_account_id containing a C0 control character must be rejected.
	header := walhelmHeader(map[string]string{"acq_account_id": "user\x01example.com"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_DataSubjectTooLong(t *testing.T) {
	// provenanceStringMaxBytes is 256; 257 bytes must be rejected.
	header := walhelmHeader(map[string]string{"data_subject": strings.Repeat("a", 257)})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_DataSubjectControlChar(t *testing.T) {
	// data_subject containing a C0 control character must be rejected.
	header := walhelmHeader(map[string]string{"data_subject": "subject\x01opaque-id"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

func TestParseUploadMetadata_WalhelmExport_InvalidAudience(t *testing.T) {
	// "unknown-token" is not a valid audience token
	header := walhelmHeader(map[string]string{"audience": "unknown-token"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v, want ErrMetadataInvalid", err)
	}
}

// ---- backward compatibility: mbox unaffected by provenance enforcement ----

func TestParseUploadMetadata_MboxUnaffectedByProvenanceFields(t *testing.T) {
	// archive/mbox with NO acq_* / data_subject must still parse (additive).
	header := validHeader(nil)
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("mbox parse: %v", err)
	}
	if m.AcqProvider != "" || m.AcqAccountID != "" || m.AcqAuthMethod != "" || m.DataSubject != "" {
		t.Errorf("unexpected provenance fields on mbox: provider=%q account=%q auth=%q subject=%q",
			m.AcqProvider, m.AcqAccountID, m.AcqAuthMethod, m.DataSubject)
	}
}

func TestParseUploadMetadata_MboxWithStrayAcqProviderIsIgnored(t *testing.T) {
	// A stray acq_provider on archive/mbox must NOT cause an error.
	header := validHeader(map[string]string{"acq_provider": "extra-provider"})
	m, err := ParseUploadMetadata(header, 1024)
	if err != nil {
		t.Fatalf("mbox with stray acq_provider should parse: %v", err)
	}
	// The value is copied into the struct (round-trip) but not enforced.
	if m.AcqProvider != "extra-provider" {
		t.Errorf("AcqProvider=%q want %q (stray key should round-trip)", m.AcqProvider, "extra-provider")
	}
}

// ---- regression: delivered_by still rejected ----

func TestParseUploadMetadata_DeliveredByStillRejected_Regression(t *testing.T) {
	header := walhelmHeader(map[string]string{"delivered_by": "recognizer"})
	_, err := ParseUploadMetadata(header, 1024)
	if !errors.Is(err, ErrMetadataReservedKey) {
		t.Fatalf("err=%v, want ErrMetadataReservedKey", err)
	}
}

// ---- hasControlChar unit coverage ----

func TestHasControlChar(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc", false},
		{"abc\tdef", false}, // tab is allowed
		{"abc\ndef", true},  // newline is rejected
		{"abc\rdef", true},  // CR is rejected
		{"abc\x00def", true},
		{"abc\x1fdef", true}, // last C0 (US) is rejected
		{"abc\x20def", false},
		{"abc\x7fdef", false}, // DEL is NOT a C0 control; helper does not reject
		{"", false},
	}
	for _, c := range cases {
		if got := hasControlChar(c.in); got != c.want {
			t.Errorf("hasControlChar(%q)=%v, want %v", c.in, got, c.want)
		}
	}
}
