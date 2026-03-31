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
)

// JiraConnector polls Jira projects for recently updated issues.
type JiraConnector struct {
	config     Config
	writer     *connector.StagingWriter
	matcher    *connector.RuleMatcher
	email      string
	apiToken   string
	httpClient *http.Client
}

func (c *JiraConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, project := range c.config.Projects {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollProject(ctx, project, checkpoint, logger); err != nil {
			logger.Warn("project poll failed", "project", project, "error", err)
			// Continue to next project rather than aborting entirely.
		}
	}
	return nil
}

func (c *JiraConnector) pollProject(ctx context.Context, project string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	cpKey := "updated:" + project

	jql := fmt.Sprintf("project = %s", project)
	if lastUpdated, ok := checkpoint.Load(cpKey); ok {
		jql += fmt.Sprintf(" AND updated > \"%s\"", lastUpdated)
	}
	jql += " ORDER BY updated ASC"

	issues, err := c.searchIssues(ctx, jql)
	if err != nil {
		return fmt.Errorf("search issues for %s: %w", project, err)
	}

	if len(issues) == 0 {
		return nil
	}

	ruleKey := "project:" + project
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for project, skipping", "project", project)
		return nil
	}

	var latestUpdated string

	for _, issue := range issues {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Build the content JSON.
		descStr := descriptionToString(issue.Fields.Description)
		summary := issueSummary{
			Key:         issue.Key,
			Summary:     issue.Fields.Summary,
			Status:      issue.Fields.Status.Name,
			Description: descStr,
		}
		content, err := json.Marshal(summary)
		if err != nil {
			return fmt.Errorf("marshal issue content: %w", err)
		}

		ts := parseJiraTime(issue.Fields.Updated)

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "jira",
			Sender:           project,
			Subject:          fmt.Sprintf("%s: %s", issue.Key, issue.Fields.Summary),
			Timestamp:        ts,
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity: &connector.Identity{
				Provider:   "jira",
				AuthMethod: "api_key",
				AccountID:  c.email,
			},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(content); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		// Track the latest updated timestamp (issues are ordered ASC).
		if issue.Fields.Updated > latestUpdated {
			latestUpdated = issue.Fields.Updated
		}
	}

	if latestUpdated != "" {
		if err := checkpoint.Save(cpKey, latestUpdated); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *JiraConnector) searchIssues(ctx context.Context, jql string) ([]jiraIssue, error) {
	searchURL := fmt.Sprintf("%s/rest/api/3/search?jql=%s", c.config.BaseURL, url.QueryEscape(jql))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(c.email, c.apiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("Jira API returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	const maxBody = 10 << 20 // 10 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}

	var searchResp jiraSearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	return searchResp.Issues, nil
}

// descriptionToString converts a Jira description field (which may be ADF
// or a plain string or nil) into a plain string.
func descriptionToString(desc interface{}) string {
	if desc == nil {
		return ""
	}
	switch v := desc.(type) {
	case string:
		return v
	default:
		// For ADF or other complex types, marshal to JSON as a fallback.
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(data)
	}
}

// parseJiraTime parses a Jira timestamp string into a time.Time.
func parseJiraTime(raw string) time.Time {
	formats := []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05.000+0000",
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
	}
	for _, layout := range formats {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}
