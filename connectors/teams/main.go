package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
)

const (
	graphTeamsAPIBase = "https://graph.microsoft.com/v1.0"
	microsoftTokenURL = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
)

func main() {
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

	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")
	tenantID := os.Getenv("MS_TENANT_ID")

	tokenURL := fmt.Sprintf(microsoftTokenURL, tenantID)
	tokenSource, err := connector.NewRefreshableTokenSource(connector.OAuthConfig{
		ClientID:     os.Getenv("MS_CLIENT_ID"),
		ClientSecret: os.Getenv("MS_CLIENT_SECRET"),
		TokenURL:     tokenURL,
		Scopes:       []string{"https://graph.microsoft.com/ChannelMessage.Read.All"},
	}, stateDir)
	if err != nil {
		slog.Error("create token source", "error", err)
		os.Exit(1)
	}

	c := &TeamsConnector{
		config:      cfg,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		tokenSource: tokenSource,
		apiBase:     graphTeamsAPIBase,
	}

	connector.Run(connector.Options{
		Name:       "teams",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   stateDir,
		ConfigFile: cfgFile,
		Connector:  c,
		Setup: func(cc connector.ConnectorContext) error {
			c.writer = cc.Writer
			c.matcher = cc.Matcher
			c.fetchCounter = cc.FetchCounter
			if cfg.ConfigIdentity != nil {
				cc.Writer.SetConfigIdentity(cfg.ConfigIdentity)
			}
			return nil
		},
		PollInterval: 5 * time.Minute,
	})
}

// Ensure TeamsConnector satisfies connector.Connector at compile time.
var _ connector.Connector = (*TeamsConnector)(nil)
