package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const (
	graphAPIBase    = "https://graph.microsoft.com/v1.0/me/mailFolders"
	microsoftTokenURL = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
)

// graphMessageResponse represents a single message in the Microsoft Graph
// messages list response.
type graphMessageResponse struct {
	ID               string `json:"id"`
	Subject          string `json:"subject"`
	ReceivedDateTime string `json:"receivedDateTime"`
	From             struct {
		EmailAddress struct {
			Address string `json:"address"`
			Name    string `json:"name"`
		} `json:"emailAddress"`
	} `json:"from"`
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

// graphListResponse is the JSON structure returned by the Microsoft Graph
// messages list endpoint.
type graphListResponse struct {
	Value []graphMessageResponse `json:"value"`
}

// httpOutlookClient implements OutlookClient using real HTTP calls to
// the Microsoft Graph API.
type httpOutlookClient struct {
	tokenSource connector.TokenSource
	httpClient  *http.Client
}

func (h *httpOutlookClient) ListMessages(ctx context.Context, folderID string, checkpoint string, maxResults int) ([]OutlookMessage, error) {
	params := url.Values{}
	if checkpoint != "" {
		params.Set("$filter", fmt.Sprintf("receivedDateTime gt '%s'", checkpoint))
	}
	params.Set("$orderby", "receivedDateTime")
	if maxResults > 0 {
		params.Set("$top", fmt.Sprintf("%d", maxResults))
	}

	reqURL := fmt.Sprintf("%s/%s/messages?%s", graphAPIBase, url.PathEscape(folderID), params.Encode())
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
		return nil, fmt.Errorf("list messages request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("list messages returned status %d: %s", resp.StatusCode, string(body))
	}

	var lr graphListResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}

	var messages []OutlookMessage
	for _, m := range lr.Value {
		messages = append(messages, OutlookMessage{
			ID:               m.ID,
			Subject:          m.Subject,
			From:             m.From.EmailAddress.Address,
			ReceivedDateTime: m.ReceivedDateTime,
			BodyContent:      m.Body.Content,
			BodyContentType:  m.Body.ContentType,
		})
	}
	return messages, nil
}

func main() {
	c := &OutlookConnector{}

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
	if len(cfg.FolderIDs) == 0 {
		cfg.FolderIDs = []string{"inbox"}
	}
	if cfg.MaxResults == 0 {
		cfg.MaxResults = 25
	}

	c.config = cfg

	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")
	tenantID := os.Getenv("MS_TENANT_ID")

	tokenURL := fmt.Sprintf(microsoftTokenURL, tenantID)
	tokenSource, err := connector.NewRefreshableTokenSource(connector.OAuthConfig{
		ClientID:     os.Getenv("MS_CLIENT_ID"),
		ClientSecret: os.Getenv("MS_CLIENT_SECRET"),
		TokenURL:     tokenURL,
		Scopes:       []string{"https://graph.microsoft.com/Mail.Read"},
	}, stateDir)
	if err != nil {
		slog.Error("create token source", "error", err)
		os.Exit(1)
	}

	c.client = &httpOutlookClient{
		tokenSource: tokenSource,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}

	connector.Run(connector.Options{
		Name:       "outlook",
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
