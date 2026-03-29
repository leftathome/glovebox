package routing

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

type QuarantineNotification struct {
	QuarantineID string   `json:"quarantine_id"`
	Source       string   `json:"source"`
	Sender       string   `json:"sender"`
	Subject      string   `json:"subject"`
	Timestamp    string   `json:"timestamp"`
	ContentLength int64   `json:"content_length"`
	Signals      []string `json:"signals"`
	TotalScore   float64  `json:"total_score"`
	QuarantinedAt string  `json:"quarantined_at"`
}

func WriteQuarantineNotification(item staging.StagingItem, scanResult engine.ScanResult, contentLength int64, notifyDir string) error {
	if err := os.MkdirAll(notifyDir, 0755); err != nil {
		return fmt.Errorf("create notification dir: %w", err)
	}

	signalNames := make([]string, len(scanResult.Signals))
	for i, s := range scanResult.Signals {
		signalNames[i] = s.Name
	}

	now := time.Now().UTC()
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(item.DirPath+now.String())))[:12]
	quarantineID := fmt.Sprintf("%s-%s", now.Format("20060102-150405"), hash)

	notification := QuarantineNotification{
		QuarantineID:  quarantineID,
		Source:        item.Metadata.Source,
		Sender:        item.Metadata.Sender,
		Subject:       item.Metadata.Subject,
		Timestamp:     item.Metadata.Timestamp.Format(time.RFC3339),
		ContentLength: contentLength,
		Signals:       signalNames,
		TotalScore:    scanResult.TotalScore,
		QuarantinedAt: now.Format(time.RFC3339),
	}

	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	filename := quarantineID + ".json"
	return os.WriteFile(filepath.Join(notifyDir, filename), data, 0644)
}
