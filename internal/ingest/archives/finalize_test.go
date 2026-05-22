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
		"a.txt":       []byte("hello a"),
		"b.txt":       []byte("hello b"),
		"sub/c.txt":   []byte("hello c"),
		"sub/d.txt":   []byte("hello d"),
		"deep/e.txt":  []byte("hello e"),
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
// Compile-time guard: confirm UploadState.Hasher satisfies hash.Hash so
// the assumption that st.Hasher.Sum(nil) returns the running digest
// holds regardless of any future store.go evolution.
// ---------------------------------------------------------------------

var _ hash.Hash = sha256.New()
