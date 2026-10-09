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
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connectors/powerschool"
	pslib "github.com/leftathome/powerschool-go"
)

// healthAddr is where the framework's health server listens
// (connector.Options.HealthPort defaults to 8080) and therefore where the
// stand-in must listen while the binary waits for a session.
const healthAddr = ":8080"

func main() {
	// Logged with every escalation; read from the binary's build info so it
	// cannot drift from go.mod.
	libVersion := powerschool.LibraryVersion()

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

	if err := powerschool.CheckLibrary(); err != nil {
		slog.Error("powerschool: linked library is not the one this connector was reviewed against",
			"library_version", libVersion, "error", err)
		os.Exit(1)
	}

	// If the portal already refused this exact session, do not present it
	// again: wait here, making no upstream requests, until the session
	// source holds a different one (the login workload writes a new session
	// and the mounted Secret is updated in place). A session delivered
	// through the POWERSCHOOL_SESSION environment variable cannot change
	// under a running process; there the wait lasts until the pod is
	// replaced, which is still the right amount of upstream traffic: none.
	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")
	rejected := connector.NewRejectedSession(stateDir)
	// While it waits the framework's health server is not up. Answer the
	// kubelet's probes ourselves (alive, not ready) so the pod is not
	// restarted for waiting, and release the port before connector.Run
	// binds it.
	var waitHealth *powerschool.WaitHealth
	session, err := powerschool.WaitForFreshSession(context.Background(), rejected,
		func() (*pslib.SessionExport, error) { return powerschool.LoadSession(os.Getenv, time.Now()) },
		time.Minute,
		func() {
			h, err := powerschool.StartWaitHealth(healthAddr)
			if err != nil {
				slog.Error("powerschool: could not serve health endpoints while waiting", "error", err)
				return
			}
			waitHealth = h
		})
	if stopErr := waitHealth.Stop(); stopErr != nil {
		slog.Error("powerschool: could not release the health port", "error", stopErr)
		os.Exit(1)
	}
	if err != nil {
		// Missing, malformed or expired: the login job must produce a
		// fresh session before the connector can come up.
		slog.Error("powerschool: load session", "error", err)
		os.Exit(1)
	}
	sessionFP := powerschool.SessionFingerprint(session)

	// The session belongs to a real parent account. Every request the
	// library makes goes through a transport pinned to this host that
	// refuses anything but GET/HEAD, with an allowlist of two POSTs that
	// change no record: the assignment-lookup query (only while the
	// assignments surface is enabled) and the portal's student-switcher
	// form (only for the student ids configured here). No credentials are
	// passed, so the library cannot re-authenticate.
	studentIDs := make([]string, 0, len(cfg.Kids))
	for _, kid := range cfg.Kids {
		studentIDs = append(studentIDs, kid.StudentID)
	}
	httpClient := powerschool.NewHTTPClient(host, cfg.AssignmentsEnabled(), studentIDs, 30*time.Second)
	lib, err := pslib.NewClient(baseURL,
		pslib.WithHTTPClient(httpClient),
		pslib.WithHTTPPageFetch(),
		pslib.WithSession(session.Cookies, session.ExpiresAt),
		pslib.WithLogLevel(pslib.LogLevelWarn),
	)
	if err != nil {
		slog.Error("powerschool: construct library client", "error", err)
		os.Exit(1)
	}

	client, err := powerschool.NewProductionClient(lib)
	if err != nil {
		slog.Error("powerschool: library cannot honour the read-only transport",
			"library_version", libVersion, "error", err)
		os.Exit(1)
	}

	c, err := powerschool.NewConnector(client, cfg, libVersion)
	if err != nil {
		slog.Error("powerschool: construct connector", "error", err)
		os.Exit(1)
	}
	clock := powerschool.SessionClock{Dir: stateDir}
	c.SessionFirstSeen = clock.FirstSeen(sessionFP, time.Now())
	if age, ok := clock.LastRejectedAge(); ok {
		c.PreviousRejectedAge = age
	}
	c.OnSessionRejected = func(age time.Duration) {
		if err := rejected.Mark(sessionFP); err != nil {
			slog.Error("powerschool: could not record rejected session", "error", err)
		}
		if err := clock.SaveRejectedAge(age); err != nil {
			slog.Error("powerschool: could not record the rejected session's age", "error", err)
		}
	}

	connector.Run(connector.Options{
		Name:       "powerschool",
		Tier:       connector.TierPersonal,
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
		Connector:  c,
		// The windowed schedule in Watch decides when to poll. The
		// framework's periodic re-poll both calls Poll and restarts Watch;
		// push it out past the pod's natural lifetime. Poll is also gated
		// to its first call per process, and skips that one when no window
		// has opened since the last recorded poll.
		PollInterval: 30 * 24 * time.Hour,
		Setup: func(cc connector.ConnectorContext) error {
			return c.Wire(cc)
		},
	})
}
