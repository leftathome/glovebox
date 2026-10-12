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
// With login_on_demand enabled in the config the session need not exist at
// startup, and the pod needs its ServiceAccount token mounted
// (KUBERNETES_SERVICE_HOST/_PORT, /var/run/secrets/kubernetes.io/serviceaccount):
// when a poll finds the session dead the binary writes a login request
// into one field of one ConfigMap and waits for the new session file.
// With login_on_demand.broker.url set it asks the login broker instead
// (connectors/login-broker), presenting the token at broker.token_file, and
// polls on the session in the answer; it then needs no access to the
// Kubernetes API.
// POWERSCHOOL_LOGIN_BACKOFF_RESET (any new value) clears the count of
// requests that led nowhere, after which the connector asks again.
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
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
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

	stateDir := os.Getenv("GLOVEBOX_STATE_DIR")
	clock := powerschool.SessionClock{Dir: stateDir}

	// The session belongs to a real parent account. Every request the
	// library makes goes through a transport pinned to this host that
	// refuses anything but GET/HEAD, with an allowlist of two POSTs that
	// change no record: the assignment-lookup query (only while the
	// assignments surface is enabled) and the portal's student-switcher
	// form (only for the student ids configured here). No credentials are
	// passed, so the library cannot re-authenticate.
	//
	// A client is built per session: when the session file changes, the
	// gate below builds a new one. Each has its own transport and its own
	// gateway-cookie overlay.
	studentIDs := make([]string, 0, len(cfg.Kids))
	for _, kid := range cfg.Kids {
		studentIDs = append(studentIDs, kid.StudentID)
	}
	source := powerschool.SessionSource{
		Load: func(now time.Time) (*pslib.SessionExport, bool, error) {
			return powerschool.LoadSessionAdvisory(os.Getenv, now)
		},
		Build: func(session *pslib.SessionExport) (powerschool.PowerSchoolClient, powerschool.ProbeFunc, func(), error) {
			httpClient, resetGatewayCookies := powerschool.NewHTTPClientWithReset(host, cfg.AssignmentsEnabled(), studentIDs, 30*time.Second)
			lib, err := pslib.NewClient(baseURL,
				pslib.WithHTTPClient(httpClient),
				pslib.WithHTTPPageFetch(),
				pslib.WithSession(session.Cookies, session.ExpiresAt),
				pslib.WithLogLevel(pslib.LogLevelWarn),
			)
			if err != nil {
				return nil, nil, nil, err
			}
			client, err := powerschool.NewProductionClient(lib)
			if err != nil {
				return nil, nil, nil, err
			}
			return client, powerschool.NewSessionProbe(httpClient, baseURL, session), resetGatewayCookies, nil
		},
		FileAge:   func(now time.Time) (time.Duration, bool) { return powerschool.SessionFileAge(os.Getenv, now) },
		FirstSeen: clock.FirstSeen,
	}

	var c *powerschool.PowerSchoolConnector
	if cfg.LoginOnDemand.Enabled {
		// On-demand login: when a poll finds the session dead (or there is
		// none yet), the connector ASKS for a login and waits for the new
		// session file. Asking is writing one short string into one field
		// of one ConfigMap; the login workload reads it on its own schedule
		// and decides by its own rules. This process does not log in, holds
		// no credentials and cannot start a workload. So there is nothing
		// to wait for at startup and no "rejected" marker: a dead session
		// is simply replaced at the next poll.
		//
		// With login_on_demand.broker.url set it asks the login broker
		// instead of the ConfigMap: one authenticated call, answered with
		// the outcome and the session. The token it presents is projected
		// for the broker's audience only, and the client sends it to the
		// broker's address and nowhere else. The pod then needs no access
		// to the Kubernetes API at all.
		var gate *powerschool.SessionGate
		if cfg.LoginOnDemand.UsesBroker() {
			broker, err := brokerapi.NewClient(cfg.LoginOnDemand.Broker.URL,
				brokerapi.TokenFile(cfg.LoginOnDemand.Broker.TokenFile), cfg.LoginOnDemand.BrokerTimeout(), nil)
			if err != nil {
				slog.Error("powerschool: login_on_demand.broker is not usable", "error", err)
				os.Exit(1)
			}
			slog.Info("powerschool: on-demand login through the login broker is enabled",
				"broker", cfg.LoginOnDemand.Broker.URL,
				"timeout_seconds", int64(cfg.LoginOnDemand.BrokerTimeout()/time.Second))
			gate = powerschool.NewBrokerSessionGate(source, cfg.LoginOnDemand, broker)
		} else {
			kube, err := kubecm.NewInCluster(os.Getenv, kubecm.ServiceAccountDir, cfg.LoginOnDemand.Namespace)
			if err != nil {
				slog.Error("powerschool: login_on_demand is enabled but the Kubernetes API is not usable from this pod",
					"error", err)
				os.Exit(1)
			}
			slog.Info("powerschool: on-demand login is enabled",
				"configmap", cfg.LoginOnDemand.ConfigMapName, "namespace", kube.Namespace(),
				"wait_seconds", cfg.LoginOnDemand.WaitSeconds, "min_interval_minutes", cfg.LoginOnDemand.MinIntervalMinutes)
			requester := powerschool.ConfigMapRequester{Client: kube, ConfigMap: cfg.LoginOnDemand.ConfigMapName}
			gate = powerschool.NewSessionGate(source, cfg.LoginOnDemand, requester)
		}
		// An operator's "try again" after the connector stopped asking.
		gate.BackoffResetToken = os.Getenv("POWERSCHOOL_LOGIN_BACKOFF_RESET")
		var err error
		c, err = powerschool.NewConnectorWithGate(gate, cfg, libVersion)
		if err != nil {
			slog.Error("powerschool: construct connector", "error", err)
			os.Exit(1)
		}
		if age, ok := clock.LastRejectedAge(); ok {
			c.PreviousRejectedAge = age
		}
		gateOnDead := clock.SaveRejectedAge
		c.OnSessionAge = func(age time.Duration) {
			if err := gateOnDead(age); err != nil {
				slog.Error("powerschool: could not record the dead session's age", "error", err)
			}
		}
	} else {
		// Without on-demand login nobody in this process can replace a dead
		// session. If the portal already refused this exact session, do not
		// present it again: wait here, making no upstream requests, until
		// the session source holds a different one (the login workload
		// writes a new session and the mounted Secret is updated in place).
		// A session delivered through the POWERSCHOOL_SESSION environment
		// variable cannot change under a running process; there the wait
		// lasts until the pod is replaced, which is still the right amount
		// of upstream traffic: none.
		rejected := connector.NewRejectedSession(stateDir)
		// While it waits the framework's health server is not up. Answer
		// the kubelet's probes ourselves (alive, not ready) so the pod is
		// not restarted for waiting, and release the port before
		// connector.Run binds it.
		var waitHealth *powerschool.WaitHealth
		_, err := powerschool.WaitForFreshSession(context.Background(), rejected,
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

		// The gate still checks the session at the start of every poll; it
		// just cannot ask for a new one. A dead session takes the
		// rejected-session path: mark it, exit, wait above.
		gate := powerschool.NewSessionGate(source, powerschool.LoginOnDemandConfig{}, nil)
		c, err = powerschool.NewConnectorWithGate(gate, cfg, libVersion)
		if err != nil {
			slog.Error("powerschool: construct connector", "error", err)
			os.Exit(1)
		}
		if age, ok := clock.LastRejectedAge(); ok {
			c.PreviousRejectedAge = age
		}
		c.OnSessionRejected = func(age time.Duration) {
			if fp := gate.Fingerprint(); fp != "" {
				if err := rejected.Mark(fp); err != nil {
					slog.Error("powerschool: could not record rejected session", "error", err)
				}
			}
			if err := clock.SaveRejectedAge(age); err != nil {
				slog.Error("powerschool: could not record the rejected session's age", "error", err)
			}
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
