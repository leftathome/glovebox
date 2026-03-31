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

	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	if clientID == "" {
		slog.Error("GOOGLE_CLIENT_ID environment variable is required")
		os.Exit(1)
	}
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if clientSecret == "" {
		slog.Error("GOOGLE_CLIENT_SECRET environment variable is required")
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
		TokenURL:     "https://oauth2.googleapis.com/token",
	}, stateDir)
	if err != nil {
		slog.Error("create token source", "error", err)
		os.Exit(1)
	}

	c := &GDriveConnector{
		config:      cfg,
		httpClient:  connector.NewHTTPClient(connector.HTTPClientOptions{}),
		tokenSource: tokenSource,
		apiBase:     "https://www.googleapis.com",
	}

	connector.Run(connector.Options{
		Name:       "gdrive",
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

// Ensure GDriveConnector satisfies connector.Connector at compile time.
var _ connector.Connector = (*GDriveConnector)(nil)
