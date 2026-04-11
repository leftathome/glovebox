package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const (
	gmailAPIBase = "https://gmail.googleapis.com/gmail/v1/users/me"
	googleTokenURL = "https://oauth2.googleapis.com/token"
)

// listResponse is the JSON structure returned by the Gmail messages.list endpoint.
type listResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

// rawMessageResponse is the JSON structure returned by the Gmail messages.get
// endpoint with format=raw.
type rawMessageResponse struct {
	ID           string `json:"id"`
	InternalDate string `json:"internalDate"`
	Raw          string `json:"raw"`
	Payload      struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

// httpGmailClient implements GmailClient using real HTTP calls to the Gmail API.
type httpGmailClient struct {
	tokenSource connector.TokenSource
	httpClient  *http.Client
}

func (h *httpGmailClient) ListMessages(ctx context.Context, labelID string, query string, maxResults int) ([]string, error) {
	params := url.Values{}
	params.Set("labelIds", labelID)
	if query != "" {
		params.Set("q", query)
	}
	if maxResults > 0 {
		params.Set("maxResults", fmt.Sprintf("%d", maxResults))
	}

	reqURL := fmt.Sprintf("%s/messages?%s", gmailAPIBase, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}

	token, err := h.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list messages request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("list messages returned %d: %s", resp.StatusCode, string(body))
	}

	var lr listResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}

	var ids []string
	for _, m := range lr.Messages {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func (h *httpGmailClient) GetMessage(ctx context.Context, messageID string) (*GmailMessage, error) {
	reqURL := fmt.Sprintf("%s/messages/%s?format=raw", gmailAPIBase, messageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build get request: %w", err)
	}

	token, err := h.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get message request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("get message returned %d: %s", resp.StatusCode, string(body))
	}

	var raw rawMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode message response: %w", err)
	}

	// Gmail uses URL-safe base64 without padding for the raw field.
	decoded, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(raw.Raw)
	if err != nil {
		return nil, fmt.Errorf("base64url decode raw message: %w", err)
	}

	var internalDate int64
	fmt.Sscanf(raw.InternalDate, "%d", &internalDate)

	var sender, subject string
	for _, hdr := range raw.Payload.Headers {
		switch strings.ToLower(hdr.Name) {
		case "from":
			sender = hdr.Value
		case "subject":
			subject = hdr.Value
		}
	}

	return &GmailMessage{
		ID:           raw.ID,
		Raw:          decoded,
		Sender:       sender,
		Subject:      subject,
		InternalDate: internalDate,
	}, nil
}

func main() {
	c := &GmailConnector{}

	cfgFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
	if cfgFile == "" {
		cfgFile = "/etc/connector/config.json"
	}

	data, err := os.ReadFile(cfgFile)
	if err != nil {
		slog.Error("read config", "path", cfgFile, "error", err)
		os.Exit(1)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("parse config", "error", err)
		os.Exit(1)
	}

	// Apply defaults.
	if len(cfg.LabelIDs) == 0 {
		cfg.LabelIDs = []string{"INBOX"}
	}
	if cfg.MaxResults == 0 {
		cfg.MaxResults = 25
	}

	c.config = cfg

	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")

	tokenSource, err := connector.NewRefreshableTokenSource(connector.OAuthConfig{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		TokenURL:     googleTokenURL,
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly"},
	}, stateDir)
	if err != nil {
		slog.Error("create token source", "error", err)
		os.Exit(1)
	}

	c.client = &httpGmailClient{
		tokenSource: tokenSource,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}

	connector.Run(connector.Options{
		Name:       "gmail",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   stateDir,
		ConfigFile: cfgFile,
		Connector:  c,
		Setup: func(cc connector.ConnectorContext) error {
			c.writer = cc.Backend
			c.matcher = cc.Matcher
			c.fetchCounter = cc.FetchCounter
			if cfg.ConfigIdentity != nil && cc.Writer != nil {
				cc.Writer.SetConfigIdentity(cfg.ConfigIdentity)
			}
			return nil
		},
		PollInterval: 5 * time.Minute,
	})
}
