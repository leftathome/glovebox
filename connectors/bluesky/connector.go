package main

import (
	"bytes"
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

const maxResponseBytes = 10 << 20 // 10 MB

// BlueskyConnector polls Bluesky (AT Protocol) feeds and stages new posts.
type BlueskyConnector struct {
	config      Config
	identifier  string
	appPassword string
	writer      *connector.StagingWriter
	matcher     *connector.RuleMatcher
	httpClient  *http.Client
}

// sessionResponse is the JSON returned by com.atproto.server.createSession.
type sessionResponse struct {
	DID        string `json:"did"`
	Handle     string `json:"handle"`
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
}

// feedResponse is the JSON returned by app.bsky.feed.getAuthorFeed.
type feedResponse struct {
	Feed []feedItem `json:"feed"`
}

// feedItem wraps a post in the feed response.
type feedItem struct {
	Post postView `json:"post"`
}

// postView represents a post object in the AT Protocol feed.
type postView struct {
	URI       string          `json:"uri"`
	CID       string          `json:"cid"`
	Author    authorView      `json:"author"`
	Record    json.RawMessage `json:"record"`
	IndexedAt string          `json:"indexedAt"`
}

// authorView represents the author of a post.
type authorView struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
}

// postRecord holds the text and timestamp from the post record.
type postRecord struct {
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

func (c *BlueskyConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	// Step 1: Create a session to get an access token and DID.
	session, err := c.createSession(ctx)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	logger.Info("bluesky session created", "did", session.DID)

	// Step 2: Fetch the author feed.
	posts, err := c.fetchAuthorFeed(ctx, session)
	if err != nil {
		return fmt.Errorf("fetch author feed: %w", err)
	}

	if len(posts) == 0 {
		return nil
	}

	// Posts come newest-first from the API. Reverse to process oldest first.
	slices.Reverse(posts)

	// Determine which posts are new using checkpoint.
	cpKey := "post:latest"
	lastCID, hasCheckpoint := checkpoint.Load(cpKey)

	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, p := range posts {
			if p.Post.CID == lastCID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		}
	}

	// Match against rules.
	ruleKey := "feed:timeline"
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for feed:timeline, skipping")
		return nil
	}

	for i := startIdx; i < len(posts); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		post := posts[i]

		// Parse the record to extract text.
		var rec postRecord
		if err := json.Unmarshal(post.Post.Record, &rec); err != nil {
			logger.Warn("failed to parse post record", "cid", post.Post.CID, "error", err)
			continue
		}

		// Serialize the full post body as JSON content.
		postJSON, err := json.Marshal(post.Post)
		if err != nil {
			logger.Warn("failed to marshal post", "cid", post.Post.CID, "error", err)
			continue
		}

		ts := parsePostTime(post.Post.IndexedAt)

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "bluesky",
			Sender:           post.Post.Author.Handle,
			Subject:          truncateSubject(rec.Text),
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity: &connector.Identity{
				Provider:   "bluesky",
				AuthMethod: "app_password",
				AccountID:  c.identifier,
			},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(postJSON); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, post.Post.CID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

// createSession authenticates with the AT Protocol PDS and returns session info.
func (c *BlueskyConnector) createSession(ctx context.Context) (*sessionResponse, error) {
	reqBody, err := json.Marshal(map[string]string{
		"identifier": c.identifier,
		"password":   c.appPassword,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal session request: %w", err)
	}

	url := c.config.Service + "/xrpc/com.atproto.server.createSession"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read session response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("session request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var session sessionResponse
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("parse session response: %w", err)
	}

	return &session, nil
}

// fetchAuthorFeed retrieves the authenticated user's feed.
func (c *BlueskyConnector) fetchAuthorFeed(ctx context.Context, session *sessionResponse) ([]feedItem, error) {
	url := fmt.Sprintf("%s/xrpc/app.bsky.feed.getAuthorFeed?actor=%s", c.config.Service, session.DID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+session.AccessJwt)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read feed response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var feed feedResponse
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("parse feed response: %w", err)
	}

	return feed.Feed, nil
}

// parsePostTime parses an ISO 8601 timestamp from the AT Protocol.
func parsePostTime(raw string) time.Time {
	if raw == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Now().UTC()
	}
	return t.UTC()
}

// truncateSubject returns the first 100 characters of text for use as a subject line.
func truncateSubject(text string) string {
	runes := []rune(text)
	if len(runes) <= 100 {
		return text
	}
	return string(runes[:100]) + "..."
}
