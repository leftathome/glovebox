package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/leftathome/glovebox/internal/staging"
)

type ItemOptions struct {
	Source           string
	Sender           string
	Subject          string
	Timestamp        time.Time
	DestinationAgent string
	ContentType      string
	Ordered          bool
	AuthFailure      bool
}

type StagingWriter struct {
	stagingDir    string
	connectorName string
	tmpDir        string
}

func NewStagingWriter(stagingDir string, connectorName string) (*StagingWriter, error) {
	tmpDir := filepath.Join(stagingDir+"-tmp", connectorName)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return nil, fmt.Errorf("create staging tmp dir: %w", err)
	}
	return &StagingWriter{
		stagingDir:    stagingDir,
		connectorName: connectorName,
		tmpDir:        tmpDir,
	}, nil
}

type StagingItem struct {
	dir        string
	stagingDir string
	opts       ItemOptions
}

func (w *StagingWriter) NewItem(opts ItemOptions) (*StagingItem, error) {
	name := fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), uuid.New().String()[:8])
	dir := filepath.Join(w.tmpDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create item dir: %w", err)
	}
	return &StagingItem{dir: dir, stagingDir: w.stagingDir, opts: opts}, nil
}

// WriteContent writes (or appends) content to content.raw.
// For streaming, use ContentWriter() instead.
func (si *StagingItem) WriteContent(data []byte) error {
	w, err := si.contentFile()
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = w.Write(data)
	return err
}

func (si *StagingItem) ContentWriter() (io.WriteCloser, error) {
	return si.contentFile()
}

func (si *StagingItem) contentFile() (*os.File, error) {
	path := filepath.Join(si.dir, "content.raw")
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func (si *StagingItem) Commit() error {
	// Build metadata using the shared type for consistent JSON keys
	meta := staging.ItemMetadata{
		Source:           si.opts.Source,
		Sender:           si.opts.Sender,
		Subject:          staging.StripSubjectControlChars(si.opts.Subject),
		Timestamp:        si.opts.Timestamp,
		DestinationAgent: si.opts.DestinationAgent,
		ContentType:      si.opts.ContentType,
		Ordered:          si.opts.Ordered,
		AuthFailure:      si.opts.AuthFailure,
	}

	// Validate using shared validation. Pass destination as its own allowlist
	// so the allowlist check passes -- glovebox does the real allowlist check.
	allowlist := []string{meta.DestinationAgent}
	if meta.DestinationAgent == "" {
		os.RemoveAll(si.dir)
		return fmt.Errorf("metadata validation: destination_agent is required")
	}
	if errs := staging.Validate(meta, allowlist); len(errs) > 0 {
		os.RemoveAll(si.dir)
		return fmt.Errorf("metadata validation: %v", errs)
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(si.dir, "metadata.json"), data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	destDir := filepath.Join(si.stagingDir, filepath.Base(si.dir))
	if err := os.Rename(si.dir, destDir); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}

	return nil
}

func (w *StagingWriter) CleanOrphans() {
	entries, err := os.ReadDir(w.tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			os.RemoveAll(filepath.Join(w.tmpDir, e.Name()))
		}
	}
}
