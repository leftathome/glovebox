package routing

import (
	"fmt"
	"os"
	"path/filepath"
)

// RouteToFailed moves a staging item to the failed directory for later rescan.
func RouteToFailed(itemPath string, failedDir string, reason string) error {
	if err := os.MkdirAll(failedDir, 0755); err != nil {
		return fmt.Errorf("create failed dir: %w", err)
	}

	itemName := filepath.Base(itemPath)
	destPath := filepath.Join(failedDir, itemName)

	if err := os.Rename(itemPath, destPath); err != nil {
		// Cross-device: fall back to copy
		return copyDir(itemPath, destPath)
	}

	return nil
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, data, 0644); err != nil {
			return err
		}
	}
	os.RemoveAll(src)
	return nil
}
