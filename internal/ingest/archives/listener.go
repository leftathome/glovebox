// Package archives -- spec 13 archive listener wire-up (Wave C Task C3).
//
// StartArchiveListener mounts the /v1/archives* routes onto a shared
// http.ServeMux after performing the two startup checks documented
// inline:
//
//  1. spec 13 §5.2 -- archives/ and .tmp-archives/ MUST share a
//     filesystem (same st_dev). Otherwise os.Rename in finalize
//     returns EXDEV mid-publish and we'd risk a partially-formed
//     archives/<id>/ being observed by importers.
//  2. spec 10 §4.1 -- the bearer-token store MUST load successfully
//     from Vault before binding the listener. If the FIRST load
//     fails (no cached store), we register a 503 fallback handler
//     on /v1/archives* instead of the real handler; subsequent
//     successful periodic reloads do NOT promote the route to the
//     real handler in v1 (the operator must kubectl rollout restart
//     to re-bind).
//
// Wave C also adds three background goroutines that the listener
// spins up after a successful bind:
//
//   - quota measurement (60s tick, §5.4)
//   - orphan cleanup (60min tick, §5.5)
//   - token reload (SIGHUP + 5min periodic tick, spec 10 §4.2)
//
// This file lives in package archives (rather than the parent
// internal/ingest) so it can import both internal/ingest (for the
// ctx-keyed DeliveredBy helpers) and internal/ingest/auth (for the
// middleware) without creating an import cycle. internal/ingest
// already imports nothing from this package, so the dependency
// graph stays directed.
package archives

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/leftathome/glovebox/internal/ingest/auth"
	"github.com/leftathome/glovebox/internal/source"
)

// ArchiveListenerConfig parameterizes StartArchiveListener. Every
// required field is documented inline; defaults below cover the
// production homelab values.
type ArchiveListenerConfig struct {
	// Mux is the shared http.ServeMux the archive routes attach to.
	// Required.
	Mux *http.ServeMux

	// StagingRoot is the absolute path under which archives/ and
	// .tmp-archives/ live (spec 13 §5.1). Required.
	StagingRoot string

	// PVCCapacityBytes is the total capacity of the staging PVC. Used
	// by the quota goroutine to compute the percentage gauge and trip
	// the §5.4 hard cap.
	PVCCapacityBytes int64

	// TusMaxSize is the protocol-level per-upload cap advertised on
	// OPTIONS and enforced on POST (spec 13 §3.2; default 30 GiB).
	TusMaxSize int64

	// PatchIdleTimeout bounds the wall-clock duration of a single
	// PATCH body read (spec 13 §4.4; default 5 min).
	PatchIdleTimeout time.Duration

	// Store is the in-memory upload-id table. Required. The caller
	// owns construction so the same store can be reused across
	// process-lifetime tests.
	Store *Store

	// Telemetry is the OTel telemetry handle. May be nil; nil-safe
	// methods on *Telemetry no-op.
	Telemetry *Telemetry

	// TokenStore is the bearer-token store. Required. The startup
	// path calls TokenStore.Reload(ctx, AuthReloadConfig) once before
	// mounting the real handler; on failure the 503 fallback is
	// registered instead.
	TokenStore *auth.TokenStore

	// AuthReloadConfig is passed to every TokenStore.Reload call
	// (both the startup load and the periodic reload goroutine).
	// Required.
	AuthReloadConfig auth.ReloadConfig

	// RateLimiter, ProxyResolver wire the spec 10 §5 middleware.
	// Required.
	RateLimiter   *auth.RateLimiter
	ProxyResolver *auth.ProxyResolver

	// TokenReloadInterval drives the periodic reload goroutine
	// (default 5 min per spec 10 §4.2).
	TokenReloadInterval time.Duration

	// CleanupInterval drives the orphan-cleanup goroutine (default
	// 60 min per spec 13 §5.5).
	CleanupInterval time.Duration

	// QuotaInterval drives the storage-measurement goroutine
	// (default 60s per spec 13 §5.4).
	QuotaInterval time.Duration

	// Sources is the connector-lane registry (glovebox-9s60), passed
	// through to the handler's FinalizeConfig. When a registered
	// recognizer-scanner source delivers, finalize stamps the operator
	// lane and renders content.extracted.md; the recognizer-scan media
	// type is gated (fail-closed) to it. May be nil (lane disabled).
	Sources *source.Registry
}

// StartArchiveListener wires the spec 13 archive-delivery routes onto
// cfg.Mux, performs the two startup checks (st_dev + token-load), and
// spins up the three background goroutines.
//
// Returns the *QuotaState so the caller can inspect ShouldBlock() in
// integration tests; production callers can ignore the return value
// (a nil return indicates a startup-check failure -- the 503 fallback
// has been mounted).
//
// On either startup check failing, the listener mounts a 503 fallback
// handler on /v1/archives* and returns nil. The /v1/ingest route is
// unaffected. The process continues to run.
func StartArchiveListener(ctx context.Context, cfg ArchiveListenerConfig) *QuotaState {
	// (1) Spec 13 §5.2 -- verify same filesystem.
	if err := verifySameFilesystem(cfg.StagingRoot); err != nil {
		slog.Error("glovebox archive listener unavailable: tmp and final dirs on different filesystems",
			"staging_root", cfg.StagingRoot,
			"error", err)
		mountArchive503(cfg.Mux)
		return nil
	}

	// (2) Spec 10 §4.1 -- first token load. On failure with no cached
	// store (LoadErr() reports the error), mount the 503 fallback. We
	// DO NOT spin up the periodic-reload goroutine in that case
	// either: per spec, the operator must kubectl rollout restart to
	// retry the bind once Vault is fixed.
	if err := cfg.TokenStore.Reload(ctx, cfg.AuthReloadConfig); err != nil {
		slog.Error("glovebox archive listener unavailable: token load failed",
			"error", err)
		mountArchive503(cfg.Mux)
		return nil
	}

	// (3) Both checks pass -- mount the real handler tree.
	quotaState := NewQuotaState()
	handler := NewHandler(
		cfg.Store, quotaState, cfg.Telemetry,
		HandlerConfig{
			StagingRoot:      cfg.StagingRoot,
			TusMaxSize:       cfg.TusMaxSize,
			PatchIdleTimeout: cfg.PatchIdleTimeout,
			Sources:          cfg.Sources,
		},
	)
	mw := auth.Middleware(cfg.TokenStore, cfg.RateLimiter, cfg.ProxyResolver, cfg.Telemetry)
	handler.Mount(cfg.Mux, Middleware(mw))

	// (4) Spin background goroutines. Each respects ctx for shutdown.
	go StartQuotaGoroutine(ctx, QuotaConfig{
		StagingRoot:   cfg.StagingRoot,
		CapacityBytes: cfg.PVCCapacityBytes,
		Interval:      cfg.QuotaInterval,
		HardTripPct:   95.0,
		HardLiftPct:   85.0,
		Telemetry:     cfg.Telemetry,
	}, quotaState)

	go StartCleanupGoroutine(ctx, CleanupConfig{
		StagingRoot:   cfg.StagingRoot,
		Interval:      cfg.CleanupInterval,
		TmpAge:        72 * time.Hour,
		FinalizeAge:   time.Hour,
		DoneRetention: 7 * 24 * time.Hour,
		Telemetry:     cfg.Telemetry,
	})

	go StartTokenReloadGoroutine(ctx, cfg.TokenStore, cfg.AuthReloadConfig, cfg.TokenReloadInterval)

	return quotaState
}

// verifySameFilesystem stats <root>/archives and <root>/.tmp-archives
// and returns nil iff they live on the same filesystem (matching
// Stat_t.Dev). Creates both directories if missing. Linux-specific:
// on darwin/windows the Stat_t.Dev semantics differ; this binary is
// only built for linux/amd64 production targets. statDev returns 0 on
// non-Linux runtimes so developer machines (darwin / WSL2) don't
// refuse to boot in tests.
func verifySameFilesystem(stagingRoot string) error {
	archivesPath := filepath.Join(stagingRoot, "archives")
	tmpPath := filepath.Join(stagingRoot, ".tmp-archives")

	if err := os.MkdirAll(archivesPath, 0o700); err != nil {
		return fmt.Errorf("mkdir archives: %w", err)
	}
	if err := os.MkdirAll(tmpPath, 0o700); err != nil {
		return fmt.Errorf("mkdir .tmp-archives: %w", err)
	}

	archDev, err := statDev(archivesPath)
	if err != nil {
		return fmt.Errorf("stat archives: %w", err)
	}
	tmpDev, err := statDev(tmpPath)
	if err != nil {
		return fmt.Errorf("stat .tmp-archives: %w", err)
	}
	if archDev != tmpDev {
		return fmt.Errorf("st_dev mismatch: archives=%d, .tmp-archives=%d", archDev, tmpDev)
	}
	return nil
}

// mountArchive503 attaches the 503 fallback to /v1/archives and
// /v1/archives/ on the mux. The fallback responds with Retry-After: 60
// and a plain-text body so clients know to back off.
func mountArchive503(mux *http.ServeMux) {
	mux.Handle("/v1/archives", http.HandlerFunc(serveArchive503))
	mux.Handle("/v1/archives/", http.HandlerFunc(serveArchive503))
}

// serveArchive503 is the 503 fallback for the unavailable-listener
// case. The Retry-After value matches spec 13 §5.2's recommendation.
func serveArchive503(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "60")
	http.Error(w, "archive listener unavailable", http.StatusServiceUnavailable)
}

// StartTokenReloadGoroutine ticks every interval (default 5 min per
// spec 10 §4.2) AND listens for SIGHUP for operator-initiated
// reloads. The function blocks; callers MUST invoke it via `go`.
// Returns when ctx is cancelled.
//
// Reload failures on the periodic tick log WARN and DO NOT crash the
// goroutine; the in-memory store stays at the prior successful load
// (spec 10 §4.2). Reload failures on SIGHUP log ERROR but still
// preserve the prior store. Tests exercise both paths via fake
// VaultClient injection.
func StartTokenReloadGoroutine(ctx context.Context, store *auth.TokenStore, cfg auth.ReloadConfig, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.Reload(ctx, cfg); err != nil {
				slog.Warn("token reload (periodic) failed", "error", err)
			}
		case <-hup:
			slog.Info("token reload (SIGHUP) triggered")
			if err := store.Reload(ctx, cfg); err != nil {
				slog.Error("token reload (SIGHUP) failed", "error", err)
			}
		}
	}
}
