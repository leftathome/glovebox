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

// SteamConnector polls Steam reviews and news for configured apps.
type SteamConnector struct {
	config       Config
	apiKey       string
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	httpClient   *http.Client
	fetchCounter *connector.FetchCounter

	// baseReviewURL and baseNewsURL allow tests to redirect requests
	// to a local httptest server. When empty, the real Steam URLs are used.
	baseReviewURL string
	baseNewsURL   string
}

func (c *SteamConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, app := range c.config.Apps {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if c.config.FetchReviews {
			if err := c.pollReviews(ctx, app, checkpoint, logger); err != nil {
				logger.Warn("review poll failed", "app", app.Name, "error", err)
			}
		}

		if c.config.FetchNews {
			if err := c.pollNews(ctx, app, checkpoint, logger); err != nil {
				logger.Warn("news poll failed", "app", app.Name, "error", err)
			}
		}
	}
	return nil
}

func (c *SteamConnector) reviewURL(appID string) string {
	base := "https://store.steampowered.com"
	if c.baseReviewURL != "" {
		base = c.baseReviewURL
	}
	return base + "/appreviews/" + appID + "?json=1&filter=recent&language=english&num_per_page=25"
}

func (c *SteamConnector) newsURL(appID string) string {
	base := "https://api.steampowered.com"
	if c.baseNewsURL != "" {
		base = c.baseNewsURL
	}
	return base + "/ISteamNews/GetNewsForApp/v0002/?appid=" + appID + "&count=10&key=" + c.apiKey
}

func (c *SteamConnector) pollReviews(ctx context.Context, app AppConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	body, err := c.fetchURL(ctx, c.reviewURL(app.ID))
	if err != nil {
		return fmt.Errorf("fetch reviews for %s: %w", app.Name, err)
	}

	var resp reviewsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse reviews for %s: %w", app.Name, err)
	}

	if len(resp.Reviews) == 0 {
		return nil
	}

	cpKey := "review:" + app.ID
	lastTSStr, hasCheckpoint := checkpoint.Load(cpKey)
	var lastTS int64
	if hasCheckpoint {
		lastTS, _ = strconv.ParseInt(lastTSStr, 10, 64)
	}

	// Reviews come newest-first from the API. Process in reverse (oldest first)
	// so checkpoint advances monotonically.
	var newestTS int64
	for i := len(resp.Reviews) - 1; i >= 0; i-- {
		r := resp.Reviews[i]

		if hasCheckpoint && r.TimestampCreated <= lastTS {
			continue
		}

		status := c.fetchCounter.TryFetch(app.Name)
		if status == connector.FetchPollLimit {
			return nil
		}
		if status == connector.FetchSourceLimit {
			break
		}

		ruleKey := "app:" + app.Name
		result, ok := c.matcher.Match(ruleKey)
		if !ok {
			logger.Warn("no rule for app, skipping", "app", app.Name)
			return nil
		}

		contentBytes, err := json.Marshal(map[string]interface{}{
			"recommendation_id": r.RecommendationID,
			"author_steamid":    r.Author.SteamID,
			"review":            r.Review,
			"voted_up":          r.VotedUp,
			"timestamp_created": r.TimestampCreated,
		})
		if err != nil {
			return fmt.Errorf("marshal review content: %w", err)
		}

		ts := time.Unix(r.TimestampCreated, 0).UTC()

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "steam",
			Sender:           app.Name,
			Subject:          "Review " + r.RecommendationID,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "steam", AuthMethod: "none"},
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

		if r.TimestampCreated > newestTS {
			newestTS = r.TimestampCreated
		}
	}

	if newestTS > 0 {
		if err := checkpoint.Save(cpKey, strconv.FormatInt(newestTS, 10)); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *SteamConnector) pollNews(ctx context.Context, app AppConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	body, err := c.fetchURL(ctx, c.newsURL(app.ID))
	if err != nil {
		return fmt.Errorf("fetch news for %s: %w", app.Name, err)
	}

	var resp newsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse news for %s: %w", app.Name, err)
	}

	if len(resp.AppNews.NewsItems) == 0 {
		return nil
	}

	cpKey := "news:" + app.ID
	lastTSStr, hasCheckpoint := checkpoint.Load(cpKey)
	var lastTS int64
	if hasCheckpoint {
		lastTS, _ = strconv.ParseInt(lastTSStr, 10, 64)
	}

	// News items come newest-first. Process in reverse (oldest first).
	var newestTS int64
	for i := len(resp.AppNews.NewsItems) - 1; i >= 0; i-- {
		n := resp.AppNews.NewsItems[i]

		if hasCheckpoint && n.Date <= lastTS {
			continue
		}

		status := c.fetchCounter.TryFetch(app.Name)
		if status == connector.FetchPollLimit {
			return nil
		}
		if status == connector.FetchSourceLimit {
			break
		}

		ruleKey := "app:" + app.Name
		result, ok := c.matcher.Match(ruleKey)
		if !ok {
			logger.Warn("no rule for app, skipping", "app", app.Name)
			return nil
		}

		contentBytes, err := json.Marshal(map[string]interface{}{
			"gid":        n.GID,
			"title":      n.Title,
			"url":        n.URL,
			"contents":   n.Contents,
			"author":     n.Author,
			"date":       n.Date,
			"feed_label": n.FeedLabel,
			"feed_name":  n.FeedName,
		})
		if err != nil {
			return fmt.Errorf("marshal news content: %w", err)
		}

		ts := time.Unix(n.Date, 0).UTC()

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "steam",
			Sender:           app.Name,
			Subject:          n.Title,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "steam", AuthMethod: "api_key"},
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

		if n.Date > newestTS {
			newestTS = n.Date
		}
	}

	if newestTS > 0 {
		if err := checkpoint.Save(cpKey, strconv.FormatInt(newestTS, 10)); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *SteamConnector) fetchURL(ctx context.Context, url string) ([]byte, error) {
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
		return nil, fmt.Errorf("HTTP %d from Steam API", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 10<<20) // 10 MB limit
	return io.ReadAll(limited)
}
