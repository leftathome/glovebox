package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
)

// IMAPConnector implements connector.Connector and connector.Watcher.
// It polls IMAP folders for new messages and stages them for processing.
type IMAPConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	imapUsername  string
	newClient    func() IMAPClient
	fetchCounter *connector.FetchCounter
}

// Poll iterates configured folders, fetches messages newer than the
// checkpointed UID, decodes MIME content, and writes staging items.
func (c *IMAPConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	logger := slog.Default().With("component", "imap-poll")

	client := c.newClient()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("imap connect: %w", err)
	}
	defer client.Close()

	for _, folder := range c.config.Folders {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := client.SelectFolder(ctx, folder.Name); err != nil {
			logger.Warn("select folder failed, skipping", "folder", folder.Name, "error", err)
			continue
		}

		var lastUID uint32
		cpKey := "uid:" + folder.Name
		if s, ok := cp.Load(cpKey); ok {
			if v, err := strconv.ParseUint(s, 10, 32); err == nil {
				lastUID = uint32(v)
			}
		}

		result, matched := c.matcher.Match("folder:" + folder.Name)
		if !matched {
			logger.Debug("no rule for folder, skipping", "folder", folder.Name)
			continue
		}

		uids, err := client.SearchSinceUID(ctx, lastUID)
		if err != nil {
			logger.Warn("search failed, skipping folder", "folder", folder.Name, "error", err)
			continue
		}

		for _, uid := range uids {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			status := c.fetchCounter.TryFetch(folder.Name)
			if status == connector.FetchPollLimit {
				return nil
			}
			if status == connector.FetchSourceLimit {
				break
			}

			raw, sender, subject, date, err := client.FetchMessage(ctx, uid)
			if err != nil {
				logger.Warn("fetch failed, skipping message", "folder", folder.Name, "uid", uid, "error", err)
				continue
			}

			parts, err := content.DecodeMIME(raw)
			if err != nil {
				logger.Warn("MIME decode failed, skipping message", "folder", folder.Name, "uid", uid, "error", err)
				continue
			}

			var body bytes.Buffer
			for _, p := range parts {
				if strings.HasPrefix(p.ContentType, "text/") {
					body.Write(p.Body)
				}
			}

			var identity *connector.Identity
			if c.imapUsername != "" {
				identity = &connector.Identity{
					AccountID:  c.imapUsername,
					Provider:   "imap",
					AuthMethod: "app_password",
				}
			}

			item, err := c.writer.NewItem(connector.ItemOptions{
				Source:           "imap",
				Sender:           sender,
				Subject:          subject,
				Timestamp:        date,
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

			if err := cp.Save(cpKey, strconv.FormatUint(uint64(uid), 10)); err != nil {
				return fmt.Errorf("save checkpoint: %w", err)
			}
		}
	}

	return nil
}

// Watch opens an IDLE connection on the first configured folder and blocks
// until new mail arrives or the context is cancelled. The runner will
// re-poll after Watch returns.
func (c *IMAPConnector) Watch(ctx context.Context, cp connector.Checkpoint) error {
	if len(c.config.Folders) == 0 {
		return connector.PermanentError(fmt.Errorf("no folders configured for watch"))
	}

	client := c.newClient()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("imap connect: %w", err)
	}
	defer client.Close()

	if err := client.SelectFolder(ctx, c.config.Folders[0].Name); err != nil {
		return fmt.Errorf("select folder for idle: %w", err)
	}

	return client.Idle(ctx)
}
