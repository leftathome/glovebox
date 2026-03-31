package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// GitLabConnector polls GitLab project events and stages new entries.
type GitLabConnector struct {
	config      Config
	writer      *connector.StagingWriter
	matcher     *connector.RuleMatcher
	tokenSource connector.TokenSource
	httpClient  *http.Client
}

func (c *GitLabConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, project := range c.config.Projects {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollProject(ctx, project, checkpoint, logger); err != nil {
			logger.Warn("project poll failed", "project", project.Path, "error", err)
			// Continue to next project rather than aborting entirely.
		}
	}
	return nil
}

func (c *GitLabConnector) pollProject(ctx context.Context, project ProjectConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	cpKey := "event:" + project.Path

	events, err := c.fetchAllEvents(ctx, project.Path)
	if err != nil {
		return fmt.Errorf("fetch events for %s: %w", project.Path, err)
	}

	if len(events) == 0 {
		return nil
	}

	lastIDStr, hasCheckpoint := checkpoint.Load(cpKey)
	var lastID int
	if hasCheckpoint {
		lastID, _ = strconv.Atoi(lastIDStr)
	}

	ruleKey := "project:" + project.Path
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for project, skipping", "project", project.Path)
		return nil
	}

	// GitLab returns events newest-first. Reverse to process oldest-first.
	slices.Reverse(events)

	var highestID int
	for _, rawEvent := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var eventMeta struct {
			ID        int    `json:"id"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(rawEvent, &eventMeta); err != nil {
			logger.Warn("skip unparseable event", "error", err)
			continue
		}

		// Skip events at or below the checkpoint.
		if hasCheckpoint && eventMeta.ID <= lastID {
			continue
		}

		ts, err := time.Parse(time.RFC3339Nano, eventMeta.CreatedAt)
		if err != nil {
			ts = time.Now().UTC()
		}

		subject := fmt.Sprintf("GitLab event %d in %s", eventMeta.ID, project.Path)

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "gitlab",
			Sender:           project.Path,
			Subject:          subject,
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "gitlab", AuthMethod: "pat"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(rawEvent); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if eventMeta.ID > highestID {
			highestID = eventMeta.ID
		}
	}

	if highestID > 0 {
		if err := checkpoint.Save(cpKey, strconv.Itoa(highestID)); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

// fetchAllEvents fetches all pages of events for a project.
func (c *GitLabConnector) fetchAllEvents(ctx context.Context, projectPath string) ([]json.RawMessage, error) {
	baseURL := c.config.BaseURL
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}

	// URL-encode the project path so that "/" becomes "%2F", which is
	// what the GitLab API expects when using the path as a project ID.
	encodedPath := url.PathEscape(projectPath)

	const maxPages = 10
	var allEvents []json.RawMessage
	page := ""

	for pageCount := 0; pageCount < maxPages; pageCount++ {
		rawPath := fmt.Sprintf("/api/v4/projects/%s/events", encodedPath)
		query := ""
		if page != "" {
			query = "?page=" + page
		}

		body, nextPage, err := c.fetchPageRaw(ctx, baseURL, rawPath, query)
		if err != nil {
			return nil, err
		}

		var events []json.RawMessage
		if err := json.Unmarshal(body, &events); err != nil {
			return nil, fmt.Errorf("parse events response: %w", err)
		}

		allEvents = append(allEvents, events...)

		nextPage = strings.TrimSpace(nextPage)
		if nextPage == "" || nextPage == "0" {
			break
		}
		page = nextPage
	}

	return allEvents, nil
}

func (c *GitLabConnector) fetchPageRaw(ctx context.Context, baseURL, rawPath, query string) ([]byte, string, error) {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("get token: %w", err)
	}

	// Build the full URL string with the encoded path, then parse it
	// and set RawPath to preserve %2F encoding in the request URI.
	fullURL := baseURL + rawPath + query
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, "", err
	}
	// Preserve the encoded path so %2F is not decoded to /.
	req.URL.RawPath = rawPath
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("User-Agent", "glovebox-gitlab/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d from %s%s", resp.StatusCode, rawPath, query)
	}

	limited := io.LimitReader(resp.Body, 10<<20) // 10 MB limit
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}

	nextPage := resp.Header.Get("X-Next-Page")
	return body, nextPage, nil
}
