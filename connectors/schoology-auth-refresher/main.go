// Command schoology-auth-refresher renews a Schoology browser-session
// cookie set inside the cluster and persists the new 5-field session JSON
// into Vault KV v2, where ESO syncs it down to the Schoology connector
// pod's mounted Secret.
//
// The flow follows spec 06 §12:
//
//  1. Read SCHOOLOGY_HOST, SCHOOLOGY_USERNAME, SCHOOLOGY_PASSWORD plus the
//     VAULT_* configuration from environment variables.
//  2. Authenticate to Vault using the Kubernetes auth method with the
//     pod's projected ServiceAccount token (no static Vault token), and
//     read the login state record (VAULT_STATE_PATH, default "login-state"
//     beside the session). If the login latch is set for the configured
//     credentials, stop: no browser is started (exit 6).
//  3. Write the state record (fingerprint of the credentials, an attempt
//     marker), then call schoology-go/auth.LoginWithPassword to drive a
//     remote Browserless browser (auth.WithBrowserURL, via BROWSERLESS_URL/
//     BROWSERLESS_TOKEN) through Schoology's native Drupal /login form.
//     ROD_BROWSER_PATH stays a local/test fallback to a bundled binary.
//  4. Record the outcome. A login that was not confirmed sets the latch
//     (immediately when the page stayed on /login, after two in a row
//     otherwise), so the next scheduled run does not submit the same
//     credentials again.
//  5. PUT the resulting *auth.Credentials to a Vault KV v2 path. The
//     connector Deployment carries a checksum annotation on the rendered
//     Secret, so writing a new version automatically rolls the connector
//     pod once ESO syncs.
//
// Exit codes are stable across releases; cluster-side alerting may
// branch on them. See run.go and spec 06 section 12.4.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connectors/schoology"
	"github.com/leftathome/schoology-go/auth"
)

// runTimeout caps the entire flow (Vault, login, writes). Login on a cold
// container takes ~30-60s; the buffer covers DOM-change tolerance and
// Vault latency. loginTimeout caps the browser login inside it, so that
// the outcome of a login that runs long can still be recorded.
const (
	runTimeout   = 5 * time.Minute
	loginTimeout = 3 * time.Minute
)

// stateLeaf is the default state path's last element, beside the session.
const stateLeaf = "login-state"

// config captures everything run() needs from the environment.
type config struct {
	SchoologyHost     string
	SchoologyUsername string
	SchoologyPassword string
	VaultAddr         string
	VaultCACert       string
	VaultK8sRole      string
	VaultKVMount      string
	VaultSessionPath  string
	// VaultStatePath holds the login latch. Never the session path: the
	// session secret is what ESO copies to the connector.
	VaultStatePath string
	// BrowserlessToken is only here to be redacted from log lines.
	BrowserlessToken string
}

// problems returns the required-but-empty settings and any that conflict.
// It never contains a value.
func (c config) problems() []string {
	missing := c.missingFields()
	if c.VaultSessionPath != "" && c.VaultStatePath != "" &&
		strings.Trim(c.VaultStatePath, "/") == strings.Trim(c.VaultSessionPath, "/") {
		missing = append(missing, "VAULT_STATE_PATH (must differ from VAULT_SESSION_PATH)")
	}
	return missing
}

// missingFields returns the list of required-but-empty fields.
func (c config) missingFields() []string {
	var missing []string
	if c.SchoologyHost == "" {
		missing = append(missing, "SCHOOLOGY_HOST")
	}
	if c.SchoologyUsername == "" {
		missing = append(missing, "SCHOOLOGY_USERNAME")
	}
	if c.SchoologyPassword == "" {
		missing = append(missing, "SCHOOLOGY_PASSWORD")
	}
	if c.VaultAddr == "" {
		missing = append(missing, "VAULT_ADDR")
	}
	if c.VaultK8sRole == "" {
		missing = append(missing, "VAULT_K8S_ROLE")
	}
	if c.VaultSessionPath == "" {
		missing = append(missing, "VAULT_SESSION_PATH")
	}
	if c.VaultStatePath == "" {
		missing = append(missing, "VAULT_STATE_PATH")
	}
	// VaultKVMount intentionally defaults to "secret", not required.
	return missing
}

// credentialsToMap converts a *auth.Credentials to the flat 5-field map
// the connector's ExternalSecret template expects. Keys match the JSON
// tags on the Credentials struct (host, sess_id, csrf_token, csrf_key,
// uid).
func credentialsToMap(c *auth.Credentials) map[string]any {
	return map[string]any{
		"host":       c.Host,
		"sess_id":    c.SessID,
		"csrf_token": c.CSRFToken,
		"csrf_key":   c.CSRFKey,
		"uid":        c.UID,
	}
}

// loadConfig reads the configuration through getenv. Defaults are applied
// for optional values: VAULT_KV_MOUNT "secret", VAULT_STATE_PATH
// "login-state" beside the session.
func loadConfig(getenv func(string) string) config {
	mount := strings.Trim(strings.TrimSpace(getenv("VAULT_KV_MOUNT")), "/")
	if mount == "" {
		mount = "secret"
	}
	sessionPath := strings.Trim(strings.TrimSpace(getenv("VAULT_SESSION_PATH")), "/")
	statePath := strings.Trim(strings.TrimSpace(getenv("VAULT_STATE_PATH")), "/")
	if statePath == "" && sessionPath != "" {
		statePath = path.Join(path.Dir(sessionPath), stateLeaf)
	}
	return config{
		SchoologyHost:     schoology.NormalizeHost(getenv("SCHOOLOGY_HOST")),
		SchoologyUsername: getenv("SCHOOLOGY_USERNAME"),
		SchoologyPassword: getenv("SCHOOLOGY_PASSWORD"),
		VaultAddr:         getenv("VAULT_ADDR"),
		VaultCACert:       strings.TrimSpace(getenv("VAULT_CACERT")),
		VaultK8sRole:      getenv("VAULT_K8S_ROLE"),
		VaultKVMount:      mount,
		VaultSessionPath:  sessionPath,
		VaultStatePath:    statePath,
		BrowserlessToken:  getenv("BROWSERLESS_TOKEN"),
	}
}

// browserlessURL builds the rod ControlURL from BROWSERLESS_URL (+ optional
// BROWSERLESS_TOKEN appended as a query param). Empty BROWSERLESS_URL -> "".
//
// When BROWSERLESS_TOKEN is set the token is appended as "?token=..." (or
// "&token=..." if BROWSERLESS_URL already carries a query string). A token
// without a URL is meaningless, so BROWSERLESS_URL gates everything.
func browserlessURL() string {
	url := os.Getenv("BROWSERLESS_URL")
	if url == "" {
		return ""
	}
	if token := os.Getenv("BROWSERLESS_TOKEN"); token != "" {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		return url + sep + "token=" + token
	}
	return url
}

// realLogin wires the test seam to auth.LoginWithPassword. In-cluster the
// refresher drives a remote Browserless browser: when BROWSERLESS_URL is
// configured we hand its ControlURL to go-rod via auth.WithBrowserURL.
// ROD_BROWSER_PATH remains a local/test fallback that points go-rod at a
// bundled Chromium binary when no Browserless endpoint is set.
func realLogin(ctx context.Context, host, username, password string) (*auth.Credentials, error) {
	opts := []auth.Option{auth.WithTimeout(loginTimeout)}
	if url := browserlessURL(); url != "" {
		opts = append(opts, auth.WithBrowserURL(url))
	} else if bin := os.Getenv("ROD_BROWSER_PATH"); bin != "" {
		opts = append(opts, auth.WithBrowserBinary(bin))
	}
	return auth.LoginWithPassword(ctx, host, username, password, opts...)
}

// randomSalt returns 16 random bytes, hex-encoded.
func randomSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
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
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg := loadConfig(os.Getenv)

	// Top-level context bounds the whole flow. A config error returns from
	// run before connect is called, so Vault is never contacted then.
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	code := run(ctx, cfg, deps{
		now:     time.Now,
		sleep:   sleep,
		connect: func(ctx context.Context) (sessionStore, error) { return newVaultStore(ctx, cfg) },
		login:   realLogin,
		newSalt: randomSalt,
		logger:  logger,
	})
	cancel()
	os.Exit(code)
}
