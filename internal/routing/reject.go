package routing

import (
	"fmt"
	"os"
	"time"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/staging"
)

func RouteReject(itemPath string, reason string, metadata *staging.ItemMetadata, logger *audit.Logger) error {
	source := "unknown"
	sender := "unknown"
	destination := "unknown"
	if metadata != nil {
		source = metadata.Source
		sender = metadata.Sender
		destination = metadata.DestinationAgent
	}

	if err := logger.LogReject(audit.RejectEntry{
		AuditEntry: audit.AuditEntry{
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Source:      source,
			Sender:      sender,
			Verdict:     "reject",
			Destination: destination,
		},
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}

	os.RemoveAll(itemPath)
	return nil
}
