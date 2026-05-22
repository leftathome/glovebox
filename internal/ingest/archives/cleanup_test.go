// Tests for the orphan cleanup goroutine (spec 13 §5.5).
//
// Coverage:
//
//   TestCleanup_TmpOlderThan72hRemoved
//   TestCleanup_TmpYoungerThan72hPreserved
//   TestCleanup_FinalizeOlderThan1hRemoved
//   TestCleanup_FinalizeYoungerThan1hPreserved
//   TestCleanup_DoneOlderThan7dRemoved
//   TestCleanup_DoneYoungerThan7dPreserved
//   TestCleanup_EmitsEventCleanupLog
//   TestCleanup_MissingDirectoriesNotAnError
//   TestCleanup_GoroutineExitsOnCtxCancel

package archives

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stageTmpEntry creates a .tmp-archives/<id> file (when finalize=false)
// or a .tmp-archives/<id>.finalize dir (when finalize=true) and sets
// its mtime to `age` ago.
func stageTmpEntry(t *testing.T, root, id string, finalize bool, age time.Duration, contentSize int) {
	t.Helper()
	tmpRoot := filepath.Join(root, ".tmp-archives")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		t.Fatalf("mkdir tmpRoot: %v", err)
	}
	when := time.Now().Add(-age)

	if finalize {
		dir := filepath.Join(tmpRoot, id+".finalize")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir finalize: %v", err)
		}
		// Add a small file inside so freedBytes accumulates a known
		// non-zero figure.
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), make([]byte, contentSize), 0o600); err != nil {
			t.Fatalf("write inside finalize: %v", err)
		}
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatalf("chtimes finalize: %v", err)
		}
		return
	}

	path := filepath.Join(tmpRoot, id)
	if err := os.WriteFile(path, make([]byte, contentSize), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes tmp: %v", err)
	}
}

// stageDoneEntry creates an archives/.done/<id>/ dir with a metadata.json
// inside and sets its mtime to `age` ago.
func stageDoneEntry(t *testing.T, root, id string, age time.Duration, contentSize int) {
	t.Helper()
	dir := filepath.Join(root, "archives", ".done", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir done: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"archive_id": id})
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), body, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, contentSize), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatalf("chtimes done: %v", err)
	}
}

func defaultCleanupCfg(root string) CleanupConfig {
	return CleanupConfig{
		StagingRoot:   root,
		TmpAge:        72 * time.Hour,
		FinalizeAge:   time.Hour,
		DoneRetention: 7 * 24 * time.Hour,
	}
}

func TestCleanup_TmpOlderThan72hRemoved(t *testing.T) {
	root := t.TempDir()
	stageTmpEntry(t, root, "stale-id", false, 80*time.Hour, 100)
	runCleanupOnce(defaultCleanupCfg(root))
	if _, err := os.Stat(filepath.Join(root, ".tmp-archives", "stale-id")); !os.IsNotExist(err) {
		t.Errorf("expected stale-id removed, err=%v", err)
	}
}

func TestCleanup_TmpYoungerThan72hPreserved(t *testing.T) {
	root := t.TempDir()
	stageTmpEntry(t, root, "fresh-id", false, time.Hour, 50)
	runCleanupOnce(defaultCleanupCfg(root))
	if _, err := os.Stat(filepath.Join(root, ".tmp-archives", "fresh-id")); err != nil {
		t.Errorf("fresh-id removed, err=%v", err)
	}
}

func TestCleanup_FinalizeOlderThan1hRemoved(t *testing.T) {
	root := t.TempDir()
	stageTmpEntry(t, root, "stale-id", true, 2*time.Hour, 100)
	runCleanupOnce(defaultCleanupCfg(root))
	if _, err := os.Stat(filepath.Join(root, ".tmp-archives", "stale-id.finalize")); !os.IsNotExist(err) {
		t.Errorf("expected stale finalize removed, err=%v", err)
	}
}

func TestCleanup_FinalizeYoungerThan1hPreserved(t *testing.T) {
	root := t.TempDir()
	stageTmpEntry(t, root, "fresh-id", true, 30*time.Minute, 100)
	runCleanupOnce(defaultCleanupCfg(root))
	if _, err := os.Stat(filepath.Join(root, ".tmp-archives", "fresh-id.finalize")); err != nil {
		t.Errorf("fresh finalize removed, err=%v", err)
	}
}

func TestCleanup_DoneOlderThan7dRemoved(t *testing.T) {
	root := t.TempDir()
	stageDoneEntry(t, root, "old-archive", 10*24*time.Hour, 200)
	runCleanupOnce(defaultCleanupCfg(root))
	if _, err := os.Stat(filepath.Join(root, "archives", ".done", "old-archive")); !os.IsNotExist(err) {
		t.Errorf("expected old-archive removed, err=%v", err)
	}
}

func TestCleanup_DoneYoungerThan7dPreserved(t *testing.T) {
	root := t.TempDir()
	stageDoneEntry(t, root, "new-archive", 24*time.Hour, 200)
	runCleanupOnce(defaultCleanupCfg(root))
	if _, err := os.Stat(filepath.Join(root, "archives", ".done", "new-archive")); err != nil {
		t.Errorf("new-archive removed, err=%v", err)
	}
}

func TestCleanup_EmitsEventCleanupLog(t *testing.T) {
	root := t.TempDir()
	stageTmpEntry(t, root, "stale-tmp", false, 80*time.Hour, 17)
	stageTmpEntry(t, root, "stale-fin", true, 2*time.Hour, 31)
	stageDoneEntry(t, root, "stale-done", 10*24*time.Hour, 13)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := defaultCleanupCfg(root)
	cfg.Logger = logger
	runCleanupOnce(cfg)

	out := buf.String()
	if !strings.Contains(out, EventCleanup) {
		t.Errorf("missing EventCleanup log: %s", out)
	}
	if !strings.Contains(out, `"removed_uploads":1`) {
		t.Errorf("missing removed_uploads=1: %s", out)
	}
	if !strings.Contains(out, `"removed_finalize_dirs":1`) {
		t.Errorf("missing removed_finalize_dirs=1: %s", out)
	}
	if !strings.Contains(out, `"removed_done_archives":1`) {
		t.Errorf("missing removed_done_archives=1: %s", out)
	}
}

func TestCleanup_MissingDirectoriesNotAnError(t *testing.T) {
	root := t.TempDir()
	// No staging structure at all.

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultCleanupCfg(root)
	cfg.Logger = logger
	runCleanupOnce(cfg)

	if strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("unexpected WARN on missing dirs: %s", buf.String())
	}
}

func TestCleanup_GoroutineExitsOnCtxCancel(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartCleanupGoroutine(ctx, CleanupConfig{
			StagingRoot: root,
			Interval:    10 * time.Millisecond,
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup goroutine did not exit within 1s after ctx cancel")
	}
}
