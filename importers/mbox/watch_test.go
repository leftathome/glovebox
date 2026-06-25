package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	otelnoop "go.opentelemetry.io/otel/metric/noop"
)

// stageArchive writes archives/<id>/{metadata.json, raw/<filename>} using the
// bytes of an existing mbox file, mirroring the spec 13 finalize layout.
func stageArchive(t *testing.T, archivesDir, id, mediaType, rawFilename, mboxPath string) {
	t.Helper()
	dir := filepath.Join(archivesDir, id)
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raw", rawFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"archive_id":%q,"media_type":%q,"raw_filename":%q}`, id, mediaType, rawFilename)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseMediaTypes(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]bool
	}{
		{"", map[string]bool{"archive/mbox": true}},
		{"archive/mbox", map[string]bool{"archive/mbox": true}},
		{" archive/mbox , archive/imap-export ", map[string]bool{"archive/mbox": true, "archive/imap-export": true}},
		{"archive/mbox,,archive/mbox", map[string]bool{"archive/mbox": true}},
	}
	for _, c := range cases {
		got := parseMediaTypes(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseMediaTypes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSafeRawFilename(t *testing.T) {
	ok := []string{"archive.mbox", "All mail Including Spam and Trash.mbox", "a_b-c.mbox"}
	bad := []string{"", ".", "..", "sub/archive.mbox", "/etc/passwd", "a\x00b", "..\\x"}
	for _, s := range ok {
		if err := safeRawFilename(s); err != nil {
			t.Errorf("safeRawFilename(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := safeRawFilename(s); err == nil {
			t.Errorf("safeRawFilename(%q) = nil, want error", s)
		}
	}
}

func TestReadArchiveMeta(t *testing.T) {
	dir := t.TempDir()
	good := `{"archive_id":"abc-123","media_type":"archive/mbox","raw_filename":"all.mbox","sha256":"deadbeef"}`
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readArchiveMeta(dir)
	if err != nil {
		t.Fatalf("readArchiveMeta: %v", err)
	}
	if m.ArchiveID != "abc-123" || m.MediaType != "archive/mbox" || m.RawFilename != "all.mbox" {
		t.Errorf("got %+v", m)
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "metadata.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchiveMeta(bad); err == nil {
		t.Error("readArchiveMeta on malformed json = nil error, want error")
	}

	if _, err := readArchiveMeta(t.TempDir()); err == nil {
		t.Error("readArchiveMeta on missing file = nil error, want error")
	}
}

func TestAcquireReleaseLock(t *testing.T) {
	dir := t.TempDir()
	if err := acquireLock(dir); err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	if err := acquireLock(dir); err == nil {
		t.Error("second acquireLock = nil, want already-held error")
	} else if !os.IsExist(err) {
		t.Errorf("second acquireLock err = %v, want os.IsExist", err)
	}
	if err := releaseLock(dir); err != nil {
		t.Fatalf("releaseLock: %v", err)
	}
	if err := acquireLock(dir); err != nil {
		t.Fatalf("acquireLock after release: %v", err)
	}
}

func TestMoveToDone(t *testing.T) {
	archives := t.TempDir()
	src := filepath.Join(archives, "arch-1")
	if err := os.MkdirAll(filepath.Join(src, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := moveToDone(archives, src, "arch-1"); err != nil {
		t.Fatalf("moveToDone: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source dir still exists after moveToDone")
	}
	if _, err := os.Stat(filepath.Join(archives, ".done", "arch-1", "raw")); err != nil {
		t.Errorf(".done/arch-1/raw missing: %v", err)
	}
}

func TestNewArchiveMetrics(t *testing.T) {
	mp := otelnoop.NewMeterProvider()
	am, err := newArchiveMetrics(mp)
	if err != nil {
		t.Fatalf("newArchiveMetrics: %v", err)
	}
	// Recording must not panic with a real-but-noop provider.
	am.processed(context.Background())
	am.failed(context.Background())
	am.skipped(context.Background(), "media_type")
	am.skipped(context.Background(), "locked")
}

func TestE2E_WatchArchives_PicksUpAndRetires(t *testing.T) {
	root := t.TempDir()
	archivesDir := filepath.Join(root, "archives")
	if err := os.MkdirAll(archivesDir, 0o700); err != nil {
		t.Fatal(err)
	}

	mboxPath := writeMbox(t, root, fixtureSpecs)
	filterPath, configPath, stateDir := writeSupportFiles(t, root)

	mock := newIngestMock()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(srv.Close)

	stageArchive(t, archivesDir, "arch-001", "archive/mbox", "all.mbox", mboxPath)

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"--watch-archives", archivesDir,
		"--filter", filterPath,
		"--config", configPath,
		"--state-dir", stateDir,
		"--ingest-url", srv.URL,
		"--source-name", sourceName,
		"--poll-interval", "200ms",
		"--health-port", fmt.Sprintf("%d", freePort(t)),
	}

	done := make(chan int, 1)
	go func() { done <- runCtx(ctx, args) }()

	// Poll for delivery + retirement; no fixed sleeps on the code under test.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, doneErr := os.Stat(filepath.Join(archivesDir, ".done", "arch-001"))
		if mock.count() >= wantIngested && doneErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: delivered=%d (want %d), .done present=%v",
				mock.count(), wantIngested, doneErr == nil)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(filepath.Join(archivesDir, "arch-001")); !os.IsNotExist(err) {
		t.Error("archives/arch-001 still present after retire")
	}

	// The operator filter must be applied in watcher mode: the manifest (which
	// travels with the archive into .done) records exactly wantIngested ingested
	// and wantFiltered filtered, with zero errored. A regression that dropped the
	// filter would show the excluded messages as errored instead of filtered.
	movedSource := filepath.Join(archivesDir, ".done", "arch-001", "raw", "all.mbox")
	mf := loadManifestForSource(t, movedSource)
	if mf.Counts.MessagesIngested != wantIngested {
		t.Errorf("manifest ingested = %d, want %d", mf.Counts.MessagesIngested, wantIngested)
	}
	if mf.Counts.MessagesFiltered != wantFiltered {
		t.Errorf("manifest filtered = %d, want %d (operator filter not applied?)",
			mf.Counts.MessagesFiltered, wantFiltered)
	}
	if mf.Counts.MessagesErrored != wantErrored {
		t.Errorf("manifest errored = %d, want %d", mf.Counts.MessagesErrored, wantErrored)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runCtx exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCtx did not return after cancel")
	}
}
