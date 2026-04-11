package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
)

// OutlookConnector implements connector.Connector for Microsoft Graph mail.
// It polls configured mail folders for new messages and stages them for processing.
type OutlookConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	client       OutlookClient
	fetchCounter *connector.FetchCounter
}

// Poll iterates configured folders, fetches messages newer than the
// checkpointed receivedDateTime, extracts body content, and writes staging items.
func (c *OutlookConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	logger := slog.Default().With("component", "outlook-poll")

	for _, folderID := range c.config.FolderIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		result, matched := c.matcher.Match("folder:" + folderID)
		if !matched {
			logger.Debug("no rule for folder, skipping", "folder", folderID)
			continue
		}

		// Load checkpoint for this folder.
		cpKey := "receiveddatetime:" + folderID
		var checkpoint string
		if s, ok := cp.Load(cpKey); ok {
			checkpoint = s
		}

		messages, err := c.client.ListMessages(ctx, folderID, checkpoint, c.config.MaxResults)
		if err != nil {
			logger.Warn("list messages failed, skipping folder", "folder", folderID, "error", err)
			continue
		}

		var latestDateTime string
		for _, msg := range messages {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			status := c.fetchCounter.TryFetch(folderID)
			if status == connector.FetchPollLimit {
				return nil
			}
			if status == connector.FetchSourceLimit {
				break
			}

			// Convert HTML body to plain text if needed.
			bodyText := msg.BodyContent
			if msg.BodyContentType == "html" {
				bodyText = string(content.HTMLToText([]byte(msg.BodyContent)))
			}

			identity := &connector.Identity{
				Provider:   "microsoft",
				AuthMethod: "oauth",
			}

			ts, err := time.Parse(time.RFC3339, msg.ReceivedDateTime)
			if err != nil {
				logger.Warn("parse receivedDateTime failed, using current time",
					"folder", folderID, "id", msg.ID, "error", err)
				ts = time.Now()
			}

			item, err := c.writer.NewItem(connector.ItemOptions{
				Source:           "outlook",
				Sender:           msg.From,
				Subject:          msg.Subject,
				Timestamp:        ts,
				DestinationAgent: result.Destination,
				ContentType:      "text/plain",
				RuleTags:         result.Tags,
				Identity:         identity,
			})
			if err != nil {
				return fmt.Errorf("create staging item: %w", err)
			}

			if err := item.WriteContent([]byte(bodyText)); err != nil {
				return fmt.Errorf("write content: %w", err)
			}

			if err := item.Commit(); err != nil {
				return fmt.Errorf("commit staging item: %w", err)
			}

			if msg.ReceivedDateTime > latestDateTime {
				latestDateTime = msg.ReceivedDateTime
			}
		}

		if latestDateTime != "" {
			if err := cp.Save(cpKey, latestDateTime); err != nil {
				return fmt.Errorf("save checkpoint: %w", err)
			}
		}
	}

	return nil
}
