package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const defaultBaseURL = "https://api.semanticscholar.org/graph/v1"

// SemanticScholarConnector polls the Semantic Scholar API for papers
// matching configured search queries.
type SemanticScholarConnector struct {
	config       Config
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	httpClient   *http.Client
	fetchCounter *connector.FetchCounter
	apiKey       string
	baseURL      string
}

func (c *SemanticScholarConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, q := range c.config.Queries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollQuery(ctx, q, checkpoint, logger); err != nil {
			logger.Warn("query poll failed", "query", q.Name, "error", err)
		}
	}
	return nil
}

func (c *SemanticScholarConnector) pollQuery(ctx context.Context, q QueryConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	papers, err := c.fetchPapers(ctx, q.Query)
	if err != nil {
		return fmt.Errorf("fetch papers for %s: %w", q.Name, err)
	}

	if len(papers) == 0 {
		return nil
	}

	cpKey := "last:" + q.Name
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// Find where new papers start. Papers are returned newest-first by the
	// API. We process them in order and checkpoint the last one we see.
	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, p := range papers {
			if p.PaperID == lastID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		}
	}

	ruleKey := "query:" + q.Name
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for query, skipping", "query", q.Name)
		return nil
	}

	authMethod := "none"
	if c.apiKey != "" {
		authMethod = "api_key"
	}

	for i := startIdx; i < len(papers); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		status := c.fetchCounter.TryFetch(q.Name)
		if status == connector.FetchPollLimit {
			return nil
		}
		if status == connector.FetchSourceLimit {
			break
		}

		p := papers[i]
		body := buildPaperContent(p)

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "semantic-scholar",
			Sender:           q.Name,
			Subject:          p.Title,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity: &connector.Identity{
				Provider:   "semantic-scholar",
				AuthMethod: authMethod,
			},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent([]byte(body)); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, p.PaperID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *SemanticScholarConnector) fetchPapers(ctx context.Context, query string) ([]paper, error) {
	base := c.baseURL
	if base == "" {
		base = defaultBaseURL
	}

	u, err := url.Parse(base + "/paper/search")
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("query", query)
	params.Set("fields", "paperId,title,abstract,tldr,authors,year")
	params.Set("limit", "25")
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Semantic Scholar API", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 10<<20) // 10 MB limit
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}

	var sr searchResponse
	if err := json.Unmarshal(data, &sr); err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}

	return sr.Data, nil
}

func buildPaperContent(p paper) string {
	var sb strings.Builder

	content := struct {
		Title    string   `json:"title"`
		Abstract string   `json:"abstract,omitempty"`
		TLDR     string   `json:"tldr,omitempty"`
		Authors  []string `json:"authors,omitempty"`
		Year     int      `json:"year,omitempty"`
	}{
		Title:    p.Title,
		Abstract: p.Abstract,
		Year:     p.Year,
	}

	if p.TLDR != nil {
		content.TLDR = p.TLDR.Text
	}

	authors := make([]string, 0, len(p.Authors))
	for _, a := range p.Authors {
		authors = append(authors, a.Name)
	}
	content.Authors = authors

	data, err := json.Marshal(content)
	if err != nil {
		// Fallback to plain text if JSON marshaling fails.
		sb.WriteString(p.Title)
		return sb.String()
	}

	return string(data)
}
