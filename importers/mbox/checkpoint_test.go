package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/importer"
)

func TestCheckpointPath(t *testing.T) {
	got := CheckpointPath("/foo/bar.mbox")
	want := "/foo/bar.mbox.checkpoint"
	if got != want {
		t.Fatalf("CheckpointPath: got %q want %q", got, want)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.mbox.checkpoint")

	orig := &Checkpoint{
		ByteOffset:            12345678,
		LastIngestedMessageID: "<abc-123@example.com>",
	}
	if err := orig.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	loaded, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if loaded.ByteOffset != orig.ByteOffset {
		t.Errorf("ByteOffset: got %d want %d", loaded.ByteOffset, orig.ByteOffset)
	}
	if loaded.LastIngestedMessageID != orig.LastIngestedMessageID {
		t.Errorf("LastIngestedMessageID: got %q want %q",
			loaded.LastIngestedMessageID, orig.LastIngestedMessageID)
	}
}

func TestCheckpointRoundTripEmptyMessageID(t *testing.T) {
	// Early in a run we may have no last-ingested Message-ID yet. An
	// empty string must round-trip cleanly (not be mapped to some
	// placeholder or dropped).
	dir := t.TempDir()
	path := filepath.Join(dir, "m.checkpoint")

	orig := &Checkpoint{ByteOffset: 0, LastIngestedMessageID: ""}
	if err := orig.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if loaded.ByteOffset != 0 || loaded.LastIngestedMessageID != "" {
		t.Errorf("zero-value round-trip mismatch: %#v", loaded)
	}
}

func TestLoadCheckpointNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.checkpoint")
	_, err := LoadCheckpoint(path)
	if err == nil {
		t.Fatalf("expected error loading missing checkpoint, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected errors.Is(err, fs.ErrNotExist), got %v", err)
	}
}

func TestRemoveCheckpointMissingIsNoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never-existed.checkpoint")
	if err := RemoveCheckpoint(path); err != nil {
		t.Fatalf("RemoveCheckpoint on missing file returned error: %v", err)
	}
}

func TestRemoveCheckpointDeletesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "present.checkpoint")
	c := &Checkpoint{ByteOffset: 42, LastIngestedMessageID: "<x@example.com>"}
	if err := c.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("precondition: checkpoint should exist: %v", err)
	}
	if err := RemoveCheckpoint(path); err != nil {
		t.Fatalf("RemoveCheckpoint: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected file removed, stat returned %v", err)
	}

	// And idempotent: second call on a now-missing file is still a no-op.
	if err := RemoveCheckpoint(path); err != nil {
		t.Fatalf("second RemoveCheckpoint: %v", err)
	}
}

func TestCheckpointAtomicWriteLeavesNoPartialOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}

	// Path strategy mirrors TestAtomicWriteLeavesNoPartialOnFailure in
	// manifest_test.go:
	//   - Non-root: chmod the parent 0500 so OpenFile of the tmp file
	//     fails.
	//   - Root (CI containers): chmod is ignored, so point Write at a
	//     path whose parent directory does not exist, which also forces
	//     OpenFile to fail.
	// Both branches verify: no partial checkpoint at the target, no
	// lingering `.tmp-*` sibling.
	parent := t.TempDir()
	var target, watchDir string

	if os.Geteuid() == 0 {
		target = filepath.Join(parent, "does-not-exist", "c.checkpoint")
		watchDir = parent
	} else {
		roDir := filepath.Join(parent, "ro")
		if err := os.Mkdir(roDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		target = filepath.Join(roDir, "c.checkpoint")
		if err := os.WriteFile(target, []byte(`{"byte_offset":1}`), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.Chmod(roDir, 0o500); err != nil {
			t.Fatalf("chmod ro: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
		watchDir = roDir
	}

	c := &Checkpoint{ByteOffset: 9_999, LastIngestedMessageID: "<z@example.com>"}
	if err := c.Write(target); err == nil {
		t.Fatalf("expected Write to fail, got nil")
	}

	if os.Geteuid() != 0 {
		if err := os.Chmod(watchDir, 0o700); err != nil {
			t.Fatalf("chmod restore: %v", err)
		}
	}

	entries, err := os.ReadDir(watchDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", watchDir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("partial temp file lingered: %s", e.Name())
		}
	}

	// Non-root branch additionally verifies the pre-existing checkpoint
	// is untouched by the failed atomic write.
	if os.Geteuid() != 0 {
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if string(got) != `{"byte_offset":1}` {
			t.Errorf("target was mutated despite failed write: %s", got)
		}
	}
}

func TestImportManifestV1SatisfiesManifestInterface(t *testing.T) {
	// This is a runtime belt-and-suspenders companion to the
	// compile-time `var _ importer.Manifest = (*ImportManifestV1)(nil)`
	// assertion in manifest.go: it also exercises the method values so
	// a future refactor that silently widens a return type fails loud.
	m := &ImportManifestV1{
		StatusValue: validatedStatus(importer.StatusInterrupted),
		ResumeState: ResumeState{
			ByteOffset:            500,
			LastIngestedMessageID: "<last@example.com>",
		},
		MessageIDsIngested: []string{"<a@example.com>", "<b@example.com>"},
	}

	var iface importer.Manifest = m

	if got := iface.Status(); got != importer.StatusInterrupted {
		t.Errorf("Status: got %q want %q", got, importer.StatusInterrupted)
	}
	if got := iface.ByteOffset(); got != 500 {
		t.Errorf("ByteOffset: got %d want 500", got)
	}
	got := iface.MessageIDs()
	if len(got) != 2 || got[0] != "<a@example.com>" || got[1] != "<b@example.com>" {
		t.Errorf("MessageIDs: got %v", got)
	}
}
