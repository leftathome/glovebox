package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Framework holds the shared runtime resources produced by NewFramework.
// Both the polling connector runtime (connectors/*) and the one-shot
// importer runtime (importers/*) compose on top of a Framework.
//
// A Framework is created with NewFramework and must be released with
// (*Framework).Shutdown when the caller is done with it.
type Framework struct {
	// Name is the connector/importer identifier (e.g. "rss", "imap", "mbox").
	Name string

	// Logger is a structured logger tagged with the connector name.
	Logger *slog.Logger

	// BaseConfig is the parsed JSON config (rules, identity, fetch limits).
	BaseConfig BaseConfig

	// Matcher is a RuleMatcher seeded from BaseConfig.Rules.
	Matcher *RuleMatcher

	// Backend is the staging backend -- either a filesystem StagingWriter
	// or an HTTPStagingBackend, chosen at bootstrap based on
	// GLOVEBOX_INGEST_URL / StagingDir (per spec 08).
	Backend StagingBackend

	// Metrics is the Prometheus metrics registry for this process.
	Metrics *Metrics

	// FetchCounter enforces per-source / per-poll fetch limits.
	FetchCounter *FetchCounter

	// Checkpoint is the key/value checkpoint store rooted at StateDir.
	Checkpoint Checkpoint

	// HealthPort is the port the /healthz, /readyz, /metrics server listens on.
	HealthPort int

	// Ready is flipped to true once the connector has completed its first
	// successful poll. /readyz returns 503 until Ready is true.
	Ready *atomic.Bool

	opts Options

	healthServer   *http.Server
	listenerServer *http.Server

	shutdownOnce sync.Once
}

// PollInterval returns the poll interval configured for this Framework.
func (f *Framework) PollInterval() time.Duration {
	return f.opts.PollInterval
}

// NewFramework performs connector bootstrap: config loading, backend
// selection, rule matcher, metrics, fetch counter, checkpoint, and
// starting the health/metrics HTTP server. If the Connector in opts
// implements Listener, an additional HTTP server is started on
// HealthPort+1.
//
// NewFramework does not install signal handlers and does not run the
// poll/watch loop. Callers drive the runtime on top of the returned
// Framework and must call (*Framework).Shutdown when finished.
func NewFramework(opts Options) (*Framework, error) {
	if opts.HealthPort == 0 {
		opts.HealthPort = 8080
	}

	logger := slog.Default().With("connector", opts.Name)
	logger.Info("starting connector")

	var baseCfg BaseConfig
	if opts.ConfigFile != "" {
		data, err := os.ReadFile(opts.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
		if err := json.Unmarshal(data, &baseCfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		if err := ValidateBaseConfig(&baseCfg); err != nil {
			return nil, PermanentError(fmt.Errorf("config validation: %w", err))
		}
	}

	// Backward-compatible migration: "routes" -> "rules".
	if len(baseCfg.Rules) == 0 && len(baseCfg.Routes) > 0 {
		baseCfg.Rules = baseCfg.Routes
		logger.Warn("config key 'routes' is deprecated, use 'rules' instead")
	}

	matcher := NewRuleMatcher(baseCfg.Rules)
	if len(baseCfg.Rules) > 0 {
		hasWildcard := false
		for _, r := range baseCfg.Rules {
			if r.Match == "*" {
				hasWildcard = true
				break
			}
		}
		if !hasWildcard {
			logger.Warn("no wildcard rule defined -- unmatched items will be skipped")
		}
	}

	cp, err := NewCheckpoint(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("init checkpoint: %w", err)
	}

	ingestURL := os.Getenv("GLOVEBOX_INGEST_URL")
	backend, writer, err := selectBackend(opts.Name, ingestURL, opts.StagingDir, logger)
	if err != nil {
		return nil, fmt.Errorf("backend selection: %w", err)
	}

	metrics, err := NewMetrics(opts.Name)
	if err != nil {
		return nil, fmt.Errorf("init metrics: %w", err)
	}

	fetchCounter := NewFetchCounter(baseCfg.FetchLimits)

	if opts.Setup != nil {
		if err := opts.Setup(ConnectorContext{
			Writer:       writer,
			Backend:      backend,
			Matcher:      matcher,
			Metrics:      metrics,
			FetchCounter: fetchCounter,
		}); err != nil {
			metrics.Shutdown()
			return nil, fmt.Errorf("connector setup: %w", err)
		}
	}

	ready := &atomic.Bool{}

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})
	healthMux.Handle("/metrics", metrics.Handler())

	healthServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", opts.HealthPort),
		Handler: healthMux,
	}
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server", "error", err)
		}
	}()

	var listenerServer *http.Server
	if opts.Connector != nil {
		if listener, ok := opts.Connector.(Listener); ok {
			listenerServer = &http.Server{
				Addr:    fmt.Sprintf(":%d", opts.HealthPort+1),
				Handler: listener.Handler(),
			}
			go func() {
				logger.Info("listener started", "port", opts.HealthPort+1)
				if err := listenerServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					logger.Error("listener server", "error", err)
				}
			}()
		}
	}

	fw := &Framework{
		Name:           opts.Name,
		Logger:         logger,
		BaseConfig:     baseCfg,
		Matcher:        matcher,
		Backend:        backend,
		Metrics:        metrics,
		FetchCounter:   fetchCounter,
		Checkpoint:     cp,
		HealthPort:     opts.HealthPort,
		Ready:          ready,
		opts:           opts,
		healthServer:   healthServer,
		listenerServer: listenerServer,
	}
	return fw, nil
}

// Shutdown releases the resources held by the Framework: it stops the
// health/metrics server, stops any listener server, and shuts down the
// metrics registry. Shutdown is safe to call more than once; subsequent
// calls are no-ops and return nil.
func (f *Framework) Shutdown() error {
	var firstErr error
	f.shutdownOnce.Do(func() {
		if f.listenerServer != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := f.listenerServer.Shutdown(shutCtx); err != nil {
				f.Logger.Warn("listener server shutdown", "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
			cancel()
		}
		if f.healthServer != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := f.healthServer.Shutdown(shutCtx); err != nil {
				f.Logger.Warn("health server shutdown", "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
			cancel()
		}
		if f.Metrics != nil {
			f.Metrics.Shutdown()
		}
	})
	return firstErr
}
