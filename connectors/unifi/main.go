package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func main() {
	configFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
	if configFile == "" {
		configFile = "/etc/connector/config.json"
	}

	cfg, err := loadConfig(configFile)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	apiKey := os.Getenv(cfg.APIKeyEnv)
	if apiKey == "" {
		slog.Error("the UniFi API key is required", "env", cfg.APIKeyEnv)
		os.Exit(1)
	}

	var webhookSecret []byte
	if cfg.WebhookSecretEnv != "" {
		if v := os.Getenv(cfg.WebhookSecretEnv); v != "" {
			webhookSecret = []byte(v)
		}
	}
	if len(webhookSecret) == 0 {
		// Not fatal: poll-only operation is a legitimate deployment. The
		// listener refuses everything until a secret exists, so say so once
		// at startup rather than only in a rejected request.
		slog.Warn("no webhook secret configured; the listener will refuse every push",
			"env", cfg.WebhookSecretEnv)
	}

	httpClient, err := newHTTPClient(cfg)
	if err != nil {
		slog.Error("build http client", "error", err)
		os.Exit(1)
	}
	if cfg.InsecureSkipVerify {
		slog.Warn("TLS verification of the controller is disabled; the API key is the only thing authenticating it",
			"controller", cfg.ControllerURL)
	}

	c := &UniFiConnector{
		config:        cfg,
		httpClient:    httpClient,
		apiKey:        apiKey,
		webhookSecret: webhookSecret,
	}

	connector.Run(runOptions(cfg, c, configFile))
}

// runOptions builds the framework options.
//
// It is a function rather than a literal inside main so that the tier
// declaration is reachable from a test. A connector that silently declared
// TierPersonal would compile, start, pass every other test, and quietly
// repopulate the per-agent recall index that caro exists to keep clean.
func runOptions(cfg Config, c *UniFiConnector, configFile string) connector.Options {
	return connector.Options{
		Name: "unifi",

		// UniFi event volume is far above RSS, and RSS alone measured 89% of
		// the main agent's memory index before diversion existed. TierFeed is
		// what makes openclaw's triage divert these to caro instead of
		// writing them into the audiences tree that every person-agent has on
		// memorySearch extraPaths. See connector/tier.go.
		Tier: connector.TierFeed,

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
	}
}

// loadConfig reads, defaults and validates the connector configuration.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects a configuration that cannot work, at startup, rather than
// letting it fail once per poll forever.
func (c *Config) validate() error {
	if c.ControllerURL == "" {
		return fmt.Errorf("controller_url is required")
	}
	if !strings.HasPrefix(c.ControllerURL, "http://") && !strings.HasPrefix(c.ControllerURL, "https://") {
		return fmt.Errorf("controller_url %q must start with http:// or https://", c.ControllerURL)
	}
	c.ControllerURL = strings.TrimSuffix(c.ControllerURL, "/")

	if !c.Network.Enabled && !c.Protect.Enabled {
		return fmt.Errorf("neither network.enabled nor protect.enabled is set; this connector would do nothing")
	}
	if c.CACertFile != "" && c.InsecureSkipVerify {
		return fmt.Errorf("ca_cert_file and insecure_skip_verify are mutually exclusive; pick one")
	}
	switch c.WebhookAuthMode {
	case authModeHMAC, authModeBearer:
	default:
		return fmt.Errorf("webhook_auth_mode %q must be %q or %q", c.WebhookAuthMode, authModeHMAC, authModeBearer)
	}
	if strings.Count(c.Network.EventsPath, "%s") != 1 {
		return fmt.Errorf("network.events_path must contain exactly one %%s for the site id")
	}
	return nil
}
