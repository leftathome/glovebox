package routing

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/staging"
)

type PendingInfo struct {
	Status     string `json:"status"`
	Source     string `json:"source"`
	Sender     string `json:"sender"`
	Subject    string `json:"subject"`
	Timestamp  string `json:"timestamp"`
	ReceivedAt string `json:"received_at"`
}

func PendingFilename(itemID string) string {
	return itemID + ".pending.json"
}

func WritePending(item staging.StagingItem, agentInboxDir string) error {
	if !item.Metadata.Ordered {
		return nil
	}

	info := PendingInfo{
		Status:     "scanning",
		Source:     item.Metadata.Source,
		Sender:     item.Metadata.Sender,
		Subject:    item.Metadata.Subject,
		Timestamp:  item.Metadata.Timestamp.Format(time.RFC3339),
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal pending info: %w", err)
	}

	itemID := filepath.Base(item.DirPath)
	path := filepath.Join(agentInboxDir, PendingFilename(itemID))

	if err := os.MkdirAll(agentInboxDir, 0755); err != nil {
		return fmt.Errorf("create inbox dir: %w", err)
	}

	return os.WriteFile(path, data, 0644)
}

func RemovePending(itemID string, agentInboxDir string) {
	path := filepath.Join(agentInboxDir, PendingFilename(itemID))
	os.Remove(path)
}

func CleanStalePending(agentsDir string, agentNames []string) {
	for _, agent := range agentNames {
		inboxDir := filepath.Join(agentsDir, agent, "workspace", "inbox")
		entries, err := os.ReadDir(inboxDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".pending.json") {
				path := filepath.Join(inboxDir, e.Name())
				log.Printf("removing stale pending file: %s", path)
				os.Remove(path)
			}
		}
	}
}
