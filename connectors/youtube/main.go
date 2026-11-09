package main

import (
	"encoding/json"
	"log/slog"
	"os"

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

	apiKey := os.Getenv("YOUTUBE_API_KEY")
	if apiKey == "" {
		slog.Error("YOUTUBE_API_KEY environment variable is required")
		os.Exit(1)
	}

	pollInterval, err := cfg.EffectivePollInterval()
	if err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	transcripts, err := newTranscriptProvider(cfg.Transcript)
	if err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	if len(cfg.AllChannels()) == 0 {
		slog.Error("invalid config: no channels configured (set \"channels\")")
		os.Exit(1)
	}

	c := &YouTubeConnector{
		config:      cfg,
		httpClient:  connector.NewHTTPClient(connector.HTTPClientOptions{}),
		tokenSource: connector.NewStaticTokenSource(apiKey),
		apiBase:     "https://www.googleapis.com/youtube/v3",
		transcripts: transcripts,
	}

	connector.Run(connector.Options{
		Name:       "youtube",
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
		PollInterval: pollInterval,
	})
}
