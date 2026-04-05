package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func main() {
	configFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
	if configFile == "" {
		configFile = "/etc/connector/config.json"
	}

	var cfg Config
	data, err := os.ReadFile(configFile)
	if err != nil {
		slog.Error("read config", "error", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("parse config", "error", err)
		os.Exit(1)
	}

	clientID := os.Getenv("MS_CLIENT_ID")
	if clientID == "" {
		slog.Error("MS_CLIENT_ID environment variable is required")
		os.Exit(1)
	}
	clientSecret := os.Getenv("MS_CLIENT_SECRET")
	if clientSecret == "" {
		slog.Error("MS_CLIENT_SECRET environment variable is required")
		os.Exit(1)
	}
	tenantID := os.Getenv("MS_TENANT_ID")
	if tenantID == "" {
		slog.Error("MS_TENANT_ID environment variable is required")
		os.Exit(1)
	}

	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")
	if stateDir == "" {
		slog.Error("GLOVEBOX_STATE_DIR environment variable is required")
		os.Exit(1)
	}

	tokenSource, err := connector.NewRefreshableTokenSource(connector.OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     "https://login.microsoftonline.com/" + tenantID + "/oauth2/v2.0/token",
	}, stateDir)
	if err != nil {
		slog.Error("create token source", "error", err)
		os.Exit(1)
	}

	c := &OneDriveConnector{
		config:      cfg,
		httpClient:  connector.NewHTTPClient(connector.HTTPClientOptions{}),
		tokenSource: tokenSource,
		apiBase:     "https://graph.microsoft.com",
	}

	connector.Run(connector.Options{
		Name:       "onedrive",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   stateDir,
		ConfigFile: configFile,
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

// Ensure OneDriveConnector satisfies connector.Connector at compile time.
var _ connector.Connector = (*OneDriveConnector)(nil)
