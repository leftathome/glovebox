package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
)

var entryTimeFormats = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC3339,
	time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05-07:00",
	"2006-01-02",
}

// RSSConnector polls RSS and Atom feeds and stages new entries.
type RSSConnector struct {
	config        Config
	writer        connector.StagingBackend
	matcher       *connector.RuleMatcher
	linkPolicy    *content.LinkPolicy
	httpClient    *http.Client
	fetchCounter  *connector.FetchCounter
	robotsChecker *connector.RobotsChecker
}

func (c *RSSConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, feed := range c.config.Feeds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollFeed(ctx, feed, checkpoint, logger); err != nil {
			logger.Warn("feed poll failed", "feed", feed.Name, "error", err)
			// Continue to next feed rather than aborting entirely.
		}
	}
	return nil
}

func (c *RSSConnector) pollFeed(ctx context.Context, feed FeedConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	body, err := c.fetchURL(ctx, feed.URL)
	if err != nil {
		return fmt.Errorf("fetch feed %s: %w", feed.Name, err)
	}

	entries, err := parseFeed(body)
	if err != nil {
		return fmt.Errorf("parse feed %s: %w", feed.Name, err)
	}

	if len(entries) == 0 {
		return nil
	}

	cpKey := "last:" + feed.Name
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// Determine which entries are new. Entries come oldest-first after
	// parseFeed reverses them. If we have a checkpoint, skip entries up
	// to and including the checkpointed ID.
	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, e := range entries {
			if e.ID == lastID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		} else {
			// Checkpoint ID not found in current feed -- process all
			// entries to avoid missing content.
			startIdx = 0
		}
	}

	ruleKey := "feed:" + feed.Name
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for feed, skipping", "feed", feed.Name)
		return nil
	}

	for i := startIdx; i < len(entries); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		status := c.fetchCounter.TryFetch(feed.Name)
		if status == connector.FetchPollLimit {
			return nil
		}
		if status == connector.FetchSourceLimit {
			break
		}

		entry := entries[i]

		body := buildEntryContent(entry)

		if c.config.FetchLinks && entry.Link != "" {
			linkText := c.fetchLinkedContent(ctx, entry.Link, logger)
			if linkText != "" {
				body = body + "\n\n--- Linked page ---\n\n" + linkText
			}
		}

		ts := parseEntryTime(entry.PubDate)

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "rss",
			Sender:           feed.Name,
			Subject:          entry.Title,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "text/plain",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "rss", AuthMethod: "none"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent([]byte(body)); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, entry.ID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

// parseFeed tries RSS first, then Atom. Returns entries oldest-first.
func parseFeed(data []byte) ([]feedEntry, error) {
	entries, err := parseRSS(data)
	if err == nil && len(entries) > 0 {
		return entries, nil
	}

	entries, err = parseAtom(data)
	if err == nil && len(entries) > 0 {
		return entries, nil
	}

	// If both parsers returned empty but no error, the feed is just empty.
	if err == nil {
		return nil, nil
	}
	return nil, fmt.Errorf("unable to parse feed as RSS or Atom: %w", err)
}

func parseRSS(data []byte) ([]feedEntry, error) {
	var feed rssFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}
	if feed.XMLName.Local != "rss" {
		return nil, fmt.Errorf("not an RSS feed")
	}

	entries := make([]feedEntry, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		id := item.GUID
		if id == "" {
			id = item.Link
		}
		entries = append(entries, feedEntry{
			ID:      id,
			Title:   item.Title,
			Link:    item.Link,
			Content: item.Description,
			PubDate: item.PubDate,
		})
	}

	// RSS feeds are typically newest-first. Reverse to process oldest first.
	slices.Reverse(entries)
	return entries, nil
}

func parseAtom(data []byte) ([]feedEntry, error) {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}
	if feed.XMLName.Local != "feed" {
		return nil, fmt.Errorf("not an Atom feed")
	}

	entries := make([]feedEntry, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		id := e.ID
		if id == "" {
			id = e.Link.Href
		}
		body := e.Content.Body
		if body == "" {
			body = e.Summary
		}
		pubDate := e.Published
		if pubDate == "" {
			pubDate = e.Updated
		}
		entries = append(entries, feedEntry{
			ID:      id,
			Title:   e.Title,
			Link:    e.Link.Href,
			Content: body,
			PubDate: pubDate,
		})
	}

	// Atom feeds are also typically newest-first.
	slices.Reverse(entries)
	return entries, nil
}


func buildEntryContent(entry feedEntry) string {
	var sb strings.Builder
	if entry.Title != "" {
		sb.WriteString(entry.Title)
		sb.WriteString("\n\n")
	}
	if entry.Content != "" {
		// Strip HTML tags from feed content.
		cleaned := content.HTMLToText([]byte(entry.Content))
		sb.Write(cleaned)
	}
	if entry.Link != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("Link: ")
		sb.WriteString(entry.Link)
	}
	return sb.String()
}

func (c *RSSConnector) fetchLinkedContent(ctx context.Context, rawURL string, logger *slog.Logger) string {
	allowed, reason := c.linkPolicy.Check(rawURL)
	if !allowed {
		logger.Debug("link fetch denied by policy", "url", rawURL, "reason", reason)
		return ""
	}

	if c.robotsChecker != nil && !c.robotsChecker.Allowed(ctx, rawURL) {
		logger.Debug("link fetch denied by robots.txt", "url", rawURL)
		return ""
	}

	body, err := c.fetchURLWithLimit(ctx, rawURL, 1<<20) // 1 MB limit
	if err != nil {
		logger.Debug("link fetch failed", "url", rawURL, "error", err)
		return ""
	}

	text := content.HTMLToText(body)
	return string(text)
}

func (c *RSSConnector) fetchURL(ctx context.Context, url string) ([]byte, error) {
	return c.fetchURLWithLimit(ctx, url, 10<<20) // 10 MB limit for feeds
}

func (c *RSSConnector) fetchURLWithLimit(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	limited := io.LimitReader(resp.Body, maxBytes)
	return io.ReadAll(limited)
}

func parseEntryTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now().UTC()
	}

	for _, layout := range entryTimeFormats {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}

	return time.Now().UTC()
}
