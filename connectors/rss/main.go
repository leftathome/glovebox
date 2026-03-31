package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
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

	httpClient := connector.NewHTTPClient(connector.HTTPClientOptions{})
	rc := connector.NewRobotsChecker(httpClient)

	c := &RSSConnector{
		config:        cfg,
		linkPolicy:    content.NewLinkPolicy(cfg.LinkPolicy),
		httpClient:    httpClient,
		robotsChecker: rc,
	}

	connector.Run(connector.Options{
		Name:       "rss",
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
		PollInterval: 15 * time.Minute,
	})
}
