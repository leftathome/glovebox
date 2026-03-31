package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
)

// GmailConnector implements connector.Connector for the Gmail API.
// It polls configured labels for new messages and stages them for processing.
type GmailConnector struct {
	config       Config
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	client       GmailClient
	fetchCounter *connector.FetchCounter
}

// Poll iterates configured labels, fetches messages newer than the
// checkpointed internalDate, decodes MIME content, and writes staging items.
func (c *GmailConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	logger := slog.Default().With("component", "gmail-poll")

	for _, labelID := range c.config.LabelIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		result, matched := c.matcher.Match("label:" + labelID)
		if !matched {
			logger.Debug("no rule for label, skipping", "label", labelID)
			continue
		}

		// Build query with checkpoint-based dedup.
		var query string
		cpKey := "internaldate:" + labelID
		if s, ok := cp.Load(cpKey); ok {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil {
				// Gmail after: uses epoch seconds.
				epochSec := v / 1000
				query = fmt.Sprintf("after:%d", epochSec)
			}
		}

		msgIDs, err := c.client.ListMessages(ctx, labelID, query, c.config.MaxResults)
		if err != nil {
			logger.Warn("list messages failed, skipping label", "label", labelID, "error", err)
			continue
		}

		var latestDate int64
		for _, msgID := range msgIDs {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			status := c.fetchCounter.TryFetch(labelID)
			if status == connector.FetchPollLimit {
				return nil
			}
			if status == connector.FetchSourceLimit {
				break
			}

			msg, err := c.client.GetMessage(ctx, msgID)
			if err != nil {
				logger.Warn("get message failed, skipping", "label", labelID, "id", msgID, "error", err)
				continue
			}

			parts, err := content.DecodeMIME(msg.Raw)
			if err != nil {
				logger.Warn("MIME decode failed, skipping message", "label", labelID, "id", msgID, "error", err)
				continue
			}

			var body bytes.Buffer
			for _, p := range parts {
				if strings.HasPrefix(p.ContentType, "text/") {
					body.Write(p.Body)
				}
			}

			identity := &connector.Identity{
				Provider:   "google",
				AuthMethod: "oauth",
			}

			ts := time.UnixMilli(msg.InternalDate)

			item, err := c.writer.NewItem(connector.ItemOptions{
				Source:           "gmail",
				Sender:           msg.Sender,
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

			if err := item.WriteContent(body.Bytes()); err != nil {
				return fmt.Errorf("write content: %w", err)
			}

			if err := item.Commit(); err != nil {
				return fmt.Errorf("commit staging item: %w", err)
			}

			if msg.InternalDate > latestDate {
				latestDate = msg.InternalDate
			}
		}

		if latestDate > 0 {
			if err := cp.Save(cpKey, strconv.FormatInt(latestDate, 10)); err != nil {
				return fmt.Errorf("save checkpoint: %w", err)
			}
		}
	}

	return nil
}
