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

	// Default to ["primary"] if no calendar IDs configured.
	if len(cfg.CalendarIDs) == 0 {
		cfg.CalendarIDs = []string{"primary"}
	}

	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		slog.Error("GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET environment variables are required")
		os.Exit(1)
	}

	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")
	if stateDir == "" {
		slog.Error("GLOVEBOX_STATE_DIR environment variable is required")
		os.Exit(1)
	}

	oauthCfg := connector.OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     "https://oauth2.googleapis.com/token",
		Scopes:       []string{"https://www.googleapis.com/auth/calendar.readonly"},
	}

	tokenSource, err := connector.NewRefreshableTokenSource(oauthCfg, stateDir)
	if err != nil {
		slog.Error("init token source", "error", err)
		os.Exit(1)
	}

	c := &GCalendarConnector{
		config:      cfg,
		httpClient:  connector.NewHTTPClient(connector.HTTPClientOptions{}),
		tokenSource: tokenSource,
		apiBase:     "https://www.googleapis.com",
	}

	connector.Run(connector.Options{
		Name:       "gcalendar",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   stateDir,
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

// Ensure GCalendarConnector does not accidentally get a Handler method
// that would cause it to implement connector.Listener.
var _ http.Handler // import guard -- unused but keeps http imported for NewHTTPClient
