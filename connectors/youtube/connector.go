package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// YouTubeConnector polls YouTube channels for new video metadata.
type YouTubeConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://www.googleapis.com/youtube/v3" or test server URL
}

// searchResponse represents the YouTube Data API v3 search.list response.
type searchResponse struct {
	Items []searchItem `json:"items"`
}

type searchItem struct {
	ID searchItemID `json:"id"`
}

type searchItemID struct {
	VideoID string `json:"videoId"`
}

// videoResponse represents the YouTube Data API v3 videos.list response.
type videoResponse struct {
	Items []videoItem `json:"items"`
}

type videoItem struct {
	ID      string       `json:"id"`
	Snippet videoSnippet `json:"snippet"`
}

type videoSnippet struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	PublishedAt  string `json:"publishedAt"`
	ChannelTitle string `json:"channelTitle"`
}

// commentThreadsResponse represents the YouTube Data API v3 commentThreads.list response.
type commentThreadsResponse struct {
	Items []commentThreadItem `json:"items"`
}

type commentThreadItem struct {
	Snippet commentThreadSnippet `json:"snippet"`
}

type commentThreadSnippet struct {
	TopLevelComment topLevelComment `json:"topLevelComment"`
}

type topLevelComment struct {
	Snippet commentSnippet `json:"snippet"`
}

type commentSnippet struct {
	TextDisplay string `json:"textDisplay"`
}

// captionsResponse represents the YouTube Data API v3 captions.list response.
type captionsResponse struct {
	Items []captionItem `json:"items"`
}

type captionItem struct {
	Snippet captionSnippet `json:"snippet"`
}

type captionSnippet struct {
	Language string `json:"language"`
	Name     string `json:"name"`
}

// videoContent is the JSON content written to staging for each video.
type videoContent struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	PublishedAt  string   `json:"publishedAt"`
	ChannelTitle string   `json:"channelTitle"`
	Comments         []string `json:"comments,omitempty"`          // top comment texts
	CaptionLanguages []string `json:"captionLanguages,omitempty"` // available caption languages
}

func (c *YouTubeConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, channelID := range c.config.ChannelIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollChannel(ctx, channelID, checkpoint, logger); err != nil {
			logger.Warn("channel poll failed", "channel", channelID, "error", err)
		}
	}
	return nil
}

func (c *YouTubeConnector) pollChannel(ctx context.Context, channelID string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	// Step 1: Search for recent videos on this channel.
	apiKey, err := c.tokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("get api key: %w", err)
	}

	searchURL := fmt.Sprintf(
		"%s/search?channelId=%s&type=video&order=date&maxResults=10&part=snippet&key=%s",
		c.apiBase, channelID, apiKey,
	)

	searchBody, err := c.fetchAPI(ctx, searchURL)
	if err != nil {
		return fmt.Errorf("search videos for %s: %w", channelID, err)
	}

	var searchResp searchResponse
	if err := json.Unmarshal(searchBody, &searchResp); err != nil {
		return fmt.Errorf("parse search response for %s: %w", channelID, err)
	}

	if len(searchResp.Items) == 0 {
		return nil
	}

	cpKey := "channel:" + channelID
	lastPublishedAt, hasCheckpoint := checkpoint.Load(cpKey)

	ruleKey := "channel:" + channelID
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for channel, skipping", "channel", channelID)
		return nil
	}

	var latestPublishedAt string

	// Step 2: For each video, fetch full details.
	for _, si := range searchResp.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		videoID := si.ID.VideoID
		if videoID == "" {
			continue
		}

		videoURL := fmt.Sprintf(
			"%s/videos?id=%s&part=snippet&key=%s",
			c.apiBase, videoID, apiKey,
		)

		videoBody, err := c.fetchAPI(ctx, videoURL)
		if err != nil {
			logger.Warn("fetch video details failed", "video", videoID, "error", err)
			continue
		}

		var videoResp videoResponse
		if err := json.Unmarshal(videoBody, &videoResp); err != nil {
			logger.Warn("parse video details failed", "video", videoID, "error", err)
			continue
		}

		if len(videoResp.Items) == 0 {
			continue
		}

		video := videoResp.Items[0]

		// Checkpoint dedup: skip videos published at or before the last checkpoint.
		if hasCheckpoint && video.Snippet.PublishedAt <= lastPublishedAt {
			continue
		}

		if status := c.fetchCounter.TryFetch(channelID); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "channel", channelID, "status", status)
			break
		}

		content := videoContent{
			Title:        video.Snippet.Title,
			Description:  video.Snippet.Description,
			PublishedAt:  video.Snippet.PublishedAt,
			ChannelTitle: video.Snippet.ChannelTitle,
		}

		// Fetch comments for this video.
		if c.config.ShouldFetchComments() {
			comments, err := c.fetchComments(ctx, videoID, apiKey)
			if err != nil {
				logger.Warn("fetch comments failed", "video", videoID, "error", err)
			} else if len(comments) > 0 {
				content.Comments = comments
			}
		}

		// Fetch caption metadata for this video.
		if c.config.ShouldFetchCaptions() {
			captions, err := c.fetchCaptions(ctx, videoID, apiKey)
			if err != nil {
				logger.Warn("fetch captions failed", "video", videoID, "error", err)
			} else if len(captions) > 0 {
				content.CaptionLanguages = captions
			}
		}

		contentData, err := json.Marshal(content)
		if err != nil {
			return fmt.Errorf("marshal video content: %w", err)
		}

		ts, _ := time.Parse(time.RFC3339, video.Snippet.PublishedAt)
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "youtube",
			Sender:           channelID,
			Subject:          video.Snippet.Title,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "youtube", AuthMethod: "api_key"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(contentData); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		// Track the latest publishedAt for checkpoint.
		if video.Snippet.PublishedAt > latestPublishedAt {
			latestPublishedAt = video.Snippet.PublishedAt
		}
	}

	// Save checkpoint with the latest publishedAt.
	if latestPublishedAt != "" {
		if err := checkpoint.Save(cpKey, latestPublishedAt); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *YouTubeConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	// NOTE: YouTube API uses the key as a query parameter (already in the URL).
	// Do NOT set an Authorization header.

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Strip the URL from the error to avoid leaking the API key.
		var urlErr *neturl.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("YouTube API request failed: %s: %w", urlErr.Op, urlErr.Err)
		}
		return nil, fmt.Errorf("YouTube API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from YouTube API", resp.StatusCode)
	}

	const maxBody = 10 << 20 // 10 MB
	limited := io.LimitReader(resp.Body, maxBody)
	return io.ReadAll(limited)
}

// fetchComments retrieves the top comments for a video via the commentThreads API.
func (c *YouTubeConnector) fetchComments(ctx context.Context, videoID, apiKey string) ([]string, error) {
	maxComments := c.config.EffectiveMaxComments()
	url := fmt.Sprintf(
		"%s/commentThreads?videoId=%s&part=snippet&maxResults=%d&order=relevance&key=%s",
		c.apiBase, videoID, maxComments, apiKey,
	)

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch comment threads for %s: %w", videoID, err)
	}

	var resp commentThreadsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse comment threads for %s: %w", videoID, err)
	}

	var comments []string
	for _, item := range resp.Items {
		text := item.Snippet.TopLevelComment.Snippet.TextDisplay
		if text != "" {
			comments = append(comments, text)
		}
	}
	return comments, nil
}

// fetchCaptions retrieves caption metadata (available languages) for a video.
// Full subtitle text download requires OAuth, so in API-key mode we only
// include the languages and track names that are available.
func (c *YouTubeConnector) fetchCaptions(ctx context.Context, videoID, apiKey string) ([]string, error) {
	url := fmt.Sprintf(
		"%s/captions?videoId=%s&part=snippet&key=%s",
		c.apiBase, videoID, apiKey,
	)

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch captions for %s: %w", videoID, err)
	}

	var resp captionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse captions for %s: %w", videoID, err)
	}

	var languages []string
	for _, item := range resp.Items {
		if item.Snippet.Language != "" {
			languages = append(languages, item.Snippet.Language)
		}
	}
	return languages, nil
}
