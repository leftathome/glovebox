package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
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

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		slog.Error("GITHUB_TOKEN environment variable is required")
		os.Exit(1)
	}

	var webhookSecret []byte
	if cfg.WebhookSecret != "" {
		secret := os.Getenv(cfg.WebhookSecret)
		if secret != "" {
			webhookSecret = []byte(secret)
		}
	}

	c := &GitHubConnector{
		config:        cfg,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		tokenSource:   connector.NewStaticTokenSource(token),
		apiBase:       "https://api.github.com",
		webhookSecret: webhookSecret,
	}

	connector.Run(connector.Options{
		Name:       "github",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
		Connector:  c,
		Setup: func(cc connector.ConnectorContext) error {
			c.writer = cc.Writer
			c.matcher = cc.Matcher
			if cfg.ConfigIdentity != nil {
				cc.Writer.SetConfigIdentity(cfg.ConfigIdentity)
			}
			return nil
		},
		PollInterval: 5 * time.Minute,
	})
}
