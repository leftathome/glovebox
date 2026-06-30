// Package integrationtest provides a shared harness for connector
// integration/smoke tests: stage a connector's output to a temp dir via a
// real StagingWriter and read the committed items back. See
// docs/superpowers/specs/2026-06-29-connector-integration-harness-design.md.
package integrationtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
)

// StagedItem is one committed staging item read back from disk.
type StagedItem struct {
	Dir         string               // absolute item directory
	Meta        staging.ItemMetadata // parsed metadata.json
	ContentPath string               // <dir>/content.raw (may not exist)
	Sidecars    []string             // filenames other than content.raw/metadata.json
}

// StageToTempDir returns a StagingWriter rooted at t.TempDir() and a
// readback func that returns every committed item. connectorName is
// required by connector.NewStagingWriter.
func StageToTempDir(t *testing.T, connectorName string) (*connector.StagingWriter, func() []StagedItem) {
	t.Helper()
	dir := t.TempDir()
	w, err := connector.NewStagingWriter(dir, connectorName)
	if err != nil {
		t.Fatalf("integrationtest: NewStagingWriter: %v", err)
	}
	return w, func() []StagedItem { return readStaged(t, dir) }
}

func readStaged(t *testing.T, dir string) []StagedItem {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("integrationtest: read staging dir: %v", err)
	}
	var out []StagedItem
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // skip .tmp and hidden
		}
		itemDir := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(filepath.Join(itemDir, "metadata.json"))
		if err != nil {
			t.Fatalf("integrationtest: read metadata.json in %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("integrationtest: parse metadata.json in %s: %v", e.Name(), err)
		}
		si := StagedItem{Dir: itemDir, Meta: meta, ContentPath: filepath.Join(itemDir, "content.raw")}
		files, _ := os.ReadDir(itemDir)
		for _, f := range files {
			n := f.Name()
			if n == "content.raw" || n == "metadata.json" {
				continue
			}
			si.Sidecars = append(si.Sidecars, n)
		}
		out = append(out, si)
	}
	return out
}
