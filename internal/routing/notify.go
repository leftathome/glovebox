package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

type QuarantineNotification struct {
	QuarantineID  string   `json:"quarantine_id"`
	Source        string   `json:"source"`
	Sender        string   `json:"sender"`
	Subject       string   `json:"subject"`
	Timestamp     string   `json:"timestamp"`
	ContentLength int64    `json:"content_length"`
	Signals       []string `json:"signals"`
	TotalScore    float64  `json:"total_score"`
	QuarantinedAt string   `json:"quarantined_at"`
}

func WriteQuarantineNotification(quarantineID string, item staging.StagingItem, scanResult engine.ScanResult, contentLength int64, notifyDir string) error {
	if err := os.MkdirAll(notifyDir, 0755); err != nil {
		return fmt.Errorf("create notification dir: %w", err)
	}

	signalNames := make([]string, len(scanResult.Signals))
	for i, s := range scanResult.Signals {
		signalNames[i] = s.Name
	}

	notification := QuarantineNotification{
		QuarantineID:  quarantineID,
		Source:        item.Metadata.Source,
		Sender:        item.Metadata.Sender,
		Subject:       item.Metadata.Subject,
		Timestamp:     item.Metadata.Timestamp.Format(time.RFC3339),
		ContentLength: contentLength,
		Signals:       signalNames,
		TotalScore:    scanResult.TotalScore,
		QuarantinedAt: time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	filename := quarantineID + ".json"
	return os.WriteFile(filepath.Join(notifyDir, filename), data, 0644)
}
