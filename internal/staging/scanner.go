package staging

import (
	"fmt"
	"os"
	"path/filepath"
)

func ReadStagingItem(dirPath string, allowlist []string) (StagingItem, error) {
	contentPath := filepath.Join(dirPath, "content.raw")
	metadataPath := filepath.Join(dirPath, "metadata.json")

	if _, err := os.Stat(contentPath); err != nil {
		return StagingItem{}, fmt.Errorf("content.raw missing: %w", err)
	}

	mf, err := os.Open(metadataPath)
	if err != nil {
		return StagingItem{}, fmt.Errorf("metadata.json missing: %w", err)
	}
	defer mf.Close()

	meta, err := ParseMetadata(mf)
	if err != nil {
		return StagingItem{}, fmt.Errorf("parse metadata: %w", err)
	}

	meta.Subject = StripSubjectControlChars(meta.Subject)

	if errs := Validate(meta, allowlist); len(errs) > 0 {
		return StagingItem{}, fmt.Errorf("validation failed: %v", errs)
	}

	return StagingItem{
		DirPath:     dirPath,
		ContentPath: contentPath,
		Metadata:    meta,
	}, nil
}
