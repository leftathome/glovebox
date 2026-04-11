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

// GitHubConnector polls GitHub repository events and handles webhooks.
type GitHubConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://api.github.com" or test server URL
	webhookSecret []byte
}

// ghEvent is a minimal representation of a GitHub event from the Events API.
type ghEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Repo json.RawMessage `json:"repo"`
	Raw  json.RawMessage `json:"-"`
}

func (c *GitHubConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, repo := range c.config.Repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollRepo(ctx, repo, checkpoint, logger); err != nil {
			logger.Warn("repo poll failed", "owner", repo.Owner, "repo", repo.Repo, "error", err)
		}
	}
	return nil
}

func (c *GitHubConnector) pollRepo(ctx context.Context, repo RepoConfig, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	repoSlug := repo.Owner + "/" + repo.Repo
	url := fmt.Sprintf("%s/repos/%s/events", c.apiBase, repoSlug)

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch events for %s: %w", repoSlug, err)
	}

	// Parse the raw JSON array, keeping the raw bytes of each element.
	var rawEvents []json.RawMessage
	if err := json.Unmarshal(body, &rawEvents); err != nil {
		return fmt.Errorf("parse events for %s: %w", repoSlug, err)
	}

	events := make([]ghEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var ev ghEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("parse event element: %w", err)
		}
		ev.Raw = raw
		events = append(events, ev)
	}

	if len(events) == 0 {
		return nil
	}

	cpKey := "event:" + repoSlug
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// GitHub returns events newest-first. Reverse to process oldest-first.
	slices.Reverse(events)

	// Find start index after checkpoint.
	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, ev := range events {
			if ev.ID == lastID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		}
	}

	ruleKey := "repo:" + repoSlug
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for repo, skipping", "repo", repoSlug)
		return nil
	}

	for i := startIdx; i < len(events); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		ev := events[i]

		if status := c.fetchCounter.TryFetch(repoSlug); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "repo", repoSlug, "status", status)
			break
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "github",
			Sender:           repoSlug,
			Subject:          ev.Type + " on " + repoSlug,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "github", AuthMethod: "pat"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(ev.Raw); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, ev.ID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *GitHubConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

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

// Handler returns an http.Handler for GitHub webhooks, implementing connector.Listener.
func (c *GitHubConnector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		const maxBody = 10 << 20 // 10 MB
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		// Verify HMAC signature if webhook secret is configured.
		if len(c.webhookSecret) > 0 {
			sig := r.Header.Get("X-Hub-Signature-256")
			if !connector.VerifyHMAC(body, sig, c.webhookSecret, "sha256") {
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
		}

		eventType := r.Header.Get("X-GitHub-Event")
		if eventType == "" {
			eventType = "unknown"
		}

		ruleKey := "event:" + eventType
		result, ok := c.matcher.Match(ruleKey)
		if !ok {
			// No rule matched; accept the webhook but do not stage.
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("no matching rule"))
			return
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "github",
			Sender:           "webhook",
			Subject:          eventType + " webhook event",
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "github", AuthMethod: "webhook"},
		})
		if err != nil {
			http.Error(w, "staging error", http.StatusInternalServerError)
			return
		}

		if err := item.WriteContent(body); err != nil {
			http.Error(w, "write error", http.StatusInternalServerError)
			return
		}

		if err := item.Commit(); err != nil {
			http.Error(w, "commit error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

