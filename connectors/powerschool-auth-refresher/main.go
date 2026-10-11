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
//   - No login between 16:00 and 21:00 America/Los_Angeles, enforced here
//     against the real clock, whatever schedule started the process. Such a
//     run logs one line and exits 0.
//   - No login while the stored session is younger than a minimum age and
//     the portal still accepts it. Extra runs are harmless.
//   - One login per run; a second attempt only when the first failed before
//     the credentials were submitted.
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
//	POWERSCHOOL_BLACKOUT_START    default 16:00
//	POWERSCHOOL_BLACKOUT_END      default 21:00
//	POWERSCHOOL_BLACKOUT_TZ       default America/Los_Angeles
//	POWERSCHOOL_MIN_SESSION_AGE   Go duration, default 10h
//	POWERSCHOOL_SESSION_LIFETIME  Go duration, default 36h
//	POWERSCHOOL_CHROME_NO_SANDBOX true to start Chromium with --no-sandbox
//	                              (the image sets it; see the Dockerfile)
//
// No secret is taken from, or written to, the command line, a log line, an
// error string or a Kubernetes event. See docs/connectors/powerschool-auth-refresher.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	// The blackout is evaluated in a named zone. Embedding the zone
	// database means that cannot fail, or silently become UTC, because an
	// image lacks /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/leftathome/glovebox/connectors/powerschool"
)

// runTimeout caps the whole run: Vault, at most two logins, the home-page
// reads and the write.
const runTimeout = 8 * time.Minute

const (
	defaultVaultK8sRole     = "glovebox-powerschool-refresher"
	defaultVaultKVMount     = "secret"
	defaultVaultSessionPath = "glovebox/powerschool/default/session"
	defaultMinSessionAge    = 10 * time.Hour
	defaultSessionLifetime  = 36 * time.Hour
)

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
	VaultK8sTokenPath string

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
	cfg.VaultK8sTokenPath = strings.TrimSpace(getenv("VAULT_K8S_TOKEN_PATH"))

	window, err := parseBlackout(
		orDefault("POWERSCHOOL_BLACKOUT_START", defaultBlackoutStart),
		orDefault("POWERSCHOOL_BLACKOUT_END", defaultBlackoutEnd),
		orDefault("POWERSCHOOL_BLACKOUT_TZ", defaultBlackoutZone),
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
	return cfg, problems
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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, problems := loadConfig(os.Getenv)
	if len(problems) > 0 {
		logger.Error("config invalid", "outcome", "config_error", "problems", problems)
		os.Exit(exitConfigError)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	code := run(ctx, cfg, deps{
		now:   time.Now,
		sleep: sleepCtx,
		connect: func(ctx context.Context) (sessionStore, error) {
			return newVaultStore(ctx, cfg)
		},
		login:  realLogin(cfg),
		probe:  realProbe(cfg, nil),
		logger: logger,
	})
	cancel()
	os.Exit(code)
}
