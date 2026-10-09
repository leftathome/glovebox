// Command powerschool is the Glovebox connector binary for the PowerSchool
// parent portal. It loads its operator config from
// $GLOVEBOX_CONNECTOR_CONFIG, loads a saved portal session, constructs a
// powerschool-go client on a read-only transport, and hands it all off to
// the framework's connector.Run.
//
// Required environment:
//
//	GLOVEBOX_CONNECTOR_CONFIG   path to the JSON config (default
//	                            /etc/connector/config.json)
//	POWERSCHOOL_HOST            portal location, bare host or full https
//	                            URL (e.g. https://ps.example.org)
//	POWERSCHOOL_SESSION_FILE    path to the session JSON written by the
//	                            login workload, or
//	POWERSCHOOL_SESSION         the session JSON itself
//
// Optional environment:
//
//	GLOVEBOX_STAGING_DIR        filesystem staging root (filesystem mode)
//	GLOVEBOX_INGEST_URL         HTTP ingest URL (takes precedence)
//	GLOVEBOX_STATE_DIR          checkpoint root
//	POWERSCHOOL_TIMEZONE        scheduler / due-date timezone, default
//	                            America/Los_Angeles
//
// This binary never reads a username or password and never logs in; see
// docs/connectors/powerschool.md. Fatal startup errors log one ERROR line
// and exit non-zero.
package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
)

// powerschoolLibVersion is logged with every escalation. It must be kept
// in sync with the go.mod pin for github.com/leftathome/powerschool-go.
const powerschoolLibVersion = "v0.1.0"

func main() {
	configFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
	if configFile == "" {
		configFile = "/etc/connector/config.json"
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		slog.Error("powerschool: read config", "path", configFile, "error", err)
		os.Exit(1)
	}

	var cfg powerschool.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("powerschool: parse config", "path", configFile, "error", err)
		os.Exit(1)
	}

	powerschool.ApplyDefaults(&cfg)
	if err := powerschool.ValidateConfig(&cfg); err != nil {
		slog.Error("powerschool: validate config", "error", err)
		os.Exit(1)
	}

	baseURL, host, err := powerschool.NormalizeBaseURL(os.Getenv("POWERSCHOOL_HOST"))
	if err != nil {
		slog.Error("powerschool: POWERSCHOOL_HOST is required", "error", err)
		os.Exit(1)
	}

	session, err := powerschool.LoadSession(os.Getenv, time.Now())
	if err != nil {
		// Missing, malformed or expired: the login workload must produce
		// a fresh session before the connector can come up.
		slog.Error("powerschool: load session", "error", err)
		os.Exit(1)
	}

	// The session belongs to a real parent account. Every request the
	// library makes goes through a transport pinned to this host that
	// refuses anything but GET/HEAD, plus the assignment-lookup query
	// when (and only when) the assignments surface is enabled. No
	// credentials are passed, so the library cannot re-authenticate.
	httpClient := powerschool.NewHTTPClient(host, cfg.AssignmentsEnabled(), 30*time.Second)
	lib, err := pslib.NewClient(baseURL,
		pslib.WithHTTPClient(httpClient),
		pslib.WithSession(session.Cookies, session.ExpiresAt),
		pslib.WithLogLevel(pslib.LogLevelWarn),
	)
	if err != nil {
		slog.Error("powerschool: construct library client", "error", err)
		os.Exit(1)
	}

	client, err := powerschool.NewProductionClient(lib)
	if err != nil {
		// With powerschool-go v0.1.0 this is where the binary stops: its
		// page reads would start a headless browser the transport above
		// cannot see.
		slog.Error("powerschool: library cannot honour the read-only transport",
			"library_version", powerschoolLibVersion, "error", err)
		os.Exit(1)
	}

	c, err := powerschool.NewConnector(client, cfg, powerschoolLibVersion)
	if err != nil {
		slog.Error("powerschool: construct connector", "error", err)
		os.Exit(1)
	}

	connector.Run(connector.Options{
		Name:       "powerschool",
		Tier:       connector.TierPersonal,
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
		Connector:  c,
		// The window scheduler in Watch drives the daily poll. The
		// framework also re-polls on this interval; keep it at a day so
		// the portal is not read every few minutes.
		PollInterval: 24 * time.Hour,
		Setup: func(cc connector.ConnectorContext) error {
			return c.Wire(cc)
		},
	})
}
