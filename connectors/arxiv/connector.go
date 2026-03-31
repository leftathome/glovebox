package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const defaultBaseURL = "http://export.arxiv.org/api/query"
const defaultMaxResults = 25

// ArxivConnector polls the Arxiv API for new papers matching configured queries.
type ArxivConnector struct {
	config       Config
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	httpClient   *http.Client
	fetchCounter *connector.FetchCounter
	baseURL      string // overridden in tests
}

func (c *ArxivConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, query := range c.config.Queries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollQuery(ctx, query, checkpoint, logger); err != nil {
			logger.Warn("query poll failed", "query", query.Name, "error", err)
		}
	}
	return nil
}

func (c *ArxivConnector) pollQuery(ctx context.Context, query QueryConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	maxResults := query.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}

	base := c.baseURL
	if base == "" {
		base = defaultBaseURL
	}

	url := fmt.Sprintf("%s?search_query=%s&sortBy=submittedDate&sortOrder=descending&max_results=%d",
		base, query.Query, maxResults)

	body, err := c.fetchURL(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch arxiv query %s: %w", query.Name, err)
	}

	entries, err := parseArxivFeed(body)
	if err != nil {
		return fmt.Errorf("parse arxiv response for %s: %w", query.Name, err)
	}

	if len(entries) == 0 {
		return nil
	}

	cpKey := "last:" + query.Name
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// Entries come oldest-first after parseArxivFeed reverses them.
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
		}
	}

	ruleKey := "query:" + query.Name
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for query, skipping", "query", query.Name)
		return nil
	}

	for i := startIdx; i < len(entries); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		status := c.fetchCounter.TryFetch(query.Name)
		if status == connector.FetchPollLimit {
			return nil
		}
		if status == connector.FetchSourceLimit {
			break
		}

		entry := entries[i]
		contentJSON := buildPaperContent(entry)

		ts := parsePublishedTime(entry.Published)

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "arxiv",
			Sender:           query.Name,
			Subject:          strings.TrimSpace(entry.Title),
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "arxiv", AuthMethod: "none"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(contentJSON); err != nil {
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

func parseArxivFeed(data []byte) ([]arxivEntry, error) {
	var feed arxivFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}

	// Arxiv returns newest-first (sorted by submittedDate descending).
	// Reverse to process oldest first, matching the RSS connector pattern.
	slices.Reverse(feed.Entries)
	return feed.Entries, nil
}

// paperContent is the JSON structure written as item content.
type paperContent struct {
	Title      string   `json:"title"`
	Abstract   string   `json:"abstract"`
	Authors    []string `json:"authors"`
	Categories []string `json:"categories"`
	Link       string   `json:"link"`
	ArxivID    string   `json:"arxiv_id"`
}

func buildPaperContent(entry arxivEntry) []byte {
	authors := make([]string, 0, len(entry.Authors))
	for _, a := range entry.Authors {
		authors = append(authors, a.Name)
	}

	categories := make([]string, 0, len(entry.Categories))
	for _, cat := range entry.Categories {
		categories = append(categories, cat.Term)
	}

	link := ""
	for _, l := range entry.Links {
		if l.Rel == "alternate" || l.Rel == "" {
			link = l.Href
			break
		}
	}
	if link == "" && len(entry.Links) > 0 {
		link = entry.Links[0].Href
	}

	pc := paperContent{
		Title:      strings.TrimSpace(entry.Title),
		Abstract:   strings.TrimSpace(entry.Summary),
		Authors:    authors,
		Categories: categories,
		Link:       link,
		ArxivID:    entry.ID,
	}

	data, _ := json.Marshal(pc)
	return data
}

func (c *ArxivConnector) fetchURL(ctx context.Context, url string) ([]byte, error) {
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

var timeFormats = []string{
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05-07:00",
	"2006-01-02",
}

func parsePublishedTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now().UTC()
	}

	for _, layout := range timeFormats {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}

	return time.Now().UTC()
}

// effectiveMaxResults returns the max_results value, defaulting to 25.
func effectiveMaxResults(n int) string {
	if n <= 0 {
		return strconv.Itoa(defaultMaxResults)
	}
	return strconv.Itoa(n)
}
