// Command powerschool-auth-refresher logs in to the PowerSchool parent
// portal and stores the resulting session in Vault KV v2, where ESO syncs
// it into the Secret the PowerSchool connector mounts as its session file.
// The connector never logs in (spec 16 section 4); this is the one process
// that holds the account's username and password.
//
// It is the PowerSchool counterpart of schoology-auth-refresher and keeps
// that binary's layout, Vault wiring and exit codes. What differs is what
// the account demands. The portal keeps one live session per account, and
// the account is its owner's own, so every login ends whatever session
// existed before -- the connector's, or the owner's. Therefore:
//
//   - No login between 16:00 and 21:00 America/Los_Angeles, nor within
//     loginHeadroom before it, enforced here against the real clock,
//     whatever schedule started the process. Such a run logs one line and
//     exits 0. The window can be widened by configuration, never shortened.
//   - No login while the stored session is younger than a minimum age and
//     the portal still accepts it. Extra runs are harmless.
//   - One login per run; a second attempt only when the first failed before
//     the credentials were submitted.
//   - No login with credentials the portal has already rejected, or after
//     two logins in a row that produced no working session: a latch in
//     Vault blocks every later run (exit 6) until the credentials change or
//     an operator clears it.
//   - No browser unless the sign-in page, fetched with a plain GET, is a
//     password form on the configured host.
//   - After the login the portal is only read: one GET of the home page,
//     through a transport that refuses every other method.
//
// Environment:
//
//	POWERSCHOOL_HOSTNAME          portal location, bare host or https URL
//	                              (POWERSCHOOL_HOST is accepted too)
//	POWERSCHOOL_USERNAME          the account
//	POWERSCHOOL_PASSWORD
//	VAULT_ADDR                    https only
//	VAULT_CACERT                  CA bundle for Vault's certificate
//	                              (optional; system roots otherwise)
//	VAULT_K8S_ROLE                default glovebox-powerschool-refresher
//	VAULT_KV_MOUNT                default secret
//	VAULT_SESSION_PATH            default glovebox/powerschool/default/session
//	VAULT_STATE_PATH              default: "login-state" beside the session
//	                              (glovebox/powerschool/default/login-state)
//	POWERSCHOOL_BLACKOUT_START    16:00 or earlier
//	POWERSCHOOL_BLACKOUT_END      21:00 or later
//	POWERSCHOOL_BLACKOUT_EXTRA    further windows, "HH:MM-HH:MM,..."
//	POWERSCHOOL_MIN_SESSION_AGE   Go duration, default 10h
//	POWERSCHOOL_SESSION_LIFETIME  Go duration, default 36h
//	POWERSCHOOL_CHROME_NO_SANDBOX true to start Chromium with --no-sandbox
//	                              (the image sets it; see the Dockerfile)
//
// Request mode (set on the on-request CronJobs by the chart): the run acts
// only when the connector has asked for a login, and otherwise sends
// nothing to the portal.
//
//	POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP     the ConfigMap the connector
//	                                        writes its request to; setting
//	                                        it turns request mode on
//	POWERSCHOOL_LOGIN_REQUEST_NAMESPACE     default: the pod's own
//	POWERSCHOOL_LOGIN_REQUEST_MAX_AGE       older requests are ignored;
//	                                        default 15m
//	POWERSCHOOL_LOGIN_REQUEST_MIN_INTERVAL  least time between logins made
//	                                        for requests; 20m or more
//	POWERSCHOOL_LOGIN_REQUEST_BYPASS        true: a manual run that ignores
//	                                        the request and that interval
//
// No secret is taken from, or written to, the command line, a log line, an
// error string, the termination message or a Kubernetes event. See
// docs/connectors/powerschool-auth-refresher.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	// The blackout is evaluated in a named zone. Embedding the zone
	// database means that cannot fail, or silently become UTC, because an
	// image lacks /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/pslogin"
	"github.com/leftathome/glovebox/internal/psportal"
)

// runTimeout caps the whole run: Vault, at most two logins, the home-page
// reads and the writes.
const runTimeout = 8 * time.Minute

const (
	defaultVaultK8sRole     = "glovebox-powerschool-refresher"
	defaultVaultKVMount     = "secret"
	defaultVaultSessionPath = "glovebox/powerschool/default/session"
	stateLeaf               = "login-state"
	defaultMinSessionAge    = 10 * time.Hour
	// defaultRequestMaxAge: a request older than this is for a poll that
	// has stopped waiting (the connector waits 10 minutes by default).
	defaultRequestMaxAge = 15 * time.Minute
	// minRequestInterval: the least time between two logins made because
	// the connector asked. Default and floor. The login broker applies the
	// same floor to its own logins.
	minRequestInterval     = pslogin.MinRequestInterval
	defaultSessionLifetime = 36 * time.Hour
	// terminationLogPath is where Kubernetes reads a container's
	// termination message from (terminationMessagePath's default).
	terminationLogPath = "/dev/termination-log"
)

// secretEnv are the variables removed from this process's environment as
// soon as they have been read, so that the browser it starts later does not
// inherit them. (chromedp starts Chromium with the parent's environment and
// the library offers no way to give it another.)
var secretEnv = []string{"POWERSCHOOL_PASSWORD", "POWERSCHOOL_USERNAME", "VAULT_TOKEN"}

// config is everything run needs from the environment.
type config struct {
	// BaseURL ("https://host") and Host ("host[:port]") are both derived
	// from POWERSCHOOL_HOSTNAME by the connector's own normaliser, so the
	// two binaries cannot disagree about where the portal is.
	BaseURL  string
	Host     string
	Username string
	Password string

	VaultAddr         string
	VaultCACert       string
	VaultK8sRole      string
	VaultKVMount      string
	VaultSessionPath  string
	VaultStatePath    string
	VaultK8sTokenPath string

	// Request mode (see requestMode): the ConfigMap the connector writes
	// its login request to, and the limits this binary puts on acting on
	// one.
	RequestConfigMap   string
	RequestNamespace   string
	RequestMaxAge      time.Duration
	RequestMinInterval time.Duration
	RequestBypass      bool

	Blackout        blackout
	MinSessionAge   time.Duration
	SessionLifetime time.Duration
	ChromeNoSandbox bool
}

// loadConfig reads and validates the environment. problems lists every
// fault found, by setting name; it never contains a value, since any of
// them may be a secret typed into the wrong field.
func loadConfig(getenv func(string) string) (cfg config, problems []string) {
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	orDefault := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}

	host := getenv("POWERSCHOOL_HOSTNAME")
	if strings.TrimSpace(host) == "" {
		host = getenv("POWERSCHOOL_HOST")
	}
	if strings.TrimSpace(host) == "" {
		bad("POWERSCHOOL_HOSTNAME is not set")
	} else if base, h, err := powerschool.NormalizeBaseURL(host); err != nil {
		bad("POWERSCHOOL_HOSTNAME is not a host or an https URL")
	} else {
		cfg.BaseURL, cfg.Host = base, h
	}

	cfg.Username = getenv("POWERSCHOOL_USERNAME")
	if cfg.Username == "" {
		bad("POWERSCHOOL_USERNAME is not set")
	}
	cfg.Password = getenv("POWERSCHOOL_PASSWORD")
	if cfg.Password == "" {
		bad("POWERSCHOOL_PASSWORD is not set")
	}

	cfg.VaultAddr = strings.TrimSpace(getenv("VAULT_ADDR"))
	switch {
	case cfg.VaultAddr == "":
		bad("VAULT_ADDR is not set")
	case !strings.HasPrefix(strings.ToLower(cfg.VaultAddr), "https://"):
		// The session and the Kubernetes token both travel to Vault.
		bad("VAULT_ADDR must be an https URL")
	}
	if isTruthy(getenv("VAULT_SKIP_VERIFY")) {
		bad("VAULT_SKIP_VERIFY is set: this binary does not talk to a Vault whose certificate it cannot verify; mount the CA bundle and set VAULT_CACERT")
	}
	cfg.VaultCACert = strings.TrimSpace(getenv("VAULT_CACERT"))
	cfg.VaultK8sRole = orDefault("VAULT_K8S_ROLE", defaultVaultK8sRole)
	cfg.VaultKVMount = strings.Trim(orDefault("VAULT_KV_MOUNT", defaultVaultKVMount), "/")
	cfg.VaultSessionPath = strings.Trim(orDefault("VAULT_SESSION_PATH", defaultVaultSessionPath), "/")
	// The login state lives beside the session, never in it: the session
	// secret is what ESO copies to the connector, and a write of state
	// alone would replace the session's fields.
	cfg.VaultStatePath = strings.Trim(orDefault("VAULT_STATE_PATH", path.Join(path.Dir(cfg.VaultSessionPath), stateLeaf)), "/")
	if cfg.VaultStatePath == cfg.VaultSessionPath {
		bad("VAULT_STATE_PATH must differ from VAULT_SESSION_PATH")
	}
	cfg.VaultK8sTokenPath = strings.TrimSpace(getenv("VAULT_K8S_TOKEN_PATH"))

	// On an error parseBlackout still returns the floor, so cfg.Blackout is
	// never an empty guard even on a path that forgot to check problems.
	window, err := parseBlackout(
		getenv("POWERSCHOOL_BLACKOUT_START"),
		getenv("POWERSCHOOL_BLACKOUT_END"),
		getenv("POWERSCHOOL_BLACKOUT_EXTRA"),
		getenv("POWERSCHOOL_BLACKOUT_TZ"),
	)
	if err != nil {
		bad("POWERSCHOOL_BLACKOUT_*: %v", err)
	}
	cfg.Blackout = window

	cfg.MinSessionAge = durationSetting(getenv, "POWERSCHOOL_MIN_SESSION_AGE", defaultMinSessionAge, 0, bad)
	// A lifetime of a few minutes would have the connector refuse the
	// session before its first poll.
	cfg.SessionLifetime = durationSetting(getenv, "POWERSCHOOL_SESSION_LIFETIME", defaultSessionLifetime, time.Hour, bad)

	cfg.ChromeNoSandbox = isTruthy(getenv("POWERSCHOOL_CHROME_NO_SANDBOX"))

	cfg.RequestConfigMap = strings.TrimSpace(getenv("POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP"))
	cfg.RequestNamespace = strings.TrimSpace(getenv("POWERSCHOOL_LOGIN_REQUEST_NAMESPACE"))
	if strings.ContainsAny(cfg.RequestConfigMap, "/ \t") || strings.ContainsAny(cfg.RequestNamespace, "/ \t") {
		bad("POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP / _NAMESPACE must be plain Kubernetes names")
	}
	cfg.RequestMaxAge = durationSetting(getenv, "POWERSCHOOL_LOGIN_REQUEST_MAX_AGE", defaultRequestMaxAge, time.Minute, bad)
	// A floor, not a default that can be configured away: this is the
	// bound on how often a connector that has been taken over can cause a
	// login.
	cfg.RequestMinInterval = durationSetting(getenv, "POWERSCHOOL_LOGIN_REQUEST_MIN_INTERVAL", minRequestInterval, minRequestInterval, bad)
	cfg.RequestBypass = isTruthy(getenv("POWERSCHOOL_LOGIN_REQUEST_BYPASS"))
	return cfg, problems
}

// requestMode reports whether this run only acts on a login request from
// the connector. It does when a request ConfigMap is configured -- the
// chart sets that on the on-request CronJobs -- unless
// POWERSCHOOL_LOGIN_REQUEST_BYPASS is true. The bypass is for an operator's
// manual run (it is an environment variable on the Job, which the connector
// has no permission to create or change): such a run behaves like a
// scheduled one and ignores the request and the interval between
// request-driven logins. Every other rule still applies to it.
func (c config) requestMode() bool {
	return c.RequestConfigMap != "" && !c.RequestBypass
}

// durationSetting reads a Go duration that must be at least min.
func durationSetting(getenv func(string) string, name string, def, min time.Duration, bad func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		bad("%s is not a duration such as 10h or 90m", name)
		return def
	}
	if d < min {
		bad("%s must be at least %s", name, min)
		return def
	}
	return d
}

// isTruthy reads a boolean setting the way strconv does ("1", "t", "true",
// in any case); anything else, including empty, is false.
func isTruthy(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b
}

// scrubEnvironment removes the secret variables from the process
// environment. Call it after loadConfig and before anything is started.
func scrubEnvironment() {
	for _, name := range secretEnv {
		_ = os.Unsetenv(name)
	}
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
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

// writeTermination returns the terminate seam: it writes the message to
// path, where the kubelet picks it up as the container's termination
// message (kubectl describe pod, .status.containerStatuses[].state). A
// failure to write it is ignored: outside a pod the file does not exist.
func writeTermination(path string) func(string) {
	return func(message string) {
		const max = 1024 // the kubelet truncates at 4096 bytes
		if len(message) > max {
			message = message[:max]
		}
		_ = os.WriteFile(path, []byte(message+"\n"), 0o644)
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	terminate := writeTermination(terminationLogPath)

	cfg, problems := loadConfig(os.Getenv)
	scrubEnvironment()
	if len(problems) > 0 {
		logger.Error("config invalid", "outcome", "config_error", "problems", problems)
		terminate(fmt.Sprintf("powerschool-auth-refresher: outcome=config_error exit=%d: %s", exitConfigError, strings.Join(problems, "; ")))
		os.Exit(exitConfigError)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	code := run(ctx, cfg, deps{
		now:   time.Now,
		sleep: sleepCtx,
		connect: func(ctx context.Context) (sessionStore, error) {
			store, err := newVaultStore(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return store, nil
		},
		readRequest: func(ctx context.Context) (string, error) {
			// A client that can read ConfigMaps and nothing else, pinned
			// to the API server. Built only when a request is to be read.
			kube, err := kubecm.NewInCluster(os.Getenv, kubecm.ServiceAccountDir, cfg.RequestNamespace)
			if err != nil {
				return "", err
			}
			value, _, err := kube.GetField(ctx, cfg.RequestConfigMap, psportal.LoginRequestKey)
			return value, err
		},
		checkSignIn: realSignInCheck(cfg, nil),
		login:       realLogin(cfg),
		probe:       realProbe(cfg, nil),
		newSalt:     randomSalt,
		terminate:   terminate,
		logger:      logger,
	})
	cancel()
	os.Exit(code)
}
