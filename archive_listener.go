package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/ingest/archives"
	"github.com/leftathome/glovebox/internal/ingest/auth"
	"github.com/leftathome/glovebox/internal/source"
)

// bootstrapArchives wires spec 10 auth + spec 13 archive delivery onto
// the supplied mux using the production Vault K8s auth handshake. It
// is a no-op when either Ingest.Auth.Enabled or Ingest.Archives.Enabled
// is false; this keeps the chart toggle (.Values.ingest.auth.enabled,
// .Values.ingest.archives.enabled) effective at runtime.
//
// Returns an error only for irrecoverable misconfigurations the
// validator should already have caught (e.g. an unparseable trusted
// CIDR). The process must fail-fast on those; the operator fixes the
// config and restarts.
func bootstrapArchives(ctx context.Context, cfg config.Config, mux *http.ServeMux) error {
	if !cfg.Ingest.Auth.Enabled || !cfg.Ingest.Archives.Enabled {
		return nil
	}
	src, err := buildTokenSource(ctx, cfg.Ingest.Auth)
	if err != nil {
		return err
	}
	return bootstrapArchivesWithSource(ctx, cfg, mux, src)
}

// buildTokenSource selects the bearer-token source from config. Vault is
// the production default; the env and file sources are DEV-ONLY escape
// hatches so the auth + archive path can be brought up on a single
// node/container without a Vault-backed cluster (glovebox-4ypk). The
// dev sources log loudly so a misconfigured production deploy is
// obvious in the pod logs.
func buildTokenSource(ctx context.Context, cfg config.IngestAuthConfig) (auth.TokenSource, error) {
	switch cfg.Source {
	case "", "vault":
		vc := loginVault(ctx, cfg.Vault)
		return auth.NewVaultTokenSource(vc, cfg.Vault.KVMount, cfg.Vault.TokensPath), nil
	case "env":
		sid := os.Getenv("GLOVEBOX_INGEST_SOURCE_ID")
		log.Printf("glovebox ingest auth source=env (DEV ONLY, not for production) source_id=%q", sid)
		return auth.NewEnvTokenSource(sid, os.Getenv("GLOVEBOX_INGEST_TOKEN")), nil
	case "file":
		log.Printf("glovebox ingest auth source=file (DEV ONLY, not for production) path=%s", cfg.File.Path)
		return auth.NewFileTokenSource(cfg.File.Path), nil
	default:
		return nil, fmt.Errorf("unknown ingest.auth.source %q (want vault|env|file)", cfg.Source)
	}
}

// bootstrapArchivesWithSource is the testable seam: it accepts a
// caller-supplied auth.TokenSource instead of selecting one from config
// and (for Vault) doing the K8s login. The production helper
// (bootstrapArchives) builds the source via buildTokenSource and wraps
// this; integration tests pass an in-memory source. A source whose List
// fails surfaces through archives.StartArchiveListener as the 503
// fallback path per spec 10 §4.1 (a VaultTokenSource wrapping a
// failingVaultClient produces the same outcome a Vault outage would).
func bootstrapArchivesWithSource(ctx context.Context, cfg config.Config, mux *http.ServeMux, src auth.TokenSource) error {
	trustedCIDRs, err := parseTrustedCIDRs(cfg.Ingest.Auth.TrustedProxyCIDRs)
	if err != nil {
		return fmt.Errorf("parse ingest.auth.trusted_proxy_cidrs: %w", err)
	}

	rl := auth.NewRateLimiter(auth.RateLimitConfig{
		Window:            time.Duration(cfg.Ingest.Auth.PerIPRateLimit.WindowSeconds) * time.Second,
		PerIPMaxRejected:  cfg.Ingest.Auth.PerIPRateLimit.MaxRejected,
		LRUCapacity:       cfg.Ingest.Auth.PerIPRateLimit.LRUCapacity,
		GlobalMaxRejected: cfg.Ingest.Auth.GlobalRateLimit.MaxRejected,
	})
	pr := &auth.ProxyResolver{TrustedCIDRs: trustedCIDRs}

	tokenStore := auth.NewTokenStore()

	telemetry, err := archives.NewTelemetry("glovebox", "glovebox")
	if err != nil {
		return fmt.Errorf("init archive telemetry: %w", err)
	}

	uploadStore := archives.NewStore(archives.StoreConfig{
		PerSourceMaxConcurrent: cfg.Ingest.Archives.PerSourceMaxConcurrent,
		GlobalMaxConcurrent:    cfg.Ingest.Archives.GlobalMaxConcurrent,
	})

	pvcCapacity := stagingCapacityBytes(cfg.Ingest.Archives.StagingRoot)

	// Connector-lane registry (glovebox-9s60). Fail-fast on a malformed
	// registry: config.Validate already loaded it once at boot, so a parse
	// error here is an invariant violation, but loading is also how we get
	// the *Registry to thread into the listener. A nil SourcesFile yields a
	// non-enforcing registry that rejects the recognizer-scan media type.
	sources, err := source.Load(cfg.SourcesFile)
	if err != nil {
		return fmt.Errorf("load sources registry: %w", err)
	}

	listenerCfg := archives.ArchiveListenerConfig{
		Mux:              mux,
		StagingRoot:      cfg.Ingest.Archives.StagingRoot,
		PVCCapacityBytes: pvcCapacity,
		TusMaxSize:       cfg.Ingest.Archives.MaxUploadSize,
		PatchIdleTimeout: time.Duration(cfg.Ingest.Archives.PatchIdleTimeoutSeconds) * time.Second,
		Store:            uploadStore,
		Telemetry:        telemetry,
		TokenStore:       tokenStore,
		AuthReloadConfig: auth.ReloadConfig{
			Source: src,
		},
		RateLimiter:         rl,
		ProxyResolver:       pr,
		TokenReloadInterval: time.Duration(cfg.Ingest.Auth.ReloadIntervalSeconds) * time.Second,
		CleanupInterval:     time.Duration(cfg.Ingest.Archives.CleanupIntervalSeconds) * time.Second,
		QuotaInterval:       60 * time.Second,
		Sources:             sources,
	}

	if archives.StartArchiveListener(ctx, listenerCfg) == nil {
		log.Printf("glovebox archive listener mounted 503 fallback on /v1/archives* (startup check failed; see prior error logs)")
	} else {
		log.Printf("glovebox archive listener mounted on /v1/archives* (staging_root=%s, pvc_capacity=%d bytes)",
			cfg.Ingest.Archives.StagingRoot, pvcCapacity)
	}
	return nil
}

// parseTrustedCIDRs converts a slice of CIDR strings into the
// *net.IPNet form auth.ProxyResolver expects. Empty input returns
// (nil, nil) — meaning never trust X-Forwarded-For. An invalid entry
// returns an error rather than silently dropping it; the validator
// catches obvious typos at boot.
func parseTrustedCIDRs(strs []string) ([]*net.IPNet, error) {
	if len(strs) == 0 {
		return nil, nil
	}
	out := make([]*net.IPNet, 0, len(strs))
	for _, s := range strs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// loginVault returns an auth.VaultClient. On LoginK8s failure it
// returns a sentinel client whose ListPath / ReadKV calls return the
// captured error so StartArchiveListener's first Reload trips the 503
// fallback path. Operator must fix Vault and restart the pod (matches
// spec 10 §4.1).
func loginVault(ctx context.Context, vc config.VaultClientConfig) auth.VaultClient {
	client, err := auth.LoginK8s(ctx, vc.Addr, vc.K8sRole)
	if err != nil {
		log.Printf("glovebox vault k8s login failed: %v (archive listener will mount 503 fallback)", err)
		return &failingVaultClient{err: err}
	}
	return auth.NewProductionVaultClient(client)
}

// failingVaultClient is the sentinel returned by loginVault when the
// Vault K8s auth handshake fails. Both interface methods return the
// captured error so the listener's first TokenStore.Reload observes a
// load failure and mounts the 503 fallback. This is a deliberate
// asymmetry to keep ALL startup failures funnelled through the same
// "503 + structured log + operator-restart" code path; main.go does
// not need its own mountArchive503 call.
type failingVaultClient struct {
	err error
}

func (f *failingVaultClient) ListPath(context.Context, string, string) ([]string, error) {
	return nil, f.err
}

func (f *failingVaultClient) ReadKV(context.Context, string, string) (map[string]any, error) {
	return nil, f.err
}
