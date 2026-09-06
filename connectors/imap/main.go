package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"

	// Blank-imported enrichers register themselves with enrich.Default via
	// each subpackage's init(). The set below matches
	// docs/specs/14-content-enrichment-design.md §6.2 (imap row):
	// passthrough + html + pdf + ocr + office. Removing an import simply
	// removes that enricher from this connector's pipeline.
	_ "github.com/leftathome/glovebox/connector/enrich/html"
	_ "github.com/leftathome/glovebox/connector/enrich/ocr"
	_ "github.com/leftathome/glovebox/connector/enrich/office"
	_ "github.com/leftathome/glovebox/connector/enrich/passthrough"
	_ "github.com/leftathome/glovebox/connector/enrich/pdf"
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
		Tier:       connector.TierPersonal,
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: cfgFile,
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
