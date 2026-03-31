package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// XConnector polls X (Twitter) API v2 for user mentions and handles webhooks.
type XConnector struct {
	config        Config
	writer        *connector.StagingWriter
	matcher       *connector.RuleMatcher
	httpClient    *http.Client
	tokenSource   connector.TokenSource
	apiBase       string // e.g. "https://api.x.com" or test server URL
	webhookSecret []byte
}

// tweet is a minimal representation of a tweet from the X API v2.
type tweet struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	AuthorID  string `json:"author_id"`
	CreatedAt string `json:"created_at"`
}

// mentionsResponse is the top-level response from GET /2/users/{id}/mentions.
type mentionsResponse struct {
	Data []json.RawMessage `json:"data"`
}

func (c *XConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, feedType := range c.config.FeedTypes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if feedType == "mentions" {
			if err := c.pollMentions(ctx, checkpoint, logger); err != nil {
				logger.Warn("mentions poll failed", "user_id", c.config.UserID, "error", err)
			}
		}
	}
	return nil
}

func (c *XConnector) pollMentions(ctx context.Context, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	cpKey := "tweet:latest"

	url := fmt.Sprintf("%s/2/users/%s/mentions?tweet.fields=created_at,author_id,text", c.apiBase, c.config.UserID)

	lastID, hasCheckpoint := checkpoint.Load(cpKey)
	if hasCheckpoint {
		url += "&since_id=" + lastID
	}

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch mentions: %w", err)
	}

	var resp mentionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse mentions response: %w", err)
	}

	if len(resp.Data) == 0 {
		return nil
	}

	// Parse tweets to extract IDs while keeping raw JSON.
	type tweetWithRaw struct {
		parsed tweet
		raw    json.RawMessage
	}

	tweets := make([]tweetWithRaw, 0, len(resp.Data))
	for _, raw := range resp.Data {
		var tw tweet
		if err := json.Unmarshal(raw, &tw); err != nil {
			return fmt.Errorf("parse tweet: %w", err)
		}
		tweets = append(tweets, tweetWithRaw{parsed: tw, raw: raw})
	}

	// X API returns newest first. The first element is the newest tweet.
	newestID := tweets[0].parsed.ID

	// Reverse to process oldest first.
	slices.Reverse(tweets)

	ruleKey := "feed:mentions"
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for feed type, skipping", "feed_type", "mentions")
		return nil
	}

	for _, tw := range tweets {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "x",
			Sender:           tw.parsed.AuthorID,
			Subject:          "mention from " + tw.parsed.AuthorID,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity: &connector.Identity{
				Provider:   "x",
				AuthMethod: "oauth",
				AccountID:  c.config.UserID,
			},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(tw.raw); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}
	}

	if newestID != "" {
		if err := checkpoint.Save(cpKey, newestID); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *XConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "glovebox-x/1.0")

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

// Handler returns an http.Handler for X webhooks, implementing connector.Listener.
// GET requests handle the CRC challenge. POST requests handle webhook events.
func (c *XConnector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			c.handleCRC(w, r)
		case http.MethodPost:
			c.handleWebhook(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// handleCRC responds to the CRC challenge by computing HMAC-SHA256 of
// the crc_token with the webhook secret.
func (c *XConnector) handleCRC(w http.ResponseWriter, r *http.Request) {
	crcToken := r.URL.Query().Get("crc_token")
	if crcToken == "" {
		http.Error(w, "missing crc_token", http.StatusBadRequest)
		return
	}

	mac := hmac.New(sha256.New, c.webhookSecret)
	mac.Write([]byte(crcToken))
	responseToken := "sha256=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"response_token": responseToken,
	})
}

// handleWebhook verifies the signature and stages the webhook event.
func (c *XConnector) handleWebhook(w http.ResponseWriter, r *http.Request) {
	const maxBody = 10 << 20 // 10 MB
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify HMAC signature.
	if len(c.webhookSecret) > 0 {
		sig := r.Header.Get("X-Twitter-Webhooks-Signature")
		if !connector.VerifyHMAC(body, sig, c.webhookSecret, "sha256") {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Determine event type from the payload keys.
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	eventType := "unknown"
	for key := range payload {
		eventType = key
		break
	}

	ruleKey := "event:" + eventType
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("no matching rule"))
		return
	}

	item, err := c.writer.NewItem(connector.ItemOptions{
		Source:           "x",
		Sender:           "webhook",
		Subject:          eventType + " webhook event",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: result.Destination,
		ContentType:      "application/json",
		RuleTags:         result.Tags,
		Identity: &connector.Identity{
			Provider:   "x",
			AuthMethod: "webhook",
			AccountID:  c.config.UserID,
		},
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
