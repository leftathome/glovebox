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
//	SCHOOLOGY_TIMEZONE          timezone of the poll schedule and of the
//	                            due/posted times Schoology prints, default
//	                            America/Los_Angeles
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
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connectors/schoology"
	schoologylib "github.com/leftathome/schoology-go"
	schoologyauth "github.com/leftathome/schoology-go/auth"
	"github.com/prometheus/client_golang/prometheus"
)

// healthAddr is where the framework's health server listens
// (connector.Options.HealthPort defaults to 8080) and therefore where the
// stand-in must listen while the binary waits for a session.
const healthAddr = ":8080"

// listenerAddr is the trigger listener's port (HealthPort+1). While the
// binary waits for a session the stand-in answers it 503.
const listenerAddr = ":8081"

// sessionStateFile, under GLOVEBOX_STATE_DIR, keeps the reported session
// state and when it began (connector.SessionState.PersistTo).
const sessionStateFile = "session-state"

// schoologyLibVersion is the version string embedded in parse-failure
// receipts so operators can correlate breakage to a specific upstream
// commit. It must be kept in sync with the go.mod pin for
// github.com/leftathome/schoology-go.
const schoologyLibVersion = "v0.3.0"

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

	host := schoology.NormalizeHost(os.Getenv("SCHOOLOGY_HOST"))
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

	// The session state -- unknown, or waiting for a new session -- is
	// reported as schoology_session_state on /metrics and the "session"
	// condition on /statusz. It is not readiness (glovebox-wekc.17).
	status := connector.NewStatus()
	sessionState, err := connector.NewSessionState(prometheus.DefaultRegisterer, "schoology", status)
	if err != nil {
		slog.Error("schoology: register session state metrics", "error", err)
		os.Exit(1)
	}
	if dir := os.Getenv("GLOVEBOX_STATE_DIR"); dir != "" {
		sessionState.PersistTo(filepath.Join(dir, sessionStateFile))
	}

	// If Schoology already refused this exact session, do not present it
	// again: wait here, making no upstream requests, until the credentials
	// file holds a different one (the refresher writes a new session and
	// ESO updates the mounted Secret in place). The framework's health
	// server is not up yet, so a stand-in answers the probes: alive and
	// ready -- waiting is what the process should be doing, and readiness
	// gates every Helm upgrade and rollback of the release. The operator
	// signal is the ERROR line, schoology_session_state and /statusz.
	rejected := schoology.NewRejectedSession(os.Getenv("GLOVEBOX_STATE_DIR"))
	creds, err = schoology.WaitForFreshCredentials(context.Background(), rejected, creds,
		func() (*schoologyauth.Credentials, error) { return schoologyauth.LoadCredentials(credsPath) },
		schoology.SessionWaitOptions{
			HealthAddr: healthAddr, ListenerAddr: listenerAddr, Interval: time.Minute,
			Status: status, State: sessionState,
		})
	if err != nil {
		slog.Error("schoology: wait for a new session", "error", err)
		os.Exit(1)
	}
	sessionFP := schoology.SessionFingerprint(creds.SessID)

	// The session belongs to a real parent account that can post and
	// message. Every request this process makes goes through a transport
	// that refuses anything but GET/HEAD. WithHTTPClient must precede
	// WithSession: the session cookie is written into this client's jar.
	httpClient, err := connector.NewReadOnlyHTTPClient("schoology", 30*time.Second)
	if err != nil {
		slog.Error("schoology: build read-only http client", "error", err)
		os.Exit(1)
	}
	// Schoology prints due and posted times as wall-clock text without
	// naming a zone. The library reads them in this location, the same one
	// the poll schedule uses; without it they would stay unparsed.
	tz, err := schoology.Timezone()
	if err != nil {
		slog.Error("schoology: load timezone", "error", err)
		os.Exit(1)
	}
	lib, err := schoologylib.NewClient(host,
		schoologylib.WithHTTPClient(httpClient),
		schoologylib.WithSession(creds.SessID, creds.CSRFToken, creds.CSRFKey, creds.UID),
		schoologylib.WithLocation(tz),
	)
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
	c.OnSessionRejected = func() {
		sessionState.Set(connector.SessionStateWaiting, time.Time{}, "Schoology refused the session")
		if err := rejected.Mark(sessionFP); err != nil {
			slog.Error("schoology: could not record rejected session", "error", err)
		}
	}

	connector.Run(runOptions(c, configFile, status, os.Getenv))
}

// runOptions is what the binary hands connector.Run.
func runOptions(c *schoology.SchoologyConnector, configFile string, status *connector.Status, getenv func(string) string) connector.Options {
	return connector.Options{
		Name:       "schoology",
		Tier:       connector.TierPersonal,
		StagingDir: getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
		Connector:  c,
		// Readiness stays ReadyAfterFirstPoll here: the catch-up poll is
		// bounded work that returns nil on transient failures, so it cannot
		// hold readiness false for a non-fault condition, and readiness also
		// decides when the trigger listener's Service sends it traffic.
		Status: status,
		// The windowed schedule in Watch decides when to poll. The framework's
		// periodic re-poll (default 5m) both called Poll and restarted Watch,
		// which re-rolled the in-window splay each time and could skip a
		// window outright. Push it out past the pod's natural lifetime (the
		// session refresh rolls the pod about every 12 days); Poll is also
		// gated to its first call.
		PollInterval: 30 * 24 * time.Hour,
		Setup: func(cc connector.ConnectorContext) error {
			return c.Wire(cc)
		},
	}
}
