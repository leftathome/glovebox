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

// LinkedInConnector polls LinkedIn shares via the REST API v2.
type LinkedInConnector struct {
	config      Config
	writer      *connector.StagingWriter
	matcher     *connector.RuleMatcher
	httpClient  *http.Client
	tokenSource connector.TokenSource
	apiBase     string // e.g. "https://api.linkedin.com" or test server URL
	personID    string // LinkedIn person URN ID
}

// liShare is a minimal representation of a LinkedIn share from the Shares API.
type liShare struct {
	ID  string          `json:"id"`
	Raw json.RawMessage `json:"-"`
}

// sharesResponse is the envelope returned by the LinkedIn shares endpoint.
type sharesResponse struct {
	Elements []json.RawMessage `json:"elements"`
}

func (c *LinkedInConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, feedType := range c.config.FeedTypes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollFeed(ctx, feedType, checkpoint, logger); err != nil {
			logger.Warn("feed poll failed", "feed_type", feedType, "error", err)
		}
	}
	return nil
}

func (c *LinkedInConnector) pollFeed(ctx context.Context, feedType string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	url := fmt.Sprintf("%s/v2/shares?q=owners&owners=urn:li:person:%s", c.apiBase, c.personID)

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch shares: %w", err)
	}

	var resp sharesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse shares response: %w", err)
	}

	// Parse each element to extract ID while keeping raw bytes.
	shares := make([]liShare, 0, len(resp.Elements))
	for _, raw := range resp.Elements {
		var s liShare
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("parse share element: %w", err)
		}
		s.Raw = raw
		shares = append(shares, s)
	}

	if len(shares) == 0 {
		return nil
	}

	cpKey := "share:latest"
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// LinkedIn returns shares newest-first. Reverse to process oldest-first.
	slices.Reverse(shares)

	// Find start index after checkpoint.
	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, s := range shares {
			if s.ID == lastID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		}
	}

	ruleKey := "feed:" + feedType
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for feed type, skipping", "feed_type", feedType)
		return nil
	}

	for i := startIdx; i < len(shares); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		s := shares[i]

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "linkedin",
			Sender:           "urn:li:person:" + c.personID,
			Subject:          feedType + " share " + s.ID,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "linkedin", AuthMethod: "oauth"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(s.Raw); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, s.ID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *LinkedInConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "glovebox-linkedin/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	const maxBody = 10 << 20 // 10 MB
	limited := io.LimitReader(resp.Body, maxBody)
	return io.ReadAll(limited)
}
