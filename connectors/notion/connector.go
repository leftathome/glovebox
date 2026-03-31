package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// NotionConnector polls Notion databases and pages for updated content.
type NotionConnector struct {
	config       Config
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://api.notion.com" or test server URL
}

// notionDatabaseQueryResponse represents the response from a database query.
type notionDatabaseQueryResponse struct {
	Results []notionPage `json:"results"`
	HasMore bool         `json:"has_more"`
}

// notionPage represents a page object returned by the Notion API.
type notionPage struct {
	ID             string                            `json:"id"`
	LastEditedTime string                            `json:"last_edited_time"`
	Properties     map[string]json.RawMessage        `json:"properties"`
}

// notionBlocksResponse represents the response from blocks children endpoint.
type notionBlocksResponse struct {
	Results []notionBlock `json:"results"`
	HasMore bool          `json:"has_more"`
}

// notionBlock represents a block object from the Notion API.
type notionBlock struct {
	Type             string          `json:"type"`
	Paragraph        *richTextBlock  `json:"paragraph,omitempty"`
	Heading1         *richTextBlock  `json:"heading_1,omitempty"`
	Heading2         *richTextBlock  `json:"heading_2,omitempty"`
	Heading3         *richTextBlock  `json:"heading_3,omitempty"`
	BulletedListItem *richTextBlock  `json:"bulleted_list_item,omitempty"`
	NumberedListItem *richTextBlock  `json:"numbered_list_item,omitempty"`
}

// richTextBlock holds the rich_text array common to text-bearing blocks.
type richTextBlock struct {
	RichText []richTextEntry `json:"rich_text"`
}

// richTextEntry represents a single rich text element.
type richTextEntry struct {
	PlainText string `json:"plain_text"`
}

func (c *NotionConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, dbID := range c.config.DatabaseIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollDatabase(ctx, dbID, checkpoint, logger); err != nil {
			logger.Warn("database poll failed", "database_id", dbID, "error", err)
		}
	}

	for _, pageID := range c.config.PageIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollPage(ctx, pageID, checkpoint, logger); err != nil {
			logger.Warn("page poll failed", "page_id", pageID, "error", err)
		}
	}

	return nil
}

func (c *NotionConnector) pollDatabase(ctx context.Context, dbID string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	cpKey := "database:" + dbID
	ruleKey := "database:" + dbID

	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for database, skipping", "database_id", dbID)
		return nil
	}

	// Build query filter. If we have a checkpoint (last_edited_time), filter
	// for pages edited after that timestamp.
	filterBody := map[string]interface{}{}
	lastEdited, hasCheckpoint := checkpoint.Load(cpKey)
	if hasCheckpoint && lastEdited != "" {
		filterBody["filter"] = map[string]interface{}{
			"timestamp":       "last_edited_time",
			"last_edited_time": map[string]string{"after": lastEdited},
		}
	}

	bodyBytes, err := json.Marshal(filterBody)
	if err != nil {
		return fmt.Errorf("marshal query body: %w", err)
	}

	url := fmt.Sprintf("%s/v1/databases/%s/query", c.apiBase, dbID)
	respBody, err := c.fetchAPIPost(ctx, url, bodyBytes)
	if err != nil {
		return fmt.Errorf("query database %s: %w", dbID, err)
	}

	var queryResp notionDatabaseQueryResponse
	if err := json.Unmarshal(respBody, &queryResp); err != nil {
		return fmt.Errorf("parse database query response: %w", err)
	}

	if len(queryResp.Results) == 0 {
		return nil
	}

	var latestTime string
	for _, page := range queryResp.Results {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Client-side dedup: skip pages at or before the checkpoint timestamp.
		// The API filter uses "after" (exclusive), but we guard against servers
		// or mocks that return stale results anyway.
		if hasCheckpoint && lastEdited != "" && page.LastEditedTime <= lastEdited {
			continue
		}

		if status := c.fetchCounter.TryFetch(dbID); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "database_id", dbID, "status", status)
			break
		}

		// Extract a title from properties if available.
		subject := "Page " + page.ID + " in database " + dbID

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "notion",
			Sender:           "database:" + dbID,
			Subject:          subject,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "text/plain",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "notion", AuthMethod: "api_key"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		// Write the raw page JSON as content.
		pageJSON, err := json.Marshal(page)
		if err != nil {
			return fmt.Errorf("marshal page: %w", err)
		}

		if err := item.WriteContent(pageJSON); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		// Track the latest edited time for checkpointing.
		if page.LastEditedTime > latestTime {
			latestTime = page.LastEditedTime
		}
	}

	if latestTime != "" {
		if err := checkpoint.Save(cpKey, latestTime); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *NotionConnector) pollPage(ctx context.Context, pageID string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	ruleKey := "page:" + pageID

	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for page, skipping", "page_id", pageID)
		return nil
	}

	if status := c.fetchCounter.TryFetch(pageID); !status.Allowed() {
		logger.Info("fetch limit reached, stopping", "page_id", pageID, "status", status)
		return nil
	}

	url := fmt.Sprintf("%s/v1/blocks/%s/children", c.apiBase, pageID)
	respBody, err := c.fetchAPIGet(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch page blocks %s: %w", pageID, err)
	}

	var blocksResp notionBlocksResponse
	if err := json.Unmarshal(respBody, &blocksResp); err != nil {
		return fmt.Errorf("parse blocks response: %w", err)
	}

	// Extract text content from supported block types.
	textContent := extractTextFromBlocks(blocksResp.Results)

	// Check if content has changed via checkpoint.
	cpKey := "page:" + pageID
	lastContent, hasCheckpoint := checkpoint.Load(cpKey)
	if hasCheckpoint && lastContent == textContent {
		// Content unchanged, skip staging.
		return nil
	}

	item, err := c.writer.NewItem(connector.ItemOptions{
		Source:           "notion",
		Sender:           "page:" + pageID,
		Subject:          "Page " + pageID,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: result.Destination,
		ContentType:      "text/plain",
		RuleTags:         result.Tags,
		Identity:         &connector.Identity{Provider: "notion", AuthMethod: "api_key"},
	})
	if err != nil {
		return fmt.Errorf("new staging item: %w", err)
	}

	if err := item.WriteContent([]byte(textContent)); err != nil {
		return fmt.Errorf("write content: %w", err)
	}

	if err := item.Commit(); err != nil {
		return fmt.Errorf("commit item: %w", err)
	}

	if err := checkpoint.Save(cpKey, textContent); err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}

	return nil
}

// extractTextFromBlocks concatenates plain text from supported block types.
func extractTextFromBlocks(blocks []notionBlock) string {
	var parts []string
	for _, block := range blocks {
		var rtb *richTextBlock
		switch block.Type {
		case "paragraph":
			rtb = block.Paragraph
		case "heading_1":
			rtb = block.Heading1
		case "heading_2":
			rtb = block.Heading2
		case "heading_3":
			rtb = block.Heading3
		case "bulleted_list_item":
			rtb = block.BulletedListItem
		case "numbered_list_item":
			rtb = block.NumberedListItem
		default:
			continue
		}
		if rtb == nil {
			continue
		}
		for _, rt := range rtb.RichText {
			if rt.PlainText != "" {
				parts = append(parts, rt.PlainText)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func (c *NotionConnector) fetchAPIPost(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return c.doRequest(req)
}

func (c *NotionConnector) fetchAPIGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.doRequest(req)
}

func (c *NotionConnector) doRequest(req *http.Request) ([]byte, error) {
	token, err := c.tokenSource.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Notion-Version", "2022-06-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.String())
	}

	const maxBody = 10 << 20 // 10 MB
	limited := io.LimitReader(resp.Body, maxBody)
	return io.ReadAll(limited)
}
