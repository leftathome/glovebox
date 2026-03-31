package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func main() {
	c := &IMAPConnector{}

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

	c.config = cfg
	c.newClient = newRealClient
	c.imapUsername = os.Getenv("IMAP_USERNAME")

	connector.Run(connector.Options{
		Name:       "imap",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
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
