package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRouteToFailed_MovesItem(t *testing.T) {
	base := t.TempDir()
	itemDir := filepath.Join(base, "staging", "20260328-item")
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "content.raw"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(itemDir, "metadata.json"), []byte(`{}`), 0644)

	failedDir := filepath.Join(base, "failed")

	err := RouteToFailed(itemDir, failedDir, "routing_error")
	if err != nil {
		t.Fatal(err)
	}

	// Item should be in failed dir
	destDir := filepath.Join(failedDir, "20260328-item")
	if _, err := os.Stat(filepath.Join(destDir, "content.raw")); err != nil {
		t.Error("content.raw should exist in failed dir")
	}

	// Item should NOT be in staging
	if _, err := os.Stat(itemDir); !os.IsNotExist(err) {
		t.Error("item should have been moved out of staging")
	}
}

func TestRouteToFailed_CreatesDir(t *testing.T) {
	base := t.TempDir()
	itemDir := filepath.Join(base, "staging", "item")
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "content.raw"), []byte("data"), 0644)

	failedDir := filepath.Join(base, "failed")
	// failed dir does not exist yet

	err := RouteToFailed(itemDir, failedDir, "test")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(failedDir); err != nil {
		t.Error("failed dir should have been created")
	}
}
