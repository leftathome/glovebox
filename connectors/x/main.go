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

	bearerToken := os.Getenv("X_BEARER_TOKEN")
	if bearerToken == "" {
		slog.Error("X_BEARER_TOKEN environment variable is required")
		os.Exit(1)
	}

	var webhookSecret []byte
	secret := os.Getenv("X_WEBHOOK_SECRET")
	if secret != "" {
		webhookSecret = []byte(secret)
	}

	c := &XConnector{
		config:        cfg,
		httpClient:    connector.NewHTTPClient(connector.HTTPClientOptions{}),
		tokenSource:   connector.NewStaticTokenSource(bearerToken),
		apiBase:       "https://api.x.com",
		webhookSecret: webhookSecret,
	}

	connector.Run(connector.Options{
		Name:       "x",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
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
