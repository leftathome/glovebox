package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	otelnoop "go.opentelemetry.io/otel/metric/noop"
)

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
