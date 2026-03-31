package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const defaultBaseURL = "https://api.trello.com"

// TrelloConnector polls Trello boards for recent actions and stages them.
type TrelloConnector struct {
	config     Config
	apiKey     string
	token      string
	writer     *connector.StagingWriter
	matcher    *connector.RuleMatcher
	httpClient *http.Client
	baseURL    string
}

// trelloAction represents a single action from the Trello API response.
type trelloAction struct {
	ID            string                 `json:"id"`
	Type          string                 `json:"type"`
	Date          string                 `json:"date"`
	Data          map[string]interface{} `json:"data"`
	MemberCreator map[string]interface{} `json:"memberCreator"`
}

func (c *TrelloConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, board := range c.config.Boards {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollBoard(ctx, board, checkpoint, logger); err != nil {
			logger.Warn("board poll failed", "board", board.Name, "error", err)
			// Continue to next board rather than aborting entirely.
		}
	}
	return nil
}

func (c *TrelloConnector) pollBoard(ctx context.Context, board BoardConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	actions, err := c.fetchActions(ctx, board.ID)
	if err != nil {
		return fmt.Errorf("fetch actions for board %s: %w", board.Name, err)
	}

	if len(actions) == 0 {
		return nil
	}

	// Trello returns actions newest-first. Reverse to process oldest first.
	slices.Reverse(actions)

	cpKey := "action:" + board.ID
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// Determine which actions are new.
	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, a := range actions {
			if a.ID == lastID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		}
	}

	ruleKey := "board:" + board.Name
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for board, skipping", "board", board.Name)
		return nil
	}

	for i := startIdx; i < len(actions); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		action := actions[i]

		// Serialize the action as JSON content.
		body, err := json.Marshal(action)
		if err != nil {
			return fmt.Errorf("marshal action: %w", err)
		}

		ts := parseActionTime(action.Date)

		// Extract card name for subject if available.
		subject := action.Type
		if data, ok := action.Data["card"]; ok {
			if card, ok := data.(map[string]interface{}); ok {
				if name, ok := card["name"].(string); ok {
					subject = name
				}
			}
		}

		// Extract member name for sender if available.
		sender := board.Name
		if mc := action.MemberCreator; mc != nil {
			if name, ok := mc["fullName"].(string); ok {
				sender = name
			}
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "trello",
			Sender:           sender,
			Subject:          subject,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "trello", AuthMethod: "api_key"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(body); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, action.ID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *TrelloConnector) fetchActions(ctx context.Context, boardID string) ([]trelloAction, error) {
	base := c.baseURL
	if base == "" {
		base = defaultBaseURL
	}

	url := fmt.Sprintf("%s/1/boards/%s/actions?key=%s&token=%s",
		base, boardID, c.apiKey, c.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "glovebox-trello/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Trello API", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 10<<20) // 10 MB limit
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}

	var actions []trelloAction
	if err := json.Unmarshal(data, &actions); err != nil {
		return nil, fmt.Errorf("parse actions: %w", err)
	}

	return actions, nil
}

func parseActionTime(raw string) time.Time {
	if raw == "" {
		return time.Now().UTC()
	}

	// Trello uses ISO 8601 / RFC 3339 format.
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// Try with milliseconds variant.
		t, err = time.Parse("2006-01-02T15:04:05.000Z", raw)
		if err != nil {
			return time.Now().UTC()
		}
	}
	return t.UTC()
}
