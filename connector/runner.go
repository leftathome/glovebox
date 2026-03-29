package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type BaseConfig struct {
	Routes []Route `json:"routes"`
}

func Run(opts Options) {
	if opts.HealthPort == 0 {
		opts.HealthPort = 8080
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Minute
	}

	logger := slog.Default().With("connector", opts.Name)
	logger.Info("starting connector")

	// Load config
	var baseCfg BaseConfig
	if opts.ConfigFile != "" {
		data, err := os.ReadFile(opts.ConfigFile)
		if err != nil {
			logger.Error("load config", "error", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(data, &baseCfg); err != nil {
			logger.Error("parse config", "error", err)
			os.Exit(1)
		}
	}

	router := NewRouter(baseCfg.Routes)
	if len(baseCfg.Routes) > 0 {
		hasWildcard := false
		for _, r := range baseCfg.Routes {
			if r.Match == "*" {
				hasWildcard = true
				break
			}
		}
		if !hasWildcard {
			logger.Warn("no wildcard route defined -- unmatched items will be skipped")
		}
	}

	// Init checkpoint
	cp, err := NewCheckpoint(opts.StateDir)
	if err != nil {
		logger.Error("init checkpoint", "error", err)
		os.Exit(1)
	}

	// Init staging writer
	writer, err := NewStagingWriter(opts.StagingDir, opts.Name)
	if err != nil {
		logger.Error("init staging writer", "error", err)
		os.Exit(1)
	}
	writer.CleanOrphans()

	_ = router // available for connectors via config

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ready atomic.Bool

	// Health endpoints
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

	healthServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", opts.HealthPort),
		Handler: healthMux,
	}
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server", "error", err)
		}
	}()

	// Determine execution mode
	_, isWatcher := opts.Connector.(Watcher)
	_, isListener := opts.Connector.(Listener)

	// Start listener if applicable
	if isListener {
		listener := opts.Connector.(Listener)
		listenerServer := &http.Server{
			Addr:    fmt.Sprintf(":%d", opts.HealthPort+1),
			Handler: listener.Handler(),
		}
		go func() {
			logger.Info("listener started", "port", opts.HealthPort+1)
			if err := listenerServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("listener server", "error", err)
			}
		}()
		defer func() {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			listenerServer.Shutdown(shutCtx)
		}()
	}

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Initial poll
	logger.Info("running initial poll")
	if err := runPoll(ctx, opts.Connector, cp, logger); err != nil {
		if IsPermanent(err) {
			logger.Error("permanent error during initial poll", "error", err)
			os.Exit(1)
		}
		logger.Warn("transient error during initial poll", "error", err)
	} else {
		ready.Store(true)
		logger.Info("initial poll complete, connector ready")
	}

	if ctx.Err() != nil {
		shutdown(healthServer, logger)
		return
	}

	// Poll-only mode: exit after first poll
	if !isWatcher && !isListener && opts.PollInterval == 0 {
		shutdown(healthServer, logger)
		return
	}

	// Long-running mode
	if isWatcher {
		runWatchLoop(ctx, opts, cp, &ready, logger)
	} else {
		runPollLoop(ctx, opts, cp, &ready, logger)
	}

	shutdown(healthServer, logger)
	logger.Info("connector stopped")
}

func runPoll(ctx context.Context, c Connector, cp Checkpoint, logger *slog.Logger) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.Poll(ctx, cp)
}

func runWatchLoop(ctx context.Context, opts Options, cp Checkpoint, ready *atomic.Bool, logger *slog.Logger) {
	watcher := opts.Connector.(Watcher)
	pollTicker := time.NewTicker(opts.PollInterval)
	defer pollTicker.Stop()

	var wg sync.WaitGroup

	for {
		if ctx.Err() != nil {
			break
		}

		// Start watch in background
		watchCtx, watchCancel := context.WithCancel(ctx)
		watchDone := make(chan error, 1)

		wg.Add(1)
		go func() {
			defer wg.Done()
			watchDone <- watcher.Watch(watchCtx, cp)
		}()

		select {
		case <-ctx.Done():
			watchCancel()
			wg.Wait()
			return

		case err := <-watchDone:
			watchCancel()
			if err != nil {
				if IsPermanent(err) {
					logger.Error("permanent watch error", "error", err)
					os.Exit(1)
				}
				logger.Warn("watch error, will re-poll and retry", "error", err)
				time.Sleep(opts.PollInterval)
				if err := runPoll(ctx, opts.Connector, cp, logger); err == nil {
					ready.Store(true)
				}
			}

		case <-pollTicker.C:
			// Pause watch for periodic re-poll
			watchCancel()
			wg.Wait()
			logger.Info("periodic re-poll")
			if err := runPoll(ctx, opts.Connector, cp, logger); err != nil {
				logger.Warn("re-poll error", "error", err)
			}
		}
	}
}

func runPollLoop(ctx context.Context, opts Options, cp Checkpoint, ready *atomic.Bool, logger *slog.Logger) {
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info("scheduled poll")
			if err := runPoll(ctx, opts.Connector, cp, logger); err != nil {
				if IsPermanent(err) {
					logger.Error("permanent poll error", "error", err)
					os.Exit(1)
				}
				logger.Warn("poll error", "error", err)
			} else {
				ready.Store(true)
			}
		}
	}
}

func shutdown(server *http.Server, logger *slog.Logger) {
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutCtx)
}
