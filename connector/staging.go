package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode"

	"github.com/google/uuid"
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

func NewStagingWriter(stagingDir string, connectorName string) *StagingWriter {
	tmpDir := filepath.Join(stagingDir+"-tmp", connectorName)
	os.MkdirAll(tmpDir, 0755)
	return &StagingWriter{
		stagingDir:    stagingDir,
		connectorName: connectorName,
		tmpDir:        tmpDir,
	}
}

type StagingItem struct {
	dir  string
	opts ItemOptions
	file *os.File
}

func (w *StagingWriter) NewItem(opts ItemOptions) (*StagingItem, error) {
	name := fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), uuid.New().String()[:8])
	dir := filepath.Join(w.tmpDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create item dir: %w", err)
	}
	return &StagingItem{dir: dir, opts: opts}, nil
}

func (si *StagingItem) WriteContent(data []byte) error {
	path := filepath.Join(si.dir, "content.raw")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open content.raw: %w", err)
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (si *StagingItem) ContentWriter() (io.WriteCloser, error) {
	path := filepath.Join(si.dir, "content.raw")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open content.raw: %w", err)
	}
	return f, nil
}

func (si *StagingItem) Commit(stagingDir string) error {
	if err := validateItemOptions(si.opts); err != nil {
		os.RemoveAll(si.dir)
		return fmt.Errorf("metadata validation: %w", err)
	}

	meta := map[string]any{
		"source":            si.opts.Source,
		"sender":            si.opts.Sender,
		"subject":           stripControlChars(si.opts.Subject),
		"timestamp":         si.opts.Timestamp.Format(time.RFC3339),
		"destination_agent": si.opts.DestinationAgent,
		"content_type":      si.opts.ContentType,
		"ordered":           si.opts.Ordered,
		"auth_failure":      si.opts.AuthFailure,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(si.dir, "metadata.json"), data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	destDir := filepath.Join(stagingDir, filepath.Base(si.dir))
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

func validateItemOptions(opts ItemOptions) error {
	if opts.DestinationAgent == "" {
		return fmt.Errorf("destination_agent is required")
	}
	if len(opts.Source) > 1024 {
		return fmt.Errorf("source exceeds 1024 characters")
	}
	if len(opts.Sender) > 1024 {
		return fmt.Errorf("sender exceeds 1024 characters")
	}
	if len(opts.Subject) > 1024 {
		return fmt.Errorf("subject exceeds 1024 characters")
	}
	if len(opts.DestinationAgent) > 64 {
		return fmt.Errorf("destination_agent exceeds 64 characters")
	}
	if len(opts.ContentType) > 64 {
		return fmt.Errorf("content_type exceeds 64 characters")
	}
	for _, field := range []struct{ name, value string }{
		{"source", opts.Source},
		{"sender", opts.Sender},
		{"destination_agent", opts.DestinationAgent},
		{"content_type", opts.ContentType},
	} {
		if hasControlCharsConnector(field.value) {
			return fmt.Errorf("%s contains control characters", field.name)
		}
	}
	return nil
}

func hasControlCharsConnector(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return true
		}
	}
	return false
}

func stripControlChars(s string) string {
	result := make([]rune, 0, len(s))
	for _, r := range s {
		if !unicode.IsControl(r) || r == '\n' || r == '\r' || r == '\t' {
			result = append(result, r)
		}
	}
	return string(result)
}
