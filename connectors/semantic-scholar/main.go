package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"

	// Content enrichers registered via init() per docs/specs/14-content-enrichment-design.md sec 6.2.
	// semantic-scholar items are predominantly PDFs; ocr/office intentionally NOT imported
	// (academic papers don't need image OCR or office-doc extraction).
	_ "github.com/leftathome/glovebox/connector/enrich/html"
	_ "github.com/leftathome/glovebox/connector/enrich/passthrough"
	_ "github.com/leftathome/glovebox/connector/enrich/pdf"
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

	apiKey := os.Getenv("SEMANTIC_SCHOLAR_API_KEY")
	httpClient := connector.NewHTTPClient(connector.HTTPClientOptions{})

	c := &SemanticScholarConnector{
		config:     cfg,
		httpClient: httpClient,
		apiKey:     apiKey,
	}

	connector.Run(connector.Options{
		Name:       "semantic-scholar",
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
