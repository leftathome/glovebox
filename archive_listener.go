package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/ingest/archives"
	"github.com/leftathome/glovebox/internal/ingest/auth"
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
	vc := loginVault(ctx, cfg.Ingest.Auth.Vault)
	return bootstrapArchivesWithClient(ctx, cfg, mux, vc)
}

// bootstrapArchivesWithClient is the testable seam: it accepts a
// caller-supplied auth.VaultClient instead of doing the K8s login. The
// production helper (bootstrapArchives) wraps it; integration tests
// pass an in-memory fake. Failures from the supplied client surface
// through archives.StartArchiveListener as the 503 fallback path per
// spec 10 §4.1 (a failingVaultClient produces the same outcome a Vault
// outage would).
func bootstrapArchivesWithClient(ctx context.Context, cfg config.Config, mux *http.ServeMux, vc auth.VaultClient) error {
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

	listenerCfg := archives.ArchiveListenerConfig{
		Mux:                 mux,
		StagingRoot:         cfg.Ingest.Archives.StagingRoot,
		PVCCapacityBytes:    pvcCapacity,
		TusMaxSize:          cfg.Ingest.Archives.MaxUploadSize,
		PatchIdleTimeout:    time.Duration(cfg.Ingest.Archives.PatchIdleTimeoutSeconds) * time.Second,
		Store:               uploadStore,
		Telemetry:           telemetry,
		TokenStore:          tokenStore,
		AuthReloadConfig: auth.ReloadConfig{
			Client:   vc,
			KVMount:  cfg.Ingest.Auth.Vault.KVMount,
			BasePath: cfg.Ingest.Auth.Vault.TokensPath,
		},
		RateLimiter:         rl,
		ProxyResolver:       pr,
		TokenReloadInterval: time.Duration(cfg.Ingest.Auth.ReloadIntervalSeconds) * time.Second,
		CleanupInterval:     time.Duration(cfg.Ingest.Archives.CleanupIntervalSeconds) * time.Second,
		QuotaInterval:       60 * time.Second,
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

// stagingCapacityBytes returns the total bytes of the filesystem
// hosting stagingRoot, used by the spec 13 §5.4 quota gauge. On any
// Statfs error (non-Linux dev environments, missing mountpoint at
// boot) we return 0 — the quota goroutine treats zero as "unknown
// capacity" and skips the percentage trip but still records the raw
// gauge. The startup log line carries the value so an operator can
// catch a missing PVC mount via metrics-port-only.
func stagingCapacityBytes(stagingRoot string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(stagingRoot, &st); err != nil {
		return 0
	}
	// Bsize * Blocks = total bytes on the filesystem; Bavail would
	// give the user-available subset, but the spec 13 gauge is
	// "fraction of the PVC consumed" so Blocks is the right denom.
	return int64(st.Bsize) * int64(st.Blocks)
}
