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

// MetaConnector polls Meta Graph API page feeds and handles webhooks.
type MetaConnector struct {
	config      Config
	writer      *connector.StagingWriter
	matcher     *connector.RuleMatcher
	httpClient  *http.Client
	tokenSource connector.TokenSource
	apiBase     string // e.g. "https://graph.facebook.com" or test server URL
	appSecret   []byte // META_APP_SECRET for webhook signature verification
	verifyToken string // META_VERIFY_TOKEN for webhook subscription verification
}

// metaPost is a minimal representation of a post from the Graph API page feed.
type metaPost struct {
	ID          string          `json:"id"`
	Message     string          `json:"message"`
	CreatedTime string          `json:"created_time"`
	From        json.RawMessage `json:"from"`
	Raw         json.RawMessage `json:"-"`
}

// metaFeedResponse is the envelope returned by GET /{page_id}/feed.
type metaFeedResponse struct {
	Data []json.RawMessage `json:"data"`
}

func (c *MetaConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := c.pollFeed(ctx, checkpoint, logger); err != nil {
		logger.Warn("feed poll failed", "page_id", c.config.PageID, "error", err)
		return err
	}
	return nil
}

func (c *MetaConnector) pollFeed(ctx context.Context, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	url := fmt.Sprintf("%s/%s/feed?fields=id,message,created_time,from&access_token=%s",
		c.apiBase, c.config.PageID, token)

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch feed for page %s: %w", c.config.PageID, err)
	}

	var feedResp metaFeedResponse
	if err := json.Unmarshal(body, &feedResp); err != nil {
		return fmt.Errorf("parse feed for page %s: %w", c.config.PageID, err)
	}

	posts := make([]metaPost, 0, len(feedResp.Data))
	for _, raw := range feedResp.Data {
		var p metaPost
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("parse post element: %w", err)
		}
		p.Raw = raw
		posts = append(posts, p)
	}

	if len(posts) == 0 {
		return nil
	}

	cpKey := "post:latest"
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// Graph API returns newest-first. Reverse to process oldest-first.
	slices.Reverse(posts)

	// Find start index after checkpoint.
	startIdx := 0
	if hasCheckpoint {
		foundIdx := -1
		for i, p := range posts {
			if p.ID == lastID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			startIdx = foundIdx + 1
		}
	}

	ruleKey := "platform:facebook"
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for platform, skipping", "platform", "facebook")
		return nil
	}

	for i := startIdx; i < len(posts); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		p := posts[i]

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "meta",
			Sender:           c.config.PageID,
			Subject:          "post on page " + c.config.PageID,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "meta", AuthMethod: "oauth"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(p.Raw); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if err := checkpoint.Save(cpKey, p.ID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *MetaConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "glovebox-meta/1.0")

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

// Handler returns an http.Handler for Meta webhooks, implementing connector.Listener.
func (c *MetaConnector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			c.handleWebhookVerify(w, r)
		case http.MethodPost:
			c.handleWebhookEvent(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// handleWebhookVerify responds to Meta's webhook verification challenge.
// Meta sends: GET /webhook?hub.mode=subscribe&hub.verify_token=<token>&hub.challenge=<challenge>
func (c *MetaConnector) handleWebhookVerify(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("hub.mode")
	token := r.URL.Query().Get("hub.verify_token")
	challenge := r.URL.Query().Get("hub.challenge")

	if mode == "subscribe" && token == c.verifyToken {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(challenge))
		return
	}

	http.Error(w, "verification failed", http.StatusForbidden)
}

// handleWebhookEvent processes an inbound webhook event from Meta.
func (c *MetaConnector) handleWebhookEvent(w http.ResponseWriter, r *http.Request) {
	const maxBody = 10 << 20 // 10 MB
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify HMAC signature if app secret is configured.
	if len(c.appSecret) > 0 {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !connector.VerifyHMAC(body, sig, c.appSecret, "sha256") {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Parse the payload to determine object type for routing.
	var payload struct {
		Object string `json:"object"`
	}
	objectType := "unknown"
	if json.Unmarshal(body, &payload) == nil && payload.Object != "" {
		objectType = payload.Object
	}

	ruleKey := "event:" + objectType
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("no matching rule"))
		return
	}

	item, err := c.writer.NewItem(connector.ItemOptions{
		Source:           "meta",
		Sender:           "webhook",
		Subject:          objectType + " webhook event",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: result.Destination,
		ContentType:      "application/json",
		RuleTags:         result.Tags,
		Identity:         &connector.Identity{Provider: "meta", AuthMethod: "webhook"},
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
}
