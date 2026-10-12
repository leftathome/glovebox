// Command login-broker is a small always-on service that logs in to one
// site, for one account, when a connector that found its session dead asks
// for a new one. It holds the account's credentials so that the connector
// -- which parses pages from the site and has to be assumed compromisable
// -- never does.
//
// One process serves one site. The same image runs once per site, each
// instance with its own ServiceAccount, Vault role and egress policy, so
// that taking over one instance exposes one account.
//
//	POST /v1/session/refresh   on LOGIN_BROKER_LISTEN; a ServiceAccount
//	                           token for this broker's audience, from an
//	                           allow-listed ServiceAccount
//	GET  /healthz              on both listeners
//	GET  /metrics              on LOGIN_BROKER_METRICS_LISTEN
//
// The rules every login is subject to, the driver contract and the threat
// model are in internal/loginbroker/DESIGN.md.
//
// Environment (all sites):
//
//	LOGIN_BROKER_SITE              which driver: powerschool
//	LOGIN_BROKER_AUDIENCE          the audience callers' tokens must be
//	                               minted for; this instance's own
//	LOGIN_BROKER_ALLOWED_CALLERS   ServiceAccounts that may ask,
//	                               "namespace/name,namespace/name"
//	LOGIN_BROKER_USERNAME          the account
//	LOGIN_BROKER_PASSWORD
//	LOGIN_BROKER_LISTEN            default :8080
//	LOGIN_BROKER_METRICS_LISTEN    default :9090
//	LOGIN_BROKER_MIN_INTERVAL      least time between logins, Go duration;
//	                               default 20m; a site may set a floor
//	LOGIN_BROKER_MAX_FAILURES      failed logins in a row before the latch;
//	                               default 2, at most 5
//	LOGIN_BROKER_BLACKOUT_WINDOWS  daily windows with no login,
//	                               "HH:MM-HH:MM,HH:MM-HH:MM"; added to the
//	                               site's own
//	LOGIN_BROKER_BLACKOUT_TZ       IANA zone of those windows; required
//	                               with them
//	VAULT_ADDR                     https only
//	VAULT_CACERT                   CA bundle for Vault's certificate
//	VAULT_K8S_ROLE                 this instance's own role
//	VAULT_KV_MOUNT                 default secret
//	VAULT_SESSION_PATH             where the session is written
//	VAULT_STATE_PATH               default: "login-state" beside it
//
// Site powerschool:
//
//	POWERSCHOOL_HOSTNAME           portal location, bare host or https URL
//	POWERSCHOOL_SESSION_LIFETIME   Go duration, default 36h
//	POWERSCHOOL_CHROME_NO_SANDBOX  true to start Chromium with --no-sandbox
//	                               (the image sets it; see the Dockerfile)
//
// No secret is taken from, or written to, the command line, a log line, an
// error string or a Kubernetes event.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Blackout windows are evaluated in a named zone. Embedding the zone
	// database means that cannot fail, or silently become UTC, because an
	// image lacks /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/leftathome/glovebox/internal/dailywindow"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/loginbroker"
	"github.com/leftathome/glovebox/internal/loginvault"
)

const (
	defaultListen        = ":8080"
	defaultMetricsListen = ":9090"
	defaultVaultKVMount  = "secret"
	stateLeaf            = "login-state"
	// shutdownGrace is how long the HTTP servers get to finish answering on
	// SIGTERM. A refresh in flight is waited for separately, to its own
	// deadline: a login is not abandoned halfway.
	shutdownGrace = 20 * time.Second
)

// secretEnv are the variables removed from this process's environment as
// soon as they have been read, so that a browser a driver starts later does
// not inherit them.
var secretEnv = []string{"LOGIN_BROKER_PASSWORD", "LOGIN_BROKER_USERNAME", "VAULT_TOKEN"}

// config is everything the process needs from the environment.
type config struct {
	Site           string
	Listen         string
	MetricsListen  string
	Audience       string
	AllowedCallers []string
	Credentials    loginbroker.Credentials

	MinInterval time.Duration
	MaxFailures int
	Blackout    dailywindow.Schedule

	Vault loginvault.Config
}

// loadConfig reads and validates the environment shared by every site.
// problems lists every fault found, by setting name; it never contains a
// value, since any of them may be a secret typed into the wrong field.
func loadConfig(getenv func(string) string) (cfg config, problems []string) {
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	orDefault := func(name, def string) string {
		if v := get(name); v != "" {
			return v
		}
		return def
	}

	cfg.Site = get("LOGIN_BROKER_SITE")
	if cfg.Site == "" {
		bad("LOGIN_BROKER_SITE is not set")
	} else if _, ok := drivers[cfg.Site]; !ok {
		bad("LOGIN_BROKER_SITE names no driver in this binary (known: %s)", strings.Join(driverNames(), ", "))
	}
	cfg.Listen = orDefault("LOGIN_BROKER_LISTEN", defaultListen)
	cfg.MetricsListen = orDefault("LOGIN_BROKER_METRICS_LISTEN", defaultMetricsListen)
	if cfg.Listen == cfg.MetricsListen {
		bad("LOGIN_BROKER_LISTEN and LOGIN_BROKER_METRICS_LISTEN must differ: the metrics listener is admitted to scrapers, the other only to callers")
	}

	cfg.Audience = get("LOGIN_BROKER_AUDIENCE")
	if cfg.Audience == "" {
		bad("LOGIN_BROKER_AUDIENCE is not set")
	}
	for _, c := range strings.Split(getenv("LOGIN_BROKER_ALLOWED_CALLERS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cfg.AllowedCallers = append(cfg.AllowedCallers, c)
		}
	}
	if len(cfg.AllowedCallers) == 0 {
		bad("LOGIN_BROKER_ALLOWED_CALLERS is not set: nobody could ask for a login")
	}

	cfg.Credentials.Username = getenv("LOGIN_BROKER_USERNAME")
	if cfg.Credentials.Username == "" {
		bad("LOGIN_BROKER_USERNAME is not set")
	}
	cfg.Credentials.Password = getenv("LOGIN_BROKER_PASSWORD")
	if cfg.Credentials.Password == "" {
		bad("LOGIN_BROKER_PASSWORD is not set")
	}

	cfg.MinInterval = loginbroker.DefaultMinInterval
	if raw := get("LOGIN_BROKER_MIN_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			bad("LOGIN_BROKER_MIN_INTERVAL is not a duration such as 20m")
		case d < loginbroker.FloorMinInterval:
			bad("LOGIN_BROKER_MIN_INTERVAL must be at least %s", loginbroker.FloorMinInterval)
		default:
			cfg.MinInterval = d
		}
	}
	cfg.MaxFailures = loginbroker.DefaultMaxFailures
	if raw := get("LOGIN_BROKER_MAX_FAILURES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > loginbroker.CeilingMaxFailures {
			bad("LOGIN_BROKER_MAX_FAILURES must be between 1 and %d", loginbroker.CeilingMaxFailures)
		} else {
			cfg.MaxFailures = n
		}
	}

	windows, err := dailywindow.ParseList(getenv("LOGIN_BROKER_BLACKOUT_WINDOWS"))
	if err != nil {
		bad("LOGIN_BROKER_BLACKOUT_WINDOWS: %v", err)
	}
	zone := get("LOGIN_BROKER_BLACKOUT_TZ")
	switch {
	case len(windows) > 0 && zone == "":
		bad("LOGIN_BROKER_BLACKOUT_TZ is required with LOGIN_BROKER_BLACKOUT_WINDOWS: a window means nothing without a zone")
	case len(windows) > 0:
		// "Local" would be whatever the node's zone is.
		loc, lerr := time.LoadLocation(zone)
		if lerr != nil || zone == "Local" {
			bad("LOGIN_BROKER_BLACKOUT_TZ is not an IANA time zone name")
		} else {
			cfg.Blackout = dailywindow.Schedule{{Windows: windows, Loc: loc}}
		}
	}

	cfg.Vault.Addr = get("VAULT_ADDR")
	switch {
	case cfg.Vault.Addr == "":
		bad("VAULT_ADDR is not set")
	case !strings.HasPrefix(strings.ToLower(cfg.Vault.Addr), "https://"):
		// The session and the Kubernetes token both travel to Vault.
		bad("VAULT_ADDR must be an https URL")
	}
	if b, perr := strconv.ParseBool(get("VAULT_SKIP_VERIFY")); perr == nil && b {
		bad("VAULT_SKIP_VERIFY is set: this binary does not talk to a Vault whose certificate it cannot verify; mount the CA bundle and set VAULT_CACERT")
	}
	cfg.Vault.CACert = get("VAULT_CACERT")
	cfg.Vault.K8sRole = get("VAULT_K8S_ROLE")
	if cfg.Vault.K8sRole == "" {
		// No default: each instance has a role of its own, and a default
		// would be some other instance's.
		bad("VAULT_K8S_ROLE is not set")
	}
	cfg.Vault.KVMount = strings.Trim(orDefault("VAULT_KV_MOUNT", defaultVaultKVMount), "/")
	cfg.Vault.SessionPath = strings.Trim(get("VAULT_SESSION_PATH"), "/")
	if cfg.Vault.SessionPath == "" {
		bad("VAULT_SESSION_PATH is not set")
	}
	// The state lives beside the session, never in it: the session secret
	// is what ESO copies to the connector.
	cfg.Vault.StatePath = strings.Trim(orDefault("VAULT_STATE_PATH", path.Join(path.Dir(cfg.Vault.SessionPath), stateLeaf)), "/")
	if cfg.Vault.SessionPath != "" && cfg.Vault.StatePath == cfg.Vault.SessionPath {
		bad("VAULT_STATE_PATH must differ from VAULT_SESSION_PATH")
	}
	cfg.Vault.K8sTokenPath = get("VAULT_K8S_TOKEN_PATH")
	return cfg, problems
}

// scrubEnvironment removes the secret variables from the process
// environment. Call it after loadConfig and before anything is started.
func scrubEnvironment() {
	for _, name := range secretEnv {
		_ = os.Unsetenv(name)
	}
}

// randomSalt returns 16 random bytes, hex-encoded.
func randomSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// app is the wired service.
type app struct {
	broker  *loginbroker.Broker
	server  *loginbroker.Server
	metrics *loginbroker.Metrics
}

// build wires the broker from its parts. Tests call it with a driver, a
// store and an authenticator of their own.
func build(cfg config, driver loginbroker.Driver, auth loginbroker.Authenticator,
	connect func(ctx context.Context) (loginbroker.Store, error), logger *slog.Logger, tune func(*loginbroker.Config)) (*app, error) {
	metrics := loginbroker.NewMetrics(driver.Site())
	bc := loginbroker.Config{
		Driver:      driver,
		Credentials: cfg.Credentials,
		Connect:     connect,
		Blackout:    cfg.Blackout,
		MinInterval: cfg.MinInterval,
		MaxFailures: cfg.MaxFailures,
		StatePath:   cfg.Vault.KVMount + "/" + cfg.Vault.StatePath,
		Logger:      logger,
		Metrics:     metrics,
		NewSalt:     randomSalt,
	}
	if tune != nil {
		tune(&bc)
	}
	broker, err := loginbroker.New(bc)
	if err != nil {
		return nil, err
	}
	server, err := loginbroker.NewServer(broker, auth, logger, metrics)
	if err != nil {
		return nil, err
	}
	return &app{broker: broker, server: server, metrics: metrics}, nil
}

// serve runs both listeners until ctx ends, then stops accepting requests
// and waits for a refresh in flight.
func (a *app) serve(ctx context.Context, api, metrics net.Listener, logger *slog.Logger) error {
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			// No WriteTimeout on purpose: an answer is written when the
			// refresh ends, which for a browser login is minutes. The
			// broker bounds the refresh itself.
			IdleTimeout:    2 * time.Minute,
			MaxHeaderBytes: 16 << 10,
		}
	}
	apiSrv, metricsSrv := newServer(a.server.Handler()), newServer(a.server.MetricsHandler())
	errc := make(chan error, 2)
	go func() { errc <- apiSrv.Serve(api) }()
	go func() { errc <- metricsSrv.Serve(metrics) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	stop, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = apiSrv.Shutdown(stop)
	_ = metricsSrv.Shutdown(stop)
	// A login in flight finishes and records its outcome. If the pod is
	// killed first, the attempt marker in the store makes the next request
	// count it as a failed login.
	a.broker.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("a listener failed", "outcome", "listener_failed")
		return err
	}
	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, problems := loadConfig(os.Getenv)
	var driver loginbroker.Driver
	if factory, ok := drivers[cfg.Site]; ok {
		d, more := factory(os.Getenv)
		driver, problems = d, append(problems, more...)
	}
	scrubEnvironment()
	if len(problems) > 0 {
		logger.Error("config invalid", "outcome", "config_error", "problems", problems)
		os.Exit(2)
	}

	access, err := kubecm.InCluster(os.Getenv, kubecm.ServiceAccountDir)
	if err != nil {
		logger.Error("the Kubernetes API is not usable from this pod; callers could not be authenticated",
			"outcome", "config_error", "error", err.Error())
		os.Exit(2)
	}
	auth, err := loginbroker.NewTokenReviewer(access, cfg.Audience, cfg.AllowedCallers)
	if err != nil {
		logger.Error("config invalid", "outcome", "config_error", "problems", []string{err.Error()})
		os.Exit(2)
	}
	connect := func(ctx context.Context) (loginbroker.Store, error) {
		store, err := loginvault.Connect(ctx, cfg.Vault)
		if err != nil {
			return nil, err
		}
		return store, nil
	}
	a, err := build(cfg, driver, auth, connect, logger, nil)
	if err != nil {
		logger.Error("config invalid", "outcome", "config_error", "problems", []string{err.Error()})
		os.Exit(2)
	}

	api, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("could not listen", "outcome", "listener_failed", "listener", "api")
		os.Exit(1)
	}
	metrics, err := net.Listen("tcp", cfg.MetricsListen)
	if err != nil {
		logger.Error("could not listen", "outcome", "listener_failed", "listener", "metrics")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// Nothing is sent to the site at startup, and nothing ever is without a
	// request: no warm-up login, no check of the stored session.
	logger.Info("login broker ready",
		"site", a.broker.Site(),
		"audience", auth.Audience(),
		"allowed_callers", auth.Allowed(),
		"min_interval_seconds", int64(a.broker.MinInterval()/time.Second),
		"max_failures", cfg.MaxFailures,
		"blackout", a.broker.Blackout().String(),
		"vault_session_path", cfg.Vault.KVMount+"/"+cfg.Vault.SessionPath,
		"vault_state_path", cfg.Vault.KVMount+"/"+cfg.Vault.StatePath)
	if err := a.serve(ctx, api, metrics, logger); err != nil {
		os.Exit(1)
	}
	logger.Info("login broker stopped")
}
