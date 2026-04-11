package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const defaultBaseURL = "https://hacker-news.firebaseio.com"

// HNConnector polls the Hacker News API and stages stories.
type HNConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	httpClient   *http.Client
	fetchCounter *connector.FetchCounter
	baseURL      string // overridden in tests
}

func (c *HNConnector) effectiveBaseURL() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return defaultBaseURL
}

func (c *HNConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, feed := range c.config.Feeds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollFeed(ctx, feed, checkpoint, logger); err != nil {
			logger.Warn("feed poll failed", "feed", feed, "error", err)
			// Continue to next feed rather than aborting entirely.
		}
	}
	return nil
}

func (c *HNConnector) pollFeed(ctx context.Context, feed string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	// Fetch the list of story IDs for this feed.
	storyIDs, err := c.fetchStoryIDs(ctx, feed)
	if err != nil {
		return fmt.Errorf("fetch %s story IDs: %w", feed, err)
	}

	if len(storyIDs) == 0 {
		return nil
	}

	cpKey := "last:" + feed
	lastIDStr, hasCheckpoint := checkpoint.Load(cpKey)
	lastID := 0
	if hasCheckpoint {
		lastID, _ = strconv.Atoi(lastIDStr)
	}

	ruleKey := "feed:" + feed
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for feed, skipping", "feed", feed)
		return nil
	}

	highestID := lastID

	for _, storyID := range storyIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip stories at or below the checkpoint.
		if hasCheckpoint && storyID <= lastID {
			continue
		}

		status := c.fetchCounter.TryFetch(feed)
		if status == connector.FetchPollLimit {
			return nil
		}
		if status == connector.FetchSourceLimit {
			break
		}

		story, err := c.fetchItem(ctx, storyID)
		if err != nil {
			logger.Warn("fetch story failed", "id", storyID, "error", err)
			continue
		}

		// Build content with optional comments.
		sc := storyContent{
			Title: story.Title,
			URL:   story.URL,
			Text:  story.Text,
			Score: story.Score,
		}

		if c.config.FollowComments && len(story.Kids) > 0 {
			sc.Comments = c.fetchComments(ctx, story.Kids, logger)
		}

		contentBytes, err := json.Marshal(sc)
		if err != nil {
			return fmt.Errorf("marshal story content: %w", err)
		}

		ts := time.Unix(story.Time, 0).UTC()

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "hackernews",
			Sender:           feed,
			Subject:          story.Title,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "hackernews", AuthMethod: "none"},
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

		if storyID > highestID {
			highestID = storyID
		}
	}

	// Save checkpoint as the highest ID we processed.
	if highestID > lastID {
		if err := checkpoint.Save(cpKey, strconv.Itoa(highestID)); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

// fetchStoryIDs fetches the list of story IDs for a feed type.
func (c *HNConnector) fetchStoryIDs(ctx context.Context, feed string) ([]int, error) {
	url := fmt.Sprintf("%s/v0/%sstories.json", c.effectiveBaseURL(), feed)
	body, err := c.fetchURL(ctx, url)
	if err != nil {
		return nil, err
	}

	var ids []int
	if err := json.Unmarshal(body, &ids); err != nil {
		return nil, fmt.Errorf("parse story IDs: %w", err)
	}
	return ids, nil
}

// fetchItem fetches a single item (story or comment) by ID.
func (c *HNConnector) fetchItem(ctx context.Context, id int) (*hnStory, error) {
	url := fmt.Sprintf("%s/v0/item/%d.json", c.effectiveBaseURL(), id)
	body, err := c.fetchURL(ctx, url)
	if err != nil {
		return nil, err
	}

	var story hnStory
	if err := json.Unmarshal(body, &story); err != nil {
		return nil, fmt.Errorf("parse item %d: %w", id, err)
	}
	return &story, nil
}

// fetchComments fetches up to max_comments comment texts from kid IDs.
func (c *HNConnector) fetchComments(ctx context.Context, kids []int, logger *slog.Logger) []string {
	maxComments := c.config.MaxComments
	if maxComments <= 0 {
		maxComments = 10
	}

	limit := maxComments
	if limit > len(kids) {
		limit = len(kids)
	}

	comments := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			break
		}

		url := fmt.Sprintf("%s/v0/item/%d.json", c.effectiveBaseURL(), kids[i])
		body, err := c.fetchURL(ctx, url)
		if err != nil {
			logger.Warn("fetch comment failed", "id", kids[i], "error", err)
			continue
		}

		var comment hnComment
		if err := json.Unmarshal(body, &comment); err != nil {
			logger.Warn("parse comment failed", "id", kids[i], "error", err)
			continue
		}

		if comment.Text != "" {
			comments = append(comments, comment.Text)
		}
	}

	return comments
}

func (c *HNConnector) fetchURL(ctx context.Context, url string) ([]byte, error) {
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

	limited := io.LimitReader(resp.Body, 10<<20) // 10 MB limit
	return io.ReadAll(limited)
}
