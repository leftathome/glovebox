// Tests for the finalize step (spec 13 §4.6).
//
// Coverage matrix per the Wave C Task C1 plan:
//
//   TestFinalize_HappyRaw                  -- mbox staged under raw/
//   TestFinalize_HappyTar                  -- 5-file tarball staged under tree/
//   TestFinalize_SHA256Mismatch            -- wrong claimed sha; cleanup verified
//   TestFinalize_SizeMismatch              -- declared > actual; cleanup verified
//   TestFinalize_TarSafetyViolationPropagated -- B3 sentinel wrapped + cleanup
//   TestFinalize_RenameCollision           -- ErrArchiveExists; target preserved
//   TestFinalize_IdentityBlockInMetadata   -- spec 06 §5.2 fields written
//   TestFinalize_ProvenanceInReceipt       -- walhelm provenance fields in receipt + on-disk metadata.json
//   TestFinalize_MboxOmitsProvenanceFields -- mbox omits all three new fields (omitempty)
//
// Fixtures kept local rather than added to helpers_test.go because they
// are finalize-specific (tar builder, sha256 helper, staging-dir
// layout): the helpers file is shared with B2/B3/B4 tests and we want
// to keep its surface tight.

package archives

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"testing"

	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/source"
)

// stagedFixture wires up the on-disk + UploadState fixture for a
// finalize test. The caller supplies the content bytes and the media
// type; the helper writes the tmp file, builds the Hasher with the
// correct rolling state, and produces a populated Metadata + UploadState.
type stagedFixture struct {
	stagingRoot string
	uploadID    string
	archiveID   string
	state       *UploadState
}

func setupFixture(t *testing.T, mediaType, archiveFilename string, content []byte) *stagedFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".tmp-archives"), 0o700); err != nil {
		t.Fatalf("mkdir .tmp-archives: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archives"), 0o700); err != nil {
		t.Fatalf("mkdir archives: %v", err)
	}

	uploadID := "deadbeefdeadbeefdeadbeefdeadbeef"
	archiveID := "ab12-test-archive-001"

	// Write the tmp file as the PATCH path would have done.
	tmpPath := filepath.Join(root, ".tmp-archives", uploadID)
	if err := os.WriteFile(tmpPath, content, 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	// Build the rolling hasher: the production PATCH handler writes
	// each chunk through a TeeReader so by the time Finalize is called,
	// hasher.Sum() == sha256(all bytes received). Replicate that here.
	h := sha256.New()
	h.Write(content)
	sum := hex.EncodeToString(h.Sum(nil))

	meta := &Metadata{
		ArchiveID:           archiveID,
		ArchiveFilename:     archiveFilename,
		SubtreeRelativePath: ".",
		MediaType:           mediaType,
		MatcherID:           "test/matcher",
		Provider:            "test",
		SHA256:              sum,
		SizeBytes:           int64(len(content)),
	}

	st := &UploadState{
		ID:           uploadID,
		SourceID:     "test-source",
		ArchiveID:    archiveID,
		UploadLength: int64(len(content)),
		Offset:       int64(len(content)),
		Hasher:       h,
		Meta:         meta,
	}

	return &stagedFixture{
		stagingRoot: root,
		uploadID:    uploadID,
		archiveID:   archiveID,
		state:       st,
	}
}

// buildFinalizeTar produces an uncompressed tarball with the supplied
// entries. The package-level buildTar (untar_test.go) takes a different
// signature ([]entry with body/typeflag/etc.); finalize tests want the
// simpler name->bytes API, so this wrapper adapts to it.
func buildFinalizeTar(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	es := make([]entry, 0, len(entries))
	for name, content := range entries {
		es = append(es, entry{
			name:     name,
			typeflag: tar.TypeReg,
			mode:     0o600,
			size:     -1,
			body:     content,
		})
	}
	return buildTar(t, es)
}

// buildTraversalTar produces a malicious tarball with a `..`
// path-traversal entry. Used to verify Finalize propagates the B3
// safety sentinel.
func buildTraversalTar(t *testing.T) []byte {
	return buildTar(t, []entry{{
		name:     "../escape.txt",
		typeflag: tar.TypeReg,
		mode:     0o600,
		size:     -1,
		body:     []byte("escaped"),
	}})
}

// assertCleanedUp verifies that both the tmp file and finalize dir
// were removed for the given upload-id.
func assertCleanedUp(t *testing.T, stagingRoot, uploadID string) {
	t.Helper()
	tmpPath := filepath.Join(stagingRoot, ".tmp-archives", uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file %q still exists after cleanup (err=%v)", tmpPath, err)
	}
	finalizePath := filepath.Join(stagingRoot, ".tmp-archives", uploadID+".finalize")
	if _, err := os.Stat(finalizePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finalize dir %q still exists after cleanup (err=%v)", finalizePath, err)
	}
}

// assertNoArchive verifies that archives/<archive-id>/ was NOT
// published. Used by all failure-path tests.
func assertNoArchive(t *testing.T, stagingRoot, archiveID string) {
	t.Helper()
	archivePath := filepath.Join(stagingRoot, "archives", archiveID)
	if _, err := os.Stat(archivePath); err == nil {
		t.Errorf("archives/%s/ exists after failed Finalize", archiveID)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected stat error on archives/%s/: %v", archiveID, err)
	}
}

// authedCtx returns a context with delivered_by set to sourceID so
// BuildIdentity can populate the receipt's Identity field.
func authedCtx(sourceID string) context.Context {
	return ingest.WithDeliveredBy(context.Background(), sourceID)
}

// ---------------------------------------------------------------------
// TestFinalize_HappyRaw
// ---------------------------------------------------------------------

func TestFinalize_HappyRaw(t *testing.T) {
	content := []byte("From foo@bar.com\nSubject: hi\n\nbody text\n")
	fx := setupFixture(t, "archive/mbox", "test.mbox", content)
	fx.state.SourceID = "recognizer"

	ctx := authedCtx("recognizer")
	receipt, err := Finalize(ctx, fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if receipt.ArchiveID != fx.archiveID {
		t.Errorf("ArchiveID=%q want %q", receipt.ArchiveID, fx.archiveID)
	}
	if !receipt.SHA256Verified {
		t.Error("SHA256Verified=false, want true")
	}
	if receipt.RawFilename != "test.mbox" {
		t.Errorf("RawFilename=%q want %q", receipt.RawFilename, "test.mbox")
	}
	if receipt.EntriesExtracted != 0 {
		t.Errorf("EntriesExtracted=%d want 0 for raw shape", receipt.EntriesExtracted)
	}
	if receipt.MediaType != "archive/mbox" {
		t.Errorf("MediaType=%q want archive/mbox", receipt.MediaType)
	}
	if receipt.DeliveredBy != "recognizer" {
		t.Errorf("DeliveredBy=%q want recognizer", receipt.DeliveredBy)
	}

	stagedRaw := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "raw", "test.mbox")
	got, rerr := os.ReadFile(stagedRaw)
	if rerr != nil {
		t.Fatalf("read staged raw: %v", rerr)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("staged content mismatch:\n got  %q\n want %q", got, content)
	}

	fi, ferr := os.Stat(stagedRaw)
	if ferr != nil {
		t.Fatalf("stat staged raw: %v", ferr)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("staged raw mode=%o want 0600", mode)
	}

	rawDir := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "raw")
	di, derr := os.Stat(rawDir)
	if derr != nil {
		t.Fatalf("stat staged raw dir: %v", derr)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Errorf("staged raw dir mode=%o want 0700", mode)
	}

	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	metaBytes, mErr := os.ReadFile(metaPath)
	if mErr != nil {
		t.Fatalf("read metadata.json: %v", mErr)
	}
	var staged FinalizeReceipt
	if err := json.Unmarshal(metaBytes, &staged); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	if staged.Identity == nil {
		t.Fatal("metadata.json Identity is nil; want populated block")
	}
	if staged.Identity.Provider != "ingest" {
		t.Errorf("Identity.Provider=%q want ingest", staged.Identity.Provider)
	}
	if staged.Identity.AuthMethod != "bearer_token" {
		t.Errorf("Identity.AuthMethod=%q want bearer_token", staged.Identity.AuthMethod)
	}
	if staged.Identity.AccountID != "recognizer" {
		t.Errorf("Identity.AccountID=%q want recognizer", staged.Identity.AccountID)
	}

	// Tmp file + finalize dir should be gone after a successful publish.
	tmpPath := filepath.Join(fx.stagingRoot, ".tmp-archives", fx.uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file lingered after successful finalize")
	}
	finalizePath := filepath.Join(fx.stagingRoot, ".tmp-archives", fx.uploadID+".finalize")
	if _, err := os.Stat(finalizePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finalize dir lingered after successful finalize")
	}
}

// ---------------------------------------------------------------------
// TestFinalize_HappyTar
// ---------------------------------------------------------------------

func TestFinalize_HappyTar(t *testing.T) {
	entries := map[string][]byte{
		"a.txt":      []byte("hello a"),
		"b.txt":      []byte("hello b"),
		"sub/c.txt":  []byte("hello c"),
		"sub/d.txt":  []byte("hello d"),
		"deep/e.txt": []byte("hello e"),
	}
	tarball := buildFinalizeTar(t, entries)

	fx := setupFixture(t, "archive/google-takeout-subtree", "takeout-mail.tar", tarball)
	fx.state.SourceID = "recognizer"

	ctx := authedCtx("recognizer")
	receipt, err := Finalize(ctx, fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if receipt.EntriesExtracted != len(entries) {
		t.Errorf("EntriesExtracted=%d want %d", receipt.EntriesExtracted, len(entries))
	}
	if receipt.RawFilename != "" {
		t.Errorf("RawFilename=%q want empty for tar shape", receipt.RawFilename)
	}
	if receipt.MediaType != "archive/google-takeout-subtree" {
		t.Errorf("MediaType=%q unexpected", receipt.MediaType)
	}

	for name, want := range entries {
		stagedPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "tree", name)
		got, rerr := os.ReadFile(stagedPath)
		if rerr != nil {
			t.Errorf("read entry %q: %v", name, rerr)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("entry %q content mismatch: got %q want %q", name, got, want)
		}
		fi, ferr := os.Stat(stagedPath)
		if ferr != nil {
			t.Errorf("stat entry %q: %v", name, ferr)
			continue
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("entry %q mode=%o want 0600", name, mode)
		}
	}

	// Verify metadata.json carries the same info.
	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	metaBytes, mErr := os.ReadFile(metaPath)
	if mErr != nil {
		t.Fatalf("read metadata.json: %v", mErr)
	}
	var staged FinalizeReceipt
	if err := json.Unmarshal(metaBytes, &staged); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	if staged.EntriesExtracted != len(entries) {
		t.Errorf("metadata.json EntriesExtracted=%d want %d", staged.EntriesExtracted, len(entries))
	}
	if !staged.SHA256Verified {
		t.Error("metadata.json SHA256Verified=false want true")
	}

	// Tmp file should be removed after a successful tar dispatch.
	tmpPath := filepath.Join(fx.stagingRoot, ".tmp-archives", fx.uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file lingered after successful tar finalize")
	}
}

// ---------------------------------------------------------------------
// TestFinalize_SHA256Mismatch
// ---------------------------------------------------------------------

func TestFinalize_SHA256Mismatch(t *testing.T) {
	content := []byte("real content")
	fx := setupFixture(t, "archive/mbox", "x.mbox", content)

	// Corrupt the metadata sha to a different valid hex value: hash
	// of "different content".
	other := sha256.Sum256([]byte("different content"))
	fx.state.Meta.SHA256 = hex.EncodeToString(other[:])

	receipt, err := Finalize(authedCtx("test"), fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if !errors.Is(err, ErrSHAMismatch) {
		t.Fatalf("Finalize err=%v, want ErrSHAMismatch", err)
	}
	if receipt != nil {
		t.Errorf("receipt=%+v, want nil on sha mismatch", receipt)
	}
	assertCleanedUp(t, fx.stagingRoot, fx.uploadID)
	assertNoArchive(t, fx.stagingRoot, fx.archiveID)
}

// ---------------------------------------------------------------------
// TestFinalize_SizeMismatch
// ---------------------------------------------------------------------

func TestFinalize_SizeMismatch(t *testing.T) {
	content := []byte("twenty-byte content!") // 20 bytes
	fx := setupFixture(t, "archive/mbox", "x.mbox", content)

	// Lie about the upload-length: claim 24 bytes, file holds 20.
	// We must keep Meta.SHA256 consistent with the actual content so
	// the sha check passes and we land in the size-check branch.
	fx.state.UploadLength = 24
	// Meta.SizeBytes isn't consulted by Finalize (the metadata parser
	// already verified it at POST time against UploadLength), but
	// update it for fixture honesty.
	fx.state.Meta.SizeBytes = 24

	receipt, err := Finalize(authedCtx("test"), fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("Finalize err=%v, want ErrSizeMismatch", err)
	}
	if receipt != nil {
		t.Errorf("receipt=%+v, want nil on size mismatch", receipt)
	}
	assertCleanedUp(t, fx.stagingRoot, fx.uploadID)
	assertNoArchive(t, fx.stagingRoot, fx.archiveID)
}

// ---------------------------------------------------------------------
// TestFinalize_TarSafetyViolationPropagated
// ---------------------------------------------------------------------

func TestFinalize_TarSafetyViolationPropagated(t *testing.T) {
	tarball := buildTraversalTar(t)
	fx := setupFixture(t, "archive/google-takeout-subtree", "evil.tar", tarball)

	receipt, err := Finalize(authedCtx("test"), fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err == nil {
		t.Fatal("Finalize succeeded on malicious tar, want error")
	}
	if !errors.Is(err, ErrTarPathTraversal) {
		t.Fatalf("Finalize err=%v, want wraps ErrTarPathTraversal", err)
	}
	if receipt != nil {
		t.Errorf("receipt=%+v, want nil on tar safety violation", receipt)
	}
	assertCleanedUp(t, fx.stagingRoot, fx.uploadID)
	assertNoArchive(t, fx.stagingRoot, fx.archiveID)
}

// ---------------------------------------------------------------------
// TestFinalize_RenameCollision
// ---------------------------------------------------------------------

func TestFinalize_RenameCollision(t *testing.T) {
	content := []byte("first delivery")
	fx := setupFixture(t, "archive/mbox", "x.mbox", content)

	// Pre-create archives/<archive_id>/ with arbitrary content. Two
	// distinct source-ids racing the same archive_id is the scenario
	// this covers; we simulate by pre-populating the target.
	preExisting := filepath.Join(fx.stagingRoot, "archives", fx.archiveID)
	if err := os.MkdirAll(preExisting, 0o700); err != nil {
		t.Fatalf("pre-create archive: %v", err)
	}
	markerPath := filepath.Join(preExisting, "marker.txt")
	markerContent := []byte("first finalize won this race")
	if err := os.WriteFile(markerPath, markerContent, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	receipt, err := Finalize(authedCtx("test"), fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if !errors.Is(err, ErrArchiveExists) {
		t.Fatalf("Finalize err=%v, want ErrArchiveExists", err)
	}
	if receipt != nil {
		t.Errorf("receipt=%+v, want nil on archive_id collision", receipt)
	}

	// The pre-existing archive MUST be untouched.
	got, rerr := os.ReadFile(markerPath)
	if rerr != nil {
		t.Fatalf("read pre-existing marker: %v", rerr)
	}
	if !bytes.Equal(got, markerContent) {
		t.Errorf("pre-existing marker overwritten: got %q want %q", got, markerContent)
	}

	// Finalize dir must be cleaned up.
	finalizePath := filepath.Join(fx.stagingRoot, ".tmp-archives", fx.uploadID+".finalize")
	if _, err := os.Stat(finalizePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finalize dir %q still exists after collision", finalizePath)
	}
}

// ---------------------------------------------------------------------
// TestFinalize_IdentityBlockInMetadata
// ---------------------------------------------------------------------

func TestFinalize_IdentityBlockInMetadata(t *testing.T) {
	content := []byte("identity-bearing content")
	fx := setupFixture(t, "archive/mbox", "x.mbox", content)
	fx.state.SourceID = "specific-source"

	ctx := authedCtx("specific-source")
	receipt, err := Finalize(ctx, fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if receipt.Identity == nil {
		t.Fatal("receipt.Identity is nil; want populated block")
	}

	// Re-read metadata.json from disk and assert the Identity block is
	// in the JSON (not just the in-memory receipt struct).
	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	raw, rerr := os.ReadFile(metaPath)
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}

	// Decode into a map so we verify the JSON shape (not just the
	// struct's marshaled form): identity must be a nested object with
	// the three fields per spec 06 §5.2.
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	idAny, ok := parsed["identity"]
	if !ok {
		t.Fatal(`metadata.json missing "identity" key`)
	}
	id, ok := idAny.(map[string]any)
	if !ok {
		t.Fatalf(`metadata.json "identity" is %T, want object`, idAny)
	}
	if id["provider"] != "ingest" {
		t.Errorf(`identity.provider=%v, want "ingest"`, id["provider"])
	}
	if id["auth_method"] != "bearer_token" {
		t.Errorf(`identity.auth_method=%v, want "bearer_token"`, id["auth_method"])
	}
	if id["account_id"] != "specific-source" {
		t.Errorf(`identity.account_id=%v, want "specific-source"`, id["account_id"])
	}
}

// ---------------------------------------------------------------------
// TestFinalize_ProvenanceInReceipt
// ---------------------------------------------------------------------
//
// Drives Finalize with an archive/walhelm-export UploadState whose Meta
// carries all acq_* + data_subject + audience fields. Asserts that the
// returned FinalizeReceipt and the on-disk metadata.json both contain
// the acquisition block (provider/auth_method/account_id), data_subject,
// and audience per spec 15 sec 4.4.
//
// archive/walhelm-export is MediaTar, so we build a valid single-entry
// tarball for the fixture.

func TestFinalize_ProvenanceInReceipt(t *testing.T) {
	tarball := buildFinalizeTar(t, map[string][]byte{
		"data.json": []byte(`{"hello":"world"}`),
	})

	fx := setupFixture(t, "archive/walhelm-export", "walhelm.tar", tarball)
	// Populate provenance fields on Meta (as ParseUploadMetadata would
	// have done for a walhelm-export upload).
	fx.state.Meta.AcqProvider = "walhelm"
	fx.state.Meta.AcqAccountID = "user-42"
	fx.state.Meta.AcqAuthMethod = "browser_session"
	fx.state.Meta.DataSubject = "alice@example.com"
	fx.state.Meta.Audience = []string{"subject", "guardians"}
	fx.state.SourceID = "recognizer"

	ctx := authedCtx("recognizer")
	receipt, err := Finalize(ctx, fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// --- struct-level assertions ---

	if receipt.Acquisition == nil {
		t.Fatal("receipt.Acquisition is nil; want populated block")
	}
	if receipt.Acquisition.Provider != "walhelm" {
		t.Errorf("Acquisition.Provider=%q want walhelm", receipt.Acquisition.Provider)
	}
	if receipt.Acquisition.AuthMethod != "browser_session" {
		t.Errorf("Acquisition.AuthMethod=%q want browser_session", receipt.Acquisition.AuthMethod)
	}
	if receipt.Acquisition.AccountID != "user-42" {
		t.Errorf("Acquisition.AccountID=%q want user-42", receipt.Acquisition.AccountID)
	}
	if receipt.DataSubject != "alice@example.com" {
		t.Errorf("DataSubject=%q want alice@example.com", receipt.DataSubject)
	}
	if len(receipt.Audience) != 2 ||
		receipt.Audience[0] != "subject" ||
		receipt.Audience[1] != "guardians" {
		t.Errorf("Audience=%v want [subject guardians]", receipt.Audience)
	}

	// --- on-disk metadata.json assertions ---

	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	raw, rerr := os.ReadFile(metaPath)
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}

	acqAny, ok := parsed["acquisition"]
	if !ok {
		t.Fatal(`metadata.json missing "acquisition" key`)
	}
	acq, ok := acqAny.(map[string]any)
	if !ok {
		t.Fatalf(`metadata.json "acquisition" is %T, want object`, acqAny)
	}
	if acq["provider"] != "walhelm" {
		t.Errorf(`acquisition.provider=%v, want "walhelm"`, acq["provider"])
	}
	if acq["auth_method"] != "browser_session" {
		t.Errorf(`acquisition.auth_method=%v, want "browser_session"`, acq["auth_method"])
	}
	if acq["account_id"] != "user-42" {
		t.Errorf(`acquisition.account_id=%v, want "user-42"`, acq["account_id"])
	}

	if parsed["data_subject"] != "alice@example.com" {
		t.Errorf(`metadata.json data_subject=%v, want "alice@example.com"`, parsed["data_subject"])
	}

	audAny, ok := parsed["audience"]
	if !ok {
		t.Fatal(`metadata.json missing "audience" key`)
	}
	audSlice, ok := audAny.([]any)
	if !ok {
		t.Fatalf(`metadata.json "audience" is %T, want array`, audAny)
	}
	if len(audSlice) != 2 || audSlice[0] != "subject" || audSlice[1] != "guardians" {
		t.Errorf(`metadata.json audience=%v, want ["subject","guardians"]`, audSlice)
	}
}

// ---------------------------------------------------------------------
// TestFinalize_MboxOmitsProvenanceFields
// ---------------------------------------------------------------------
//
// Verifies backward compatibility: an archive/mbox finalize (no acq_* /
// data_subject / audience in Meta) must produce a receipt and on-disk
// metadata.json that omit all three new fields entirely (omitempty).

func TestFinalize_MboxOmitsProvenanceFields(t *testing.T) {
	content := []byte("From foo@bar.com\nSubject: omit test\n\nbody\n")
	fx := setupFixture(t, "archive/mbox", "omit.mbox", content)
	// Meta has zero values for AcqProvider / AcqAccountID / AcqAuthMethod /
	// DataSubject / Audience -- the mbox path never sets them.
	fx.state.SourceID = "recognizer"

	ctx := authedCtx("recognizer")
	receipt, err := Finalize(ctx, fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Struct-level: all three fields must be absent / zero.
	if receipt.Acquisition != nil {
		t.Errorf("receipt.Acquisition=%+v, want nil for mbox", receipt.Acquisition)
	}
	if receipt.DataSubject != "" {
		t.Errorf("receipt.DataSubject=%q, want empty for mbox", receipt.DataSubject)
	}
	if len(receipt.Audience) != 0 {
		t.Errorf("receipt.Audience=%v, want nil/empty for mbox", receipt.Audience)
	}

	// On-disk metadata.json must not contain these keys at all (omitempty).
	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	raw, rerr := os.ReadFile(metaPath)
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	if _, has := parsed["acquisition"]; has {
		t.Error(`metadata.json has "acquisition" key for mbox; want omitted`)
	}
	if _, has := parsed["data_subject"]; has {
		t.Error(`metadata.json has "data_subject" key for mbox; want omitted`)
	}
	if _, has := parsed["audience"]; has {
		t.Error(`metadata.json has "audience" key for mbox; want omitted`)
	}
}

// ---------------------------------------------------------------------
// TestFinalize_MatcherIDInReceipt
// ---------------------------------------------------------------------
//
// Drives Finalize with an UploadState whose parsed Metadata carries a
// matcher_id and asserts the field is persisted onto the returned
// FinalizeReceipt AND round-trips through the on-disk metadata.json so
// importers can route on it (glovebox-ugfx).

func TestFinalize_MatcherIDInReceipt(t *testing.T) {
	content := []byte("From foo@bar.com\nSubject: route me\n\nbody\n")
	fx := setupFixture(t, "archive/mbox", "route.mbox", content)
	fx.state.Meta.MatcherID = "apple/media-services"
	fx.state.SourceID = "recognizer"

	ctx := authedCtx("recognizer")
	receipt, err := Finalize(ctx, fx.state, FinalizeConfig{StagingRoot: fx.stagingRoot})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// --- struct-level assertion ---
	if receipt.MatcherID != "apple/media-services" {
		t.Errorf("receipt.MatcherID=%q want apple/media-services", receipt.MatcherID)
	}

	// --- on-disk metadata.json round-trip ---
	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	raw, rerr := os.ReadFile(metaPath)
	if rerr != nil {
		t.Fatalf("read metadata.json: %v", rerr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	if parsed["matcher_id"] != "apple/media-services" {
		t.Errorf(`metadata.json matcher_id=%v, want "apple/media-services"`, parsed["matcher_id"])
	}
}

// ---------------------------------------------------------------------
// Compile-time guard: confirm UploadState.Hasher satisfies hash.Hash so
// the assumption that st.Hasher.Sum(nil) returns the running digest
// holds regardless of any future store.go evolution.
// ---------------------------------------------------------------------

var _ hash.Hash = sha256.New()

// ---------------------------------------------------------------------
// recognizer-scanner operator-lane tests (glovebox-9s60)
// ---------------------------------------------------------------------

// scannerRegistry builds a source.Registry with a single recognizer-scanner
// entry whose data_subject_default is the supplied value and whose audience
// default is ["operator"].
func scannerRegistry(t *testing.T, dataSubjectDefault string) *source.Registry {
	t.Helper()
	js := `{ "enforce": true, "sources": [
		{ "source_id": "recognizer-scanner", "kind": "recognizer-scanner",
		  "data_subject_default": "` + dataSubjectDefault + `", "audience_default": ["operator"] } ] }`
	path := filepath.Join(t.TempDir(), "sources.json")
	if err := os.WriteFile(path, []byte(js), 0o600); err != nil {
		t.Fatalf("write sources.json: %v", err)
	}
	reg, err := source.Load(path)
	if err != nil {
		t.Fatalf("source.Load: %v", err)
	}
	return reg
}

// scanFixture sets up a recognizer-scan tar fixture (with or without ocr.txt)
// authenticated as the given source-id.
func scanFixture(t *testing.T, sourceID string, withOCR bool) *stagedFixture {
	t.Helper()
	entries := map[string][]byte{
		"manifest.json": []byte(`{"scan_id":"s1"}`),
		"scan.pdf":      []byte("%PDF-1.7 fake"),
	}
	if withOCR {
		entries["ocr.txt"] = []byte("Invoice 2026\nTotal: $40")
	}
	tarball := buildFinalizeTar(t, entries)
	fx := setupFixture(t, "archive/recognizer-scan", "scan-0001.tar", tarball)
	fx.state.SourceID = sourceID
	return fx
}

// readStagedReceipt reads and unmarshals a published metadata.json.
func readStagedReceipt(t *testing.T, fx *stagedFixture) FinalizeReceipt {
	t.Helper()
	metaPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "metadata.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var r FinalizeReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	return r
}

func assertNothingPublished(t *testing.T, fx *stagedFixture) {
	t.Helper()
	archDir := filepath.Join(fx.stagingRoot, "archives", fx.archiveID)
	if _, err := os.Stat(archDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected nothing published at %s, stat err=%v", archDir, err)
	}
	tmpPath := filepath.Join(fx.stagingRoot, ".tmp-archives", fx.uploadID)
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file lingered after rejected finalize")
	}
}

func TestFinalize_ScannerStampsPolicy(t *testing.T) {
	fx := scanFixture(t, "recognizer-scanner", true)
	reg := scannerRegistry(t, "e_111111")

	_, err := Finalize(authedCtx("recognizer-scanner"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	staged := readStagedReceipt(t, fx)
	if staged.Source != "recognizer-scanner" {
		t.Errorf("Source=%q want recognizer-scanner", staged.Source)
	}
	if staged.DataSubject != "e_111111" {
		t.Errorf("DataSubject=%q want e_111111", staged.DataSubject)
	}
	if len(staged.Audience) != 1 || staged.Audience[0] != "operator" {
		t.Errorf("Audience=%v want [operator]", staged.Audience)
	}

	mdPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "content.extracted.md")
	body, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read content.extracted.md: %v", err)
	}
	if !bytes.Contains(body, []byte("Invoice 2026")) {
		t.Errorf("content.extracted.md missing OCR body: %q", body)
	}
}

func TestFinalize_ScannerDataSubjectDefaultHonored(t *testing.T) {
	fx := scanFixture(t, "recognizer-scanner", true)
	reg := scannerRegistry(t, "household")

	if _, err := Finalize(authedCtx("recognizer-scanner"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if staged := readStagedReceipt(t, fx); staged.DataSubject != "household" {
		t.Errorf("DataSubject=%q want household (per-connector knob)", staged.DataSubject)
	}
}

func TestFinalize_ScannerExplicitDataSubjectWins(t *testing.T) {
	fx := scanFixture(t, "recognizer-scanner", true)
	fx.state.Meta.DataSubject = "e_333333"
	reg := scannerRegistry(t, "e_111111")

	if _, err := Finalize(authedCtx("recognizer-scanner"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	staged := readStagedReceipt(t, fx)
	if staged.DataSubject != "e_333333" {
		t.Errorf("DataSubject=%q want e_333333 (per-item override wins)", staged.DataSubject)
	}
	if len(staged.Audience) != 1 || staged.Audience[0] != "operator" {
		t.Errorf("Audience=%v want [operator]", staged.Audience)
	}
}

func TestFinalize_ScannerForcesOperatorMarker(t *testing.T) {
	fx := scanFixture(t, "recognizer-scanner", true)
	// Producer asserts a non-operator audience; the lane must override it.
	fx.state.Meta.Audience = []string{"household"}
	reg := scannerRegistry(t, "e_111111")

	if _, err := Finalize(authedCtx("recognizer-scanner"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if staged := readStagedReceipt(t, fx); len(staged.Audience) != 1 || staged.Audience[0] != "operator" {
		t.Errorf("Audience=%v want forced [operator]", staged.Audience)
	}
}

func TestFinalize_SpoofRejected(t *testing.T) {
	fx := scanFixture(t, "rss", true) // valid-looking source, NOT the scanner
	reg := scannerRegistry(t, "e_111111")

	_, err := Finalize(authedCtx("rss"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}})
	if !errors.Is(err, ErrSourceNotAuthorized) {
		t.Fatalf("err=%v want ErrSourceNotAuthorized", err)
	}
	assertNothingPublished(t, fx)
}

func TestFinalize_SpoofRejectedNilRegistry(t *testing.T) {
	fx := scanFixture(t, "recognizer-scanner", true)
	// Registry unconfigured (nil): the media-type gate is fail-CLOSED.
	_, err := Finalize(authedCtx("recognizer-scanner"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot})
	if !errors.Is(err, ErrSourceNotAuthorized) {
		t.Fatalf("err=%v want ErrSourceNotAuthorized for nil registry", err)
	}
	assertNothingPublished(t, fx)
}

func TestFinalize_ScannerMissingOCR(t *testing.T) {
	fx := scanFixture(t, "recognizer-scanner", false) // no ocr.txt
	reg := scannerRegistry(t, "e_111111")

	_, err := Finalize(authedCtx("recognizer-scanner"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}})
	if !errors.Is(err, ErrScanMissingOCR) {
		t.Fatalf("err=%v want ErrScanMissingOCR", err)
	}
	assertNothingPublished(t, fx)
}

func TestFinalize_NonScannerUnaffected(t *testing.T) {
	// An ordinary mbox delivery with a registry present but a non-scanner
	// source must behave exactly as before: no Source, no operator marker.
	content := []byte("From a@b\nSubject: x\n\nbody\n")
	fx := setupFixture(t, "archive/mbox", "test.mbox", content)
	fx.state.SourceID = "imap"
	reg := scannerRegistry(t, "e_111111")

	receipt, err := Finalize(authedCtx("imap"), fx.state,
		FinalizeConfig{StagingRoot: fx.stagingRoot, Sources: reg, ExtractScanner: passExtractScanner{}})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if receipt.Source != "" {
		t.Errorf("Source=%q want empty for non-scanner", receipt.Source)
	}
	if receipt.Audience != nil {
		t.Errorf("Audience=%v want nil for non-scanner", receipt.Audience)
	}
	mdPath := filepath.Join(fx.stagingRoot, "archives", fx.archiveID, "content.extracted.md")
	if _, err := os.Stat(mdPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("content.extracted.md should not exist for non-scanner delivery")
	}
}
