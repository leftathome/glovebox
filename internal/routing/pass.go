package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

func RoutePass(item staging.StagingItem, scanResult engine.ScanResult, destDir string, logger *audit.Logger, scanDuration time.Duration) error {
	itemID := filepath.Base(item.DirPath)
	deliveryDir := filepath.Join(destDir, itemID)

	if err := os.MkdirAll(deliveryDir, 0755); err != nil {
		return fmt.Errorf("create delivery dir: %w", err)
	}

	content, err := os.ReadFile(item.ContentPath)
	if err != nil {
		return fmt.Errorf("read content: %w", err)
	}

	if err := os.WriteFile(filepath.Join(deliveryDir, "content.raw"), content, 0644); err != nil {
		return fmt.Errorf("write content to inbox: %w", err)
	}

	metaSrc := filepath.Join(item.DirPath, "metadata.json")
	metaData, err := os.ReadFile(metaSrc)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(deliveryDir, "metadata.json"), metaData, 0644); err != nil {
		return fmt.Errorf("write metadata to inbox: %w", err)
	}

	hash := contentHash(content)

	if err := logger.LogPass(audit.PassEntry{AuditEntry: audit.AuditEntry{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Source:         item.Metadata.Source,
		Sender:         item.Metadata.Sender,
		ContentHash:    hash,
		ContentLength:  int64(len(content)),
		Signals:        scanResult.Signals,
		TotalScore:     scanResult.TotalScore,
		Verdict:        string(scanResult.Verdict),
		Destination:    item.Metadata.DestinationAgent,
		ScanDurationMs: scanDuration.Milliseconds(),
	}}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}

	os.RemoveAll(item.DirPath)
	return nil
}
