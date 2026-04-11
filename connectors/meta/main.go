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

	accessToken := os.Getenv("META_ACCESS_TOKEN")
	if accessToken == "" {
		slog.Error("META_ACCESS_TOKEN environment variable is required")
		os.Exit(1)
	}

	var appSecret []byte
	if secret := os.Getenv("META_APP_SECRET"); secret != "" {
		appSecret = []byte(secret)
	}

	verifyToken := os.Getenv("META_VERIFY_TOKEN")

	c := &MetaConnector{
		config:      cfg,
		httpClient:  connector.NewHTTPClient(connector.HTTPClientOptions{}),
		tokenSource: connector.NewStaticTokenSource(accessToken),
		apiBase:     "https://graph.facebook.com",
		appSecret:   appSecret,
		verifyToken: verifyToken,
	}

	connector.Run(connector.Options{
		Name:       "meta",
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
