package main

import (
	"encoding/json"
	"errors"
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
	linkPolicy := content.NewLinkPolicy(cfg.LinkPolicy)

	// Links extracted from feed content are attacker-influenced, so they
	// get the guarded client: connect-time address validation plus a
	// policy re-check on every redirect hop. In unrestricted mode the
	// operator has explicitly opted out of address filtering, so the guard
	// allows private destinations while still capping and re-checking
	// redirects.
	linkClient := connector.NewGuardedHTTPClient(connector.GuardedClientOptions{
		AllowPrivateNetworks: cfg.LinkPolicy.Default == "unrestricted",
		ValidateURL: func(rawURL string) error {
			if allowed, reason := linkPolicy.Check(rawURL); !allowed {
				return errors.New(reason)
			}
			return nil
		},
	})
	rc := connector.NewRobotsChecker(linkClient)

	c := &RSSConnector{
		config:        cfg,
		linkPolicy:    linkPolicy,
		httpClient:    httpClient,
		linkClient:    linkClient,
		robotsChecker: rc,
	}

	connector.Run(connector.Options{
		Name:       "rss",
		Tier:       connector.TierFeed,
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
		PollInterval: 15 * time.Minute,
	})
}
