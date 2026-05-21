// Command schoology is the Glovebox connector binary for the Schoology
// LMS. It loads its operator config (rules + identity + the
// schoology-specific kids/poll_schedule/trigger/attachments sections)
// from $GLOVEBOX_CONNECTOR_CONFIG, loads saved Schoology session
// credentials from $SCHOOLOGY_CREDENTIALS_FILE, constructs a schoology
// library client, and hands it all off to the framework's
// connector.Run.
//
// Required environment:
//
//	GLOVEBOX_CONNECTOR_CONFIG   path to the JSON config (default
//	                            /etc/connector/config.json)
//	SCHOOLOGY_HOST              Schoology tenant subdomain
//	                            (e.g. yourschool.schoology.com)
//	SCHOOLOGY_CREDENTIALS_FILE  path to a JSON credentials file written
//	                            by schoology-go's auth.SaveCredentials
//	                            (see docs/AUTH-RECOVERY.md)
//
// Optional environment (consumed inside the schoology connector or
// framework):
//
//	GLOVEBOX_STAGING_DIR        filesystem staging root (filesystem mode)
//	GLOVEBOX_INGEST_URL         HTTP ingest URL (takes precedence over
//	                            GLOVEBOX_STAGING_DIR)
//	GLOVEBOX_STATE_DIR          checkpoint root
//	SCHOOLOGY_TIMEZONE          scheduler timezone, default America/Los_Angeles
//	SCHOOLOGY_TRIGGER_TOKEN     bearer token for POST /v1/poll
//
// Fatal startup errors (missing config, malformed config, missing or
// invalid credentials) log a single ERROR line referencing
// docs/AUTH-RECOVERY.md where appropriate and exit non-zero. Once the
// framework takes over, transient errors are logged and retried; permanent
// errors (schema drift escalation, session expiry detected mid-poll)
// cause connector.Run to os.Exit(1).
package main

import (
	"encoding/json"
	"log/slog"
	"os"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connectors/schoology"
	schoologylib "github.com/leftathome/schoology-go"
	schoologyauth "github.com/leftathome/schoology-go/auth"
)

// schoologyLibVersion is the version string embedded in parse-failure
// receipts so operators can correlate breakage to a specific upstream
// commit. It must be kept in sync with the go.mod pin for
// github.com/leftathome/schoology-go.
const schoologyLibVersion = "v0.1.0"

func main() {
	configFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
	if configFile == "" {
		configFile = "/etc/connector/config.json"
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		slog.Error("schoology: read config", "path", configFile, "error", err)
		os.Exit(1)
	}

	var cfg schoology.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("schoology: parse config", "path", configFile, "error", err)
		os.Exit(1)
	}

	schoology.ApplyDefaults(&cfg)
	if err := schoology.ValidateConfig(&cfg); err != nil {
		slog.Error("schoology: validate config", "error", err)
		os.Exit(1)
	}

	credsPath := os.Getenv("SCHOOLOGY_CREDENTIALS_FILE")
	if credsPath == "" {
		slog.Error("schoology: SCHOOLOGY_CREDENTIALS_FILE is required",
			"recovery_doc", "docs/AUTH-RECOVERY.md")
		os.Exit(1)
	}

	host := os.Getenv("SCHOOLOGY_HOST")
	if host == "" {
		slog.Error("schoology: SCHOOLOGY_HOST is required",
			"recovery_doc", "docs/AUTH-RECOVERY.md")
		os.Exit(1)
	}

	creds, err := schoologyauth.LoadCredentials(credsPath)
	if err != nil {
		// Treat any LoadCredentials failure (missing file, malformed
		// JSON, missing required field) as a permanent startup
		// condition: the operator must run the recovery procedure
		// before the connector can come up.
		slog.Error("schoology: load credentials",
			"path", credsPath,
			"error", err,
			"recovery_doc", "docs/AUTH-RECOVERY.md")
		os.Exit(1)
	}

	lib, err := schoologylib.NewClient(host, schoologylib.WithSession(
		creds.SessID, creds.CSRFToken, creds.CSRFKey, creds.UID,
	))
	if err != nil {
		slog.Error("schoology: construct library client",
			"host", host,
			"error", err,
			"recovery_doc", "docs/AUTH-RECOVERY.md")
		os.Exit(1)
	}

	client := schoology.NewProductionClient(lib)

	c, err := schoology.NewConnector(client, cfg, schoologyLibVersion)
	if err != nil {
		slog.Error("schoology: construct connector", "error", err)
		os.Exit(1)
	}

	connector.Run(connector.Options{
		Name:       "schoology",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
		Connector:  c,
		Setup: func(cc connector.ConnectorContext) error {
			return c.Wire(cc)
		},
	})
}
