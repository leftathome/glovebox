package routing

import (
	"fmt"
	"os"
	"time"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

func RouteReject(itemPath string, reason string, metadata *staging.ItemMetadata, logger *audit.Logger) error {
	source := "unknown"
	sender := "unknown"
	destination := "unknown"
	var dataSubject string
	var audience []string
	if metadata != nil {
		source = metadata.Source
		sender = metadata.Sender
		destination = metadata.DestinationAgent
		dataSubject = metadata.DataSubject
		audience = metadata.Audience
	}

	if err := logger.LogReject(audit.RejectEntry{
		AuditEntry: audit.AuditEntry{
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Source:      source,
			Sender:      sender,
			Verdict:     string(engine.VerdictReject),
			Destination: destination,
			DataSubject: dataSubject,
			Audience:    audience,
		},
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}

	os.RemoveAll(itemPath)
	return nil
}
