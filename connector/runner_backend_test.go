package connector

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectBackendHTTP(t *testing.T) {
	logger := slog.Default()

	backend, writer, err := selectBackend("test-connector", "http://localhost:9090/v1/ingest", "", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writer != nil {
		t.Fatal("expected writer to be nil in HTTP mode")
	}
	if backend == nil {
		t.Fatal("expected non-nil backend")
	}
	if _, ok := backend.(*HTTPStagingBackend); !ok {
		t.Fatalf("expected *HTTPStagingBackend, got %T", backend)
	}
}

func TestSelectBackendFilesystem(t *testing.T) {
	logger := slog.Default()
	stagingDir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		t.Fatal(err)
	}

	backend, writer, err := selectBackend("test-connector", "", stagingDir, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writer == nil {
		t.Fatal("expected non-nil writer in filesystem mode")
	}
	if backend == nil {
		t.Fatal("expected non-nil backend")
	}
	if _, ok := backend.(*StagingWriter); !ok {
		t.Fatalf("expected *StagingWriter, got %T", backend)
	}
	// backend and writer should be the same object
	if backend != writer {
		t.Fatal("expected backend and writer to be the same StagingWriter instance")
	}
}

func TestSelectBackendHTTPTakesPrecedence(t *testing.T) {
	logger := slog.Default()
	stagingDir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		t.Fatal(err)
	}

	// When both are provided, HTTP should win
	backend, writer, err := selectBackend("test-connector", "http://localhost:9090/v1/ingest", stagingDir, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writer != nil {
		t.Fatal("expected writer to be nil when HTTP mode is selected")
	}
	if _, ok := backend.(*HTTPStagingBackend); !ok {
		t.Fatalf("expected *HTTPStagingBackend, got %T", backend)
	}
}

func TestSelectBackendNeitherSet(t *testing.T) {
	logger := slog.Default()

	_, _, err := selectBackend("test-connector", "", "", logger)
	if err == nil {
		t.Fatal("expected error when neither ingestURL nor stagingDir is set")
	}
}
