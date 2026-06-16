package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFile_HappyPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	payload := []byte(`{"hello":"world"}`)

	if err := WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip mismatch: got %q want %q", got, payload)
	}

	// A clean write must not leave any .tmp-* siblings.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("unexpected lingering temp file: %s", e.Name())
		}
	}
}

// TestWriteFile_FailureBeforeRenameLeavesNoSibling verifies that when the
// temp-file creation itself fails (e.g. the target directory is read-only or
// does not exist), no target file is created and no .tmp-* sibling lingers.
func TestWriteFile_FailureBeforeRenameLeavesNoSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}

	parent := t.TempDir()
	var target, watchDir string

	// The failure path differs by effective UID:
	//   - As root: a non-existent parent directory also forces OpenFile
	//     to fail at temp-file creation.
	//   - As non-root: chmod 0500 on the parent denies write.
	if os.Geteuid() == 0 {
		target = filepath.Join(parent, "does-not-exist", "file.json")
		watchDir = parent
	} else {
		roDir := filepath.Join(parent, "ro")
		if err := os.Mkdir(roDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		target = filepath.Join(roDir, "file.json")
		if err := os.Chmod(roDir, 0o500); err != nil {
			t.Fatalf("chmod ro: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
		watchDir = roDir
	}

	err := WriteFile(target, []byte(`{"should":"not-land"}`), 0o644)
	if err == nil {
		t.Fatalf("expected WriteFile to fail, got nil")
	}

	if os.Geteuid() != 0 {
		if err := os.Chmod(watchDir, 0o700); err != nil {
			t.Fatalf("chmod restore: %v", err)
		}
	}

	// Target must not exist.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist after failure; stat err = %v", err)
	}

	// No .tmp-* siblings should linger.
	entries, err := os.ReadDir(watchDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("partial temp file lingered: %s", e.Name())
		}
	}
}

// TestWriteFile_FailureMidWriteCleansUpTmp simulates a mid-write failure by
// filling the target filesystem / truncating the write. Since Go's os.File
// does not expose a "short write" injection point, we approximate the
// mid-write failure path by racing two writers at the same exclusive temp
// name -- which is not deterministic -- so instead we exercise the cleanup
// path via a broken rename target: pre-create a read-only directory at
// `path` so os.Rename fails. The helper's cleanup must remove the temp file.
func TestWriteFile_FailureMidWriteCleansUpTmp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		// Running as root bypasses permission bits, so we cannot make
		// os.Rename deterministically fail via chmod. This branch just
		// covers the happy-path cleanup contract via the failure-before-
		// rename test above.
		t.Skip("running as root; rename-failure injection via chmod requires non-root")
	}

	dir := t.TempDir()
	// Rename fails when the target is an existing non-empty directory.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Drop a file inside it so POSIX renameat2 refuses to replace the
	// non-empty directory.
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	err := WriteFile(target, []byte(`{"no":"landing"}`), 0o644)
	if err == nil {
		t.Fatalf("expected WriteFile to fail (rename over non-empty dir), got nil")
	}

	// Temp file must have been cleaned up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("partial temp file lingered after rename failure: %s", e.Name())
		}
	}
}

func TestSidecarPath(t *testing.T) {
	cases := []struct {
		source, suffix, want string
	}{
		{"/foo/bar.mbox", ".survey.v1.json", "/foo/bar.mbox.survey.v1.json"},
		{"/foo/bar.mbox", ".checkpoint", "/foo/bar.mbox.checkpoint"},
		{"/foo/bar.mbox", ".import-manifest.v1.json", "/foo/bar.mbox.import-manifest.v1.json"},
	}
	for _, c := range cases {
		if got := SidecarPath(c.source, c.suffix); got != c.want {
			t.Errorf("SidecarPath(%q, %q) = %q, want %q", c.source, c.suffix, got, c.want)
		}
	}
}
