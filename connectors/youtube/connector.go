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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// YouTubeConnector polls followed channels for new uploads and stages one
// markdown item per video.
//
// Quota (Data API v3, verified 2026-11-07 against determine_quota_cost): every
// call below costs 1 unit from the shared 10,000/day pool. search.list (which
// has its own 100/day bucket) and captions.list (50 units) are NOT used.
//
// Per channel per poll:
//   - playlistItems.list on the uploads playlist             1 unit, always
//   - videos.list, all recent uploads batched in ONE call    1 unit, only when something is new
//   - commentThreads.list per new video                      1 unit each, if enabled
//
// plus channels.list once per channel ever (cached in the checkpoint).
type YouTubeConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://www.googleapis.com/youtube/v3" or test server URL
	transcripts  TranscriptProvider
	now          func() time.Time
}

// channelInfo is a followed channel after resolution.
type channelInfo struct {
	ID      string `json:"id"`
	Handle  string `json:"handle,omitempty"`
	Title   string `json:"title,omitempty"`
	Uploads string `json:"uploads"`
}

type channelsResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title     string `json:"title"`
			CustomURL string `json:"customUrl"`
		} `json:"snippet"`
		ContentDetails struct {
			RelatedPlaylists struct {
				Uploads string `json:"uploads"`
			} `json:"relatedPlaylists"`
		} `json:"contentDetails"`
	} `json:"items"`
}

type playlistItemsResponse struct {
	Items []struct {
		ContentDetails struct {
			VideoID          string `json:"videoId"`
			VideoPublishedAt string `json:"videoPublishedAt"`
		} `json:"contentDetails"`
	} `json:"items"`
}

type videoItem struct {
	ID      string `json:"id"`
	Snippet struct {
		Title                string               `json:"title"`
		Description          string               `json:"description"`
		PublishedAt          string               `json:"publishedAt"`
		ChannelID            string               `json:"channelId"`
		ChannelTitle         string               `json:"channelTitle"`
		Thumbnails           map[string]thumbnail `json:"thumbnails"`
		Tags                 []string             `json:"tags"`
		CategoryID           string               `json:"categoryId"`
		LiveBroadcastContent string               `json:"liveBroadcastContent"`
		DefaultAudioLanguage string               `json:"defaultAudioLanguage"`
		DefaultLanguage      string               `json:"defaultLanguage"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration string `json:"duration"`
	} `json:"contentDetails"`
	// Statistics counts arrive as decimal strings. They are used only to
	// compute buckets and are never staged (glovebox-8wu1).
	Statistics struct {
		ViewCount    string  `json:"viewCount"`
		CommentCount *string `json:"commentCount"`
	} `json:"statistics"`
	TopicDetails struct {
		TopicCategories []string `json:"topicCategories"`
	} `json:"topicDetails"`
}

type videosResponse struct {
	Items []videoItem `json:"items"`
}

type commentThreadsResponse struct {
	Items []struct {
		Snippet struct {
			TopLevelComment struct {
				Snippet struct {
					AuthorDisplayName string `json:"authorDisplayName"`
					TextDisplay       string `json:"textDisplay"`
				} `json:"snippet"`
			} `json:"topLevelComment"`
		} `json:"snippet"`
	} `json:"items"`
}

// apiError is a non-200 Data API response. Reason is the first
// error.errors[].reason (e.g. quotaExceeded, commentsDisabled).
type apiError struct {
	Status int
	Reason string
}

func (e *apiError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("HTTP %d from YouTube API (%s)", e.Status, e.Reason)
	}
	return fmt.Sprintf("HTTP %d from YouTube API", e.Status)
}

func (c *YouTubeConnector) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}

func (c *YouTubeConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()
	for _, ch := range c.config.AllChannels() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollChannel(ctx, ch, checkpoint, logger); err != nil {
			logger.Warn("channel poll failed", "channel", ch, "error", err)
			var ae *apiError
			if errors.As(err, &ae) && ae.Reason == "quotaExceeded" {
				// Every further call today fails the same way; stop spending
				// requests until the next poll.
				return nil
			}
		}
	}
	return nil
}

// resolveChannel turns a configured entry (UC id or @handle) into a
// channelInfo, caching the answer in the checkpoint so it costs one
// channels.list call per channel, ever.
func (c *YouTubeConnector) resolveChannel(ctx context.Context, entry string, cp connector.Checkpoint) (channelInfo, error) {
	cacheKey := "resolve:" + entry
	if raw, ok := cp.Load(cacheKey); ok {
		var info channelInfo
		if err := json.Unmarshal([]byte(raw), &info); err == nil && info.ID != "" && info.Uploads != "" {
			return info, nil
		}
	}

	q := neturl.Values{"part": {"snippet,contentDetails"}}
	if strings.HasPrefix(entry, "UC") && !strings.HasPrefix(entry, "@") {
		q.Set("id", entry)
	} else {
		q.Set("forHandle", entry)
	}
	var resp channelsResponse
	if err := c.getJSON(ctx, "channels", q, &resp); err != nil {
		return channelInfo{}, fmt.Errorf("resolve channel %s: %w", entry, err)
	}
	if len(resp.Items) == 0 {
		return channelInfo{}, fmt.Errorf("resolve channel %s: no such channel", entry)
	}
	it := resp.Items[0]
	info := channelInfo{
		ID:      it.ID,
		Handle:  it.Snippet.CustomURL,
		Title:   it.Snippet.Title,
		Uploads: it.ContentDetails.RelatedPlaylists.Uploads,
	}
	if info.Uploads == "" {
		return channelInfo{}, fmt.Errorf("resolve channel %s: no uploads playlist", entry)
	}
	if raw, err := json.Marshal(info); err == nil {
		if err := cp.Save(cacheKey, string(raw)); err != nil {
			return channelInfo{}, fmt.Errorf("cache channel %s: %w", entry, err)
		}
	}
	return info, nil
}

type upload struct {
	ID        string
	Published string // RFC 3339, from playlistItems contentDetails.videoPublishedAt
}

func (c *YouTubeConnector) pollChannel(ctx context.Context, entry string, cp connector.Checkpoint, logger *slog.Logger) error {
	result, ok := c.matcher.Match("channel:" + entry)
	if !ok {
		logger.Warn("no rule for channel, skipping", "channel", entry)
		return nil
	}

	info, err := c.resolveChannel(ctx, entry, cp)
	if err != nil {
		return err
	}

	// Discovery: the uploads playlist, newest first. contentDetails carries
	// the video's own publish time (not when it was added to the playlist).
	var pl playlistItemsResponse
	if err := c.getJSON(ctx, "playlistItems", neturl.Values{
		"playlistId": {info.Uploads},
		"part":       {"contentDetails"},
		"maxResults": {"50"},
	}, &pl); err != nil {
		return fmt.Errorf("list uploads for %s: %w", entry, err)
	}
	var uploads []upload
	for _, it := range pl.Items {
		if it.ContentDetails.VideoID != "" && it.ContentDetails.VideoPublishedAt != "" {
			uploads = append(uploads, upload{it.ContentDetails.VideoID, it.ContentDetails.VideoPublishedAt})
		}
	}
	if len(uploads) == 0 {
		return nil
	}
	sort.SliceStable(uploads, func(i, j int) bool { return uploads[i].Published > uploads[j].Published })

	// The checkpoint is keyed by the resolved channel ID so renaming a handle
	// in config does not replay the channel.
	cpKey := "channel:" + info.ID
	last, hasCheckpoint := cp.Load(cpKey)

	var fresh []upload
	if hasCheckpoint {
		for _, u := range uploads {
			if u.Published > last {
				fresh = append(fresh, u)
			}
		}
		if n := c.config.EffectiveMaxNewPerPoll(); len(fresh) > n {
			logger.Info("more new uploads than max_new_per_poll; keeping newest", "channel", entry, "new", len(fresh), "kept", n)
			fresh = fresh[:n]
		}
	} else {
		n := c.config.EffectiveInitialBackfill()
		if len(uploads) < n {
			n = len(uploads)
		}
		fresh = uploads[:n]
	}
	if len(fresh) == 0 {
		return nil
	}

	// One videos.list call for every recent upload: the fresh ones get staged,
	// the rest are the peer set the engagement buckets are ranked against.
	ids := make([]string, len(uploads))
	for i, u := range uploads {
		ids[i] = u.ID
	}
	var vr videosResponse
	if err := c.getJSON(ctx, "videos", neturl.Values{
		"id":         {strings.Join(ids, ",")},
		"part":       {"snippet,contentDetails,statistics,topicDetails"},
		"maxResults": {"50"},
	}, &vr); err != nil {
		return fmt.Errorf("fetch video details for %s: %w", entry, err)
	}
	byID := map[string]videoItem{}
	var peers []peerStats
	for _, v := range vr.Items {
		byID[v.ID] = v
		pub, _ := time.Parse(time.RFC3339, v.Snippet.PublishedAt)
		views, _ := strconv.ParseUint(v.Statistics.ViewCount, 10, 64)
		p := peerStats{ID: v.ID, Published: pub, Views: views}
		if v.Statistics.CommentCount != nil {
			p.Comments, _ = strconv.ParseUint(*v.Statistics.CommentCount, 10, 64)
			p.HasComments = true
		}
		peers = append(peers, p)
	}
	now := c.clock()
	bk := computeBuckets(peers, now)

	// Stage oldest first so the checkpoint only ever moves forward.
	latest := last
	for i := len(fresh) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u := fresh[i]
		v, ok := byID[u.ID]
		if !ok {
			// Private, deleted or not yet processed; nothing to stage.
			continue
		}
		if status := c.fetchCounter.TryFetch(info.ID); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "channel", entry, "status", status)
			break
		}
		if err := c.stageVideo(ctx, info, v, bk[v.ID], now, result); err != nil {
			return err
		}
		if u.Published > latest {
			latest = u.Published
		}
	}

	if latest != "" && latest != last {
		if err := cp.Save(cpKey, latest); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}
	return nil
}

func (c *YouTubeConnector) stageVideo(ctx context.Context, ch channelInfo, v videoItem, b buckets, now time.Time, result connector.MatchResult) error {
	logger := slog.Default()

	pub, _ := time.Parse(time.RFC3339, v.Snippet.PublishedAt)
	note := ""
	if !pub.IsZero() {
		if age := now.Sub(pub); age < 48*time.Hour {
			note = fmt.Sprintf("measured %dh after publish; early buckets understate viewership", int(age.Hours()))
		}
	}

	in := renderInput{
		VideoID:       v.ID,
		Title:         v.Snippet.Title,
		ChannelID:     ch.ID,
		ChannelTitle:  firstNonEmpty(ch.Title, v.Snippet.ChannelTitle),
		ChannelHandle: ch.Handle,
		PublishedAt:   v.Snippet.PublishedAt,
		DurationSec:   parseISODuration(v.ContentDetails.Duration),
		Thumbnail:     bestThumbnail(v.Snippet.Thumbnails),
		CategoryID:    v.Snippet.CategoryID,
		Tags:          v.Snippet.Tags,
		Topics:        v.TopicDetails.TopicCategories,
		Language:      firstNonEmpty(v.Snippet.DefaultAudioLanguage, v.Snippet.DefaultLanguage),
		LiveContent:   v.Snippet.LiveBroadcastContent,
		Description:   v.Snippet.Description,
		Chapters:      parseChapters(v.Snippet.Description),
		Buckets:       b,
		BucketNote:    note,
	}

	if c.config.ShouldFetchComments() {
		comments, err := c.fetchComments(ctx, v.ID)
		if err != nil {
			logger.Warn("fetch comments failed", "video", v.ID, "error", err)
		}
		in.Comments = comments
	}

	in.Transcript = c.transcript(ctx, v)

	ts := pub
	if ts.IsZero() {
		ts = now
	}
	item, err := c.writer.NewItem(connector.ItemOptions{
		Source:           "youtube",
		Sender:           ch.ID,
		Subject:          v.Snippet.Title,
		Timestamp:        ts,
		DestinationAgent: result.Destination,
		ContentType:      "text/markdown",
		RuleTags:         result.Tags,
		RuleDataSubject:  result.DataSubject,
		RuleAudience:     result.Audience,
		Identity:         &connector.Identity{Provider: "youtube", AuthMethod: "api_key"},
	})
	if err != nil {
		return fmt.Errorf("new staging item: %w", err)
	}
	if err := item.WriteContent([]byte(renderContent(in))); err != nil {
		return fmt.Errorf("write content: %w", err)
	}
	if err := item.Commit(); err != nil {
		return fmt.Errorf("commit item: %w", err)
	}
	return nil
}

// transcript asks the provider for a transcript, honouring skip_categories.
// It never fails the item: any problem becomes an explicit marker.
func (c *YouTubeConnector) transcript(ctx context.Context, v videoItem) transcriptResult {
	for _, cat := range c.config.Transcript.EffectiveSkipCategories() {
		if v.Snippet.CategoryID == cat {
			return transcriptResult{Status: transcriptSkipped, Reason: "category " + cat}
		}
	}
	p := c.transcripts
	if p == nil {
		p = noTranscriptProvider{}
	}
	r, err := p.Transcript(ctx, v.ID, c.config.Transcript.EffectiveLanguages())
	if err != nil {
		slog.Default().Warn("transcript fetch failed", "video", v.ID, "error", err)
		return transcriptResult{Status: transcriptError, Reason: "provider failed"}
	}
	if r.Status == "" {
		r.Status = transcriptUnavailable
	}
	return r
}

// fetchComments returns the top comments by relevance. Comments disabled on the
// video is not an error.
func (c *YouTubeConnector) fetchComments(ctx context.Context, videoID string) ([]comment, error) {
	var resp commentThreadsResponse
	err := c.getJSON(ctx, "commentThreads", neturl.Values{
		"videoId":    {videoID},
		"part":       {"snippet"},
		"maxResults": {strconv.Itoa(c.config.EffectiveMaxComments())},
		"order":      {"relevance"},
		"textFormat": {"plainText"},
	}, &resp)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Reason == "commentsDisabled" {
			return nil, nil
		}
		return nil, fmt.Errorf("comment threads for %s: %w", videoID, err)
	}
	var out []comment
	for _, it := range resp.Items {
		s := it.Snippet.TopLevelComment.Snippet
		if s.TextDisplay != "" {
			out = append(out, comment{Author: s.AuthorDisplayName, Text: s.TextDisplay})
		}
	}
	return out, nil
}

// getJSON calls one Data API method. The key travels as a query parameter;
// errors never include the URL, so the key cannot leak into logs.
func (c *YouTubeConnector) getJSON(ctx context.Context, method string, q neturl.Values, out any) error {
	apiKey, err := c.tokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("get api key: %w", err)
	}
	q.Set("key", apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/"+method+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("build %s request", method)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		var urlErr *neturl.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("YouTube API %s request failed: %s: %w", method, urlErr.Op, urlErr.Err)
		}
		return fmt.Errorf("YouTube API %s request failed: %w", method, err)
	}
	defer resp.Body.Close()

	const maxBody = 10 << 20 // 10 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		ae := &apiError{Status: resp.StatusCode}
		var e struct {
			Error struct {
				Errors []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && len(e.Error.Errors) > 0 {
			ae.Reason = e.Error.Errors[0].Reason
		}
		return fmt.Errorf("%s: %w", method, ae)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse %s response: %w", method, err)
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
