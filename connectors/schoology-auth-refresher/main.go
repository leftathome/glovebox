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
//     pod's projected ServiceAccount token (no static Vault token).
//  3. Call schoology-go/auth.LoginWithPassword to drive a headless
//     Chromium through Schoology's native Drupal /login form.
//  4. PUT the resulting *auth.Credentials to a Vault KV v2 path. The
//     connector Deployment carries a checksum annotation on the rendered
//     Secret, so writing a new version automatically rolls the connector
//     pod once ESO syncs.
//
// Exit codes are stable across releases; cluster-side alerting may
// branch on them. See spec 06 §12.4.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	vaultk8s "github.com/hashicorp/vault/api/auth/kubernetes"

	"github.com/leftathome/schoology-go/auth"
)

// Exit codes per spec 06 §12.4. Stable contract.
const (
	exitSuccess             = 0
	exitTransient           = 1
	exitConfigError         = 2
	exitBadCredentials      = 3
	exitSecretWriteRejected = 4
)

// runTimeout caps the entire login + write flow. Login on a cold
// container takes ~30-60s; the buffer covers DOM-change tolerance and
// Vault latency.
const runTimeout = 5 * time.Minute

// config captures everything run() needs from the environment.
type config struct {
	SchoologyHost     string
	SchoologyUsername string
	SchoologyPassword string
	VaultAddr         string
	VaultK8sRole      string
	VaultKVMount      string
	VaultSessionPath  string
}

// loginFunc is the test seam over auth.LoginWithPassword. Tests replace
// this with a stub so they never start Chromium.
type loginFunc func(ctx context.Context, host, username, password string) (*auth.Credentials, error)

// vaultWriter is the test seam over *vaultapi.Client.KVv2(...).Put. The
// production implementation wraps a real Vault client; tests pass a
// fake.
type vaultWriter interface {
	Put(ctx context.Context, path string, data map[string]any) (version int, err error)
}

// run is the testable entrypoint. main() builds the production wiring
// (Vault client + K8s login + real auth.LoginWithPassword) and calls
// this with seams.
//
// Vault K8s authentication happens BEFORE run() is invoked, in main().
// That keeps run() focused on the business logic and lets tests skip
// the K8s auth dance (which would require a real Vault server). The
// pre-run K8s-auth failure mode maps to exitTransient at the main()
// level.
func run(ctx context.Context, cfg config, login loginFunc, vw vaultWriter, logger *slog.Logger) int {
	// Step 1: validate required config.
	if missing := cfg.missingFields(); len(missing) > 0 {
		logger.Error("config invalid",
			"outcome", "config_error",
			"missing", missing,
		)
		return exitConfigError
	}

	// Step 2: drive the browser-based login.
	creds, err := login(ctx, cfg.SchoologyHost, cfg.SchoologyUsername, cfg.SchoologyPassword)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			// Do NOT retry; repeated bad-password submits could lock the
			// upstream account. Operator must rotate the Vault source.
			logger.Error("schoology login rejected credentials",
				"outcome", "bad_credentials",
				"host", cfg.SchoologyHost,
				"error", err.Error(),
			)
			return exitBadCredentials
		}
		logger.Error("schoology login failed",
			"outcome", "transient_failure",
			"host", cfg.SchoologyHost,
			"error", err.Error(),
		)
		return exitTransient
	}

	// Step 3: shape the 5-field credentials map. We hand-build the map
	// to keep the wire format obvious and avoid round-tripping through
	// json.Marshal/Unmarshal (which would also lose typing on the
	// stored value).
	data := credentialsToMap(creds)

	// Step 4: write to Vault KV v2.
	version, err := vw.Put(ctx, cfg.VaultSessionPath, data)
	if err != nil {
		if isClientError(err) {
			logger.Error("vault rejected write",
				"outcome", "secret_write_rejected",
				"vault_path", cfg.VaultSessionPath,
				"error", err.Error(),
			)
			return exitSecretWriteRejected
		}
		logger.Error("vault write failed",
			"outcome", "transient_failure",
			"vault_path", cfg.VaultSessionPath,
			"error", err.Error(),
		)
		return exitTransient
	}

	logger.Info("refresh complete",
		"outcome", "success",
		"vault_path", cfg.VaultSessionPath,
		"version", version,
	)
	return exitSuccess
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
	// VaultKVMount intentionally defaults to "secret" upstream, not
	// required.
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

// isClientError reports whether a Vault write error is a 4xx (operator
// must fix policy/path) versus a transient 5xx/network failure. We
// first try to extract *vaultapi.ResponseError for a structured check,
// then fall back to substring matching against the error message for
// wrapped or stringified cases.
func isClientError(err error) bool {
	if err == nil {
		return false
	}
	var re *vaultapi.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode >= 400 && re.StatusCode < 500
	}
	msg := err.Error()
	for _, marker := range []string{
		"permission denied",
		"403",
		"400",
		"404",
		"Forbidden",
		"Bad Request",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// productionVaultWriter implements vaultWriter against a real
// *vaultapi.Client. It uses KVv2's Put helper which wraps the path with
// the configured mount and the KV v2 "/data/" prefix automatically.
type productionVaultWriter struct {
	client *vaultapi.Client
	mount  string
}

func (p *productionVaultWriter) Put(ctx context.Context, path string, data map[string]any) (int, error) {
	secret, err := p.client.KVv2(p.mount).Put(ctx, path, data)
	if err != nil {
		return 0, err
	}
	if secret != nil && secret.VersionMetadata != nil {
		return secret.VersionMetadata.Version, nil
	}
	return 0, nil
}

// loadConfig reads the configuration from the environment. Defaults are
// applied for optional values.
func loadConfig() config {
	mount := os.Getenv("VAULT_KV_MOUNT")
	if mount == "" {
		mount = "secret"
	}
	return config{
		SchoologyHost:     os.Getenv("SCHOOLOGY_HOST"),
		SchoologyUsername: os.Getenv("SCHOOLOGY_USERNAME"),
		SchoologyPassword: os.Getenv("SCHOOLOGY_PASSWORD"),
		VaultAddr:         os.Getenv("VAULT_ADDR"),
		VaultK8sRole:      os.Getenv("VAULT_K8S_ROLE"),
		VaultKVMount:      mount,
		VaultSessionPath:  os.Getenv("VAULT_SESSION_PATH"),
	}
}

// newVaultClient builds an authenticated Vault client using the K8s
// auth method. The pod's projected ServiceAccount token is read from
// the default path by the kubernetes auth helper.
func newVaultClient(ctx context.Context, cfg config) (*vaultapi.Client, error) {
	vc := vaultapi.DefaultConfig()
	vc.Address = cfg.VaultAddr
	client, err := vaultapi.NewClient(vc)
	if err != nil {
		return nil, fmt.Errorf("build vault client: %w", err)
	}
	k8sAuth, err := vaultk8s.NewKubernetesAuth(cfg.VaultK8sRole)
	if err != nil {
		return nil, fmt.Errorf("build k8s auth: %w", err)
	}
	if _, err := client.Auth().Login(ctx, k8sAuth); err != nil {
		return nil, fmt.Errorf("vault k8s login: %w", err)
	}
	return client, nil
}

// realLogin wires the test seam to auth.LoginWithPassword. The
// schoology-go library picks up the Chromium binary either via the
// ROD_BROWSER_PATH env var (when go-rod respects it) or by autodetect;
// the Dockerfile installs Chromium at /usr/bin/chromium and we pass
// that through explicitly.
func realLogin(ctx context.Context, host, username, password string) (*auth.Credentials, error) {
	var opts []auth.Option
	if bin := os.Getenv("ROD_BROWSER_PATH"); bin != "" {
		opts = append(opts, auth.WithBrowserBinary(bin))
	}
	return auth.LoginWithPassword(ctx, host, username, password, opts...)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg := loadConfig()

	// Top-level context bounds the whole flow (login + Vault write).
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	// Authenticate to Vault BEFORE entering run(). A failure here is
	// always transient (Vault outage, bad role binding, network
	// partition). Run-time bad-config remains the responsibility of
	// run().
	//
	// We tolerate run() being called without a Vault client when
	// SCHOOLOGY_* or VAULT_* fields are missing: in that case run()
	// returns exitConfigError before ever using the writer. To preserve
	// that contract we still call run() with the seam set to a writer
	// that would panic if invoked — but only when we successfully
	// reached the writer construction step.
	if missing := cfg.missingFields(); len(missing) > 0 {
		// Skip the Vault login dance entirely; run() will log the
		// config error and exit.
		os.Exit(run(ctx, cfg, realLogin, nilWriter{}, logger))
	}

	client, err := newVaultClient(ctx, cfg)
	if err != nil {
		logger.Error("vault authentication failed",
			"outcome", "transient_failure",
			"vault_addr", cfg.VaultAddr,
			"vault_role", cfg.VaultK8sRole,
			"error", err.Error(),
		)
		os.Exit(exitTransient)
	}

	vw := &productionVaultWriter{client: client, mount: cfg.VaultKVMount}
	os.Exit(run(ctx, cfg, realLogin, vw, logger))
}

// nilWriter is a stub used only on the config-error path so we never
// pass an actual nil interface to run(). It returns a sentinel error if
// somebody invokes it, which run() will never do when the config is
// invalid (we return before the login step).
type nilWriter struct{}

func (nilWriter) Put(ctx context.Context, path string, data map[string]any) (int, error) {
	return 0, errors.New("refresher: writer not initialized (config error path)")
}
