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

	if cfg.Service == "" {
		cfg.Service = "https://bsky.social"
	}

	identifier := os.Getenv("BLUESKY_IDENTIFIER")
	if identifier == "" {
		slog.Error("BLUESKY_IDENTIFIER env var is required")
		os.Exit(1)
	}

	appPassword := os.Getenv("BLUESKY_APP_PASSWORD")
	if appPassword == "" {
		slog.Error("BLUESKY_APP_PASSWORD env var is required")
		os.Exit(1)
	}

	c := &BlueskyConnector{
		config:      cfg,
		identifier:  identifier,
		appPassword: appPassword,
		httpClient: connector.NewHTTPClient(connector.HTTPClientOptions{}),
	}

	connector.Run(connector.Options{
		Name:       "bluesky",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
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
