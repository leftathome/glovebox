package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
)

// TeamsConnector polls Microsoft Teams channels for new messages via
// the Microsoft Graph API.
type TeamsConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://graph.microsoft.com/v1.0" or test server URL
}

// graphMessagesResponse is the envelope returned by the Graph messages endpoint.
type graphMessagesResponse struct {
	Value []graphMessage `json:"value"`
}

// graphMessage represents a single channel message from the Graph API.
type graphMessage struct {
	ID              string    `json:"id"`
	CreatedDateTime string    `json:"createdDateTime"`
	From            graphFrom `json:"from"`
	Body            graphBody `json:"body"`
}

// graphFrom holds the sender identity of a message.
type graphFrom struct {
	User *graphUser `json:"user,omitempty"`
}

// graphUser holds display name and ID for a Teams user.
type graphUser struct {
	DisplayName string `json:"displayName"`
	ID          string `json:"id,omitempty"`
}

// graphBody holds the content and content type of a message body.
type graphBody struct {
	ContentType string `json:"contentType"` // "html" or "text"
	Content     string `json:"content"`
}

// stagedMessageContent is the JSON structure written as staged item content.
type stagedMessageContent struct {
	From            string `json:"from"`
	CreatedDateTime string `json:"createdDateTime"`
	Body            string `json:"body"`
}

func (c *TeamsConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, ch := range c.config.Channels {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollChannel(ctx, ch, checkpoint, logger); err != nil {
			logger.Warn("channel poll failed",
				"team_id", ch.TeamID,
				"channel_id", ch.ChannelID,
				"name", ch.Name,
				"error", err)
		}
	}
	return nil
}

func (c *TeamsConnector) pollChannel(ctx context.Context, ch ChannelConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	cpKey := "channel:" + ch.TeamID + ":" + ch.ChannelID

	// Build the messages URL with optional checkpoint filter.
	messagesURL := fmt.Sprintf("%s/teams/%s/channels/%s/messages",
		c.apiBase, ch.TeamID, ch.ChannelID)

	params := url.Values{}
	params.Set("$top", "25")

	lastDT, hasCheckpoint := checkpoint.Load(cpKey)
	if hasCheckpoint {
		params.Set("$filter", fmt.Sprintf("createdDateTime gt '%s'", lastDT))
	}

	fullURL := messagesURL + "?" + params.Encode()

	body, err := c.fetchAPI(ctx, fullURL)
	if err != nil {
		return fmt.Errorf("fetch messages for channel %s: %w", ch.Name, err)
	}

	var resp graphMessagesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse messages for channel %s: %w", ch.Name, err)
	}

	if len(resp.Value) == 0 {
		return nil
	}

	// Filter out messages at or before the checkpoint timestamp to prevent
	// duplicates when the API returns messages equal to the checkpoint.
	messages := resp.Value
	if hasCheckpoint {
		filtered := make([]graphMessage, 0, len(messages))
		for _, msg := range messages {
			if msg.CreatedDateTime > lastDT {
				filtered = append(filtered, msg)
			}
		}
		messages = filtered
	}

	if len(messages) == 0 {
		return nil
	}

	ruleKey := "channel:" + ch.Name
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for channel, skipping", "channel", ch.Name)
		return nil
	}

	// Track the latest createdDateTime seen so we can save it as the checkpoint.
	latestDT := lastDT

	for _, msg := range messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if status := c.fetchCounter.TryFetch(ch.Name); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "channel", ch.Name, "status", status)
			break
		}

		// Extract sender display name.
		sender := "unknown"
		if msg.From.User != nil && msg.From.User.DisplayName != "" {
			sender = msg.From.User.DisplayName
		}

		// Convert HTML body to plain text if needed.
		bodyText := msg.Body.Content
		if msg.Body.ContentType == "html" {
			bodyText = string(content.HTMLToText([]byte(bodyText)))
		}

		// Build the staged content payload.
		contentPayload := stagedMessageContent{
			From:            sender,
			CreatedDateTime: msg.CreatedDateTime,
			Body:            bodyText,
		}
		contentBytes, err := json.Marshal(contentPayload)
		if err != nil {
			return fmt.Errorf("marshal content: %w", err)
		}

		ts, _ := time.Parse(time.RFC3339, msg.CreatedDateTime)
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "teams",
			Sender:           sender,
			Subject:          fmt.Sprintf("Message from %s in %s", sender, ch.Name),
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "microsoft", AuthMethod: "oauth"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(contentBytes); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		// Track latest timestamp for checkpoint.
		if msg.CreatedDateTime > latestDT {
			latestDT = msg.CreatedDateTime
		}
	}

	// Save the latest timestamp as the checkpoint for this channel.
	if latestDT != lastDT {
		if err := checkpoint.Save(cpKey, latestDT); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *TeamsConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	const maxBody = 10 << 20 // 10 MB
	limited := io.LimitReader(resp.Body, maxBody)
	return io.ReadAll(limited)
}
