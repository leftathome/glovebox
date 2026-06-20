package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// buildZip returns the bytes of an in-memory zip whose entries are the given
// name->content pairs. Names are written verbatim so callers can inject
// traversal payloads (e.g. "../escape.txt").
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestUnpackZipReaderNestedFile(t *testing.T) {
	dest := t.TempDir()
	data := buildZip(t, map[string]string{
		"Apple_Media_Services/purchases.csv": "header\nvalue\n",
	})

	got, err := UnpackZipReader(bytes.NewReader(data), int64(len(data)), dest)
	if err != nil {
		t.Fatalf("UnpackZipReader: %v", err)
	}
	if len(got) != 1 || got[0] != "Apple_Media_Services/purchases.csv" {
		t.Fatalf("extracted = %v, want [Apple_Media_Services/purchases.csv]", got)
	}

	out := filepath.Join(dest, "Apple_Media_Services", "purchases.csv")
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(b) != "header\nvalue\n" {
		t.Fatalf("content = %q, want %q", string(b), "header\nvalue\n")
	}
}

func TestUnpackZipReaderRejectsZipSlip(t *testing.T) {
	dest := t.TempDir()
	data := buildZip(t, map[string]string{
		"../escape.txt": "pwned",
	})

	_, err := UnpackZipReader(bytes.NewReader(data), int64(len(data)), dest)
	if err == nil {
		t.Fatal("expected error for zip-slip entry, got nil")
	}
	if !errors.Is(err, ErrZipSlip) {
		t.Fatalf("error = %v, want ErrZipSlip", err)
	}

	// No file may have been written outside the destination dir.
	escaped := filepath.Join(filepath.Dir(dest), "escape.txt")
	if _, statErr := os.Stat(escaped); !os.IsNotExist(statErr) {
		t.Fatalf("zip-slip wrote a file outside dest at %s (stat err=%v)", escaped, statErr)
	}
}

func TestUnpackZipFromPath(t *testing.T) {
	dest := t.TempDir()
	srcDir := t.TempDir()
	data := buildZip(t, map[string]string{
		"a/b.txt": "hello",
	})
	zipPath := filepath.Join(srcDir, "bucket.zip")
	if err := os.WriteFile(zipPath, data, 0o600); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	got, err := UnpackZip(zipPath, dest)
	if err != nil {
		t.Fatalf("UnpackZip: %v", err)
	}
	if len(got) != 1 || got[0] != "a/b.txt" {
		t.Fatalf("extracted = %v, want [a/b.txt]", got)
	}
	b, err := os.ReadFile(filepath.Join(dest, "a", "b.txt"))
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q, want %q", string(b), "hello")
	}
}
