package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

type QuarantineMetadata struct {
	Source           string          `json:"source"`
	Sender           string          `json:"sender"`
	Subject          string          `json:"subject"`
	Timestamp        string          `json:"timestamp"`
	DestinationAgent string          `json:"destination_agent"`
	ContentType      string          `json:"content_type"`
	ContentHash      string          `json:"content_hash"`
	Signals          []engine.Signal `json:"signals"`
	TotalScore       float64         `json:"total_score"`
	Threshold        float64         `json:"threshold"`
	ScanDurationMs   int64           `json:"scan_duration_ms"`
	Reason           string          `json:"reason"`
}

func RouteQuarantine(item staging.StagingItem, scanResult engine.ScanResult, quarantineDir string, notifyDir string, logger *audit.Logger, threshold float64, scanDuration time.Duration, reason string) error {
	content, err := os.ReadFile(item.ContentPath)
	if err != nil {
		return fmt.Errorf("read content: %w", err)
	}

	hash := contentHash(content)
	now := time.Now().UTC()
	qID := fmt.Sprintf("%s-%s", now.Format("20060102-150405"), hash[:12])
	qDir := filepath.Join(quarantineDir, qID)

	if err := os.MkdirAll(qDir, 0755); err != nil {
		return fmt.Errorf("create quarantine dir: %w", err)
	}

	sanitized := SanitizeContent(content)
	if err := os.WriteFile(filepath.Join(qDir, "content.sanitized"), sanitized, 0644); err != nil {
		return fmt.Errorf("write sanitized content: %w", err)
	}

	qMeta := QuarantineMetadata{
		Source:           item.Metadata.Source,
		Sender:           item.Metadata.Sender,
		Subject:          item.Metadata.Subject,
		Timestamp:        item.Metadata.Timestamp.Format(time.RFC3339),
		DestinationAgent: item.Metadata.DestinationAgent,
		ContentType:      item.Metadata.ContentType,
		ContentHash:      hash,
		Signals:          scanResult.Signals,
		TotalScore:       scanResult.TotalScore,
		Threshold:        threshold,
		ScanDurationMs:   scanDuration.Milliseconds(),
		Reason:           reason,
	}
	metaData, err := json.MarshalIndent(qMeta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal quarantine metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(qDir, "metadata.json"), metaData, 0644); err != nil {
		return fmt.Errorf("write quarantine metadata: %w", err)
	}

	if err := WriteQuarantineNotification(qID, item, scanResult, int64(len(content)), notifyDir); err != nil {
		return fmt.Errorf("write notification: %w", err)
	}

	if err := logger.LogReject(audit.RejectEntry{
		AuditEntry: audit.AuditEntry{
			Timestamp:      now.Format(time.RFC3339),
			Source:         item.Metadata.Source,
			Sender:         item.Metadata.Sender,
			ContentHash:    hash,
			ContentLength:  int64(len(content)),
			Signals:        scanResult.Signals,
			TotalScore:     scanResult.TotalScore,
			Verdict:        string(engine.VerdictQuarantine),
			Destination:    item.Metadata.DestinationAgent,
			ScanDurationMs: scanDuration.Milliseconds(),
			DataSubject:    item.Metadata.DataSubject,
			Audience:       item.Metadata.Audience,
		},
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}

	os.RemoveAll(item.DirPath)
	return nil
}
