package connector

import (
	"context"
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

// BaseConfig is the shared JSON shape every connector config starts from.
// Connector-specific configs embed BaseConfig to pick up rules, identity,
// and fetch limits without re-declaring them.
type BaseConfig struct {
	Rules          []Rule          `json:"rules"`
	Routes         []Rule          `json:"routes"`
	ConfigIdentity *ConfigIdentity `json:"identity,omitempty"`
	FetchLimits    FetchLimits     `json:"fetch_limits"`
}

// Run is a backwards-compatible entry point that wires together
// NewFramework, RunPollLoop / RunWatchLoop, signal handling, and
// Framework.Shutdown for a long-running connector binary.
func Run(opts Options) {
	fw, err := NewFramework(opts)
	if err != nil {
		slog.Default().With("connector", opts.Name).Error("bootstrap failed", "error", err)
		os.Exit(1)
	}
	defer fw.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			fw.Logger.Info("received signal, shutting down", "signal", sig)
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	_, isWatcher := opts.Connector.(Watcher)
	_, isListener := opts.Connector.(Listener)

	fw.Logger.Info("running initial poll")
	if err := runPoll(ctx, opts.Connector, fw.Checkpoint, fw.Metrics, fw.Logger, fw.FetchCounter); err != nil {
		if IsPermanent(err) {
			fw.Logger.Error("permanent error during initial poll", "error", err)
			os.Exit(1)
		}
		fw.Logger.Warn("transient error during initial poll", "error", err)
	} else {
		fw.Ready.Store(true)
		fw.Logger.Info("initial poll complete, connector ready")
	}

	if ctx.Err() != nil {
		return
	}

	// Poll-once mode: PollInterval == 0 and no watcher/listener.
	if opts.PollInterval == 0 && !isWatcher && !isListener {
		return
	}

	// Long-running mode needs a poll interval; fall back to 5m if
	// the caller didn't configure one but did register a watcher/listener.
	if fw.opts.PollInterval == 0 {
		fw.opts.PollInterval = 5 * time.Minute
	}

	if isWatcher {
		RunWatchLoop(ctx, fw, opts.Connector)
	} else {
		RunPollLoop(ctx, fw, opts.Connector)
	}

	fw.Logger.Info("connector stopped")
}

// RunPollLoop drives a Connector on a periodic poll schedule. It
// returns when ctx is cancelled. This is a standalone entry point
// for callers who built their own Framework via NewFramework.
func RunPollLoop(ctx context.Context, fw *Framework, c Connector) {
	runPollLoop(ctx, c, fw.PollInterval(), fw.Checkpoint, fw.Metrics, fw.Ready, fw.Logger, fw.FetchCounter)
}

// RunWatchLoop drives a Watcher -- a long-lived push/notification
// consumer with a periodic re-poll safety net. The connector argument
// must implement both Poll (Connector) and Watch (Watcher); the re-poll
// path needs Poll. Returns when ctx is cancelled. Permanent errors
// from Watch still exit the process.
func RunWatchLoop(ctx context.Context, fw *Framework, c Connector) {
	w, ok := c.(Watcher)
	if !ok {
		fw.Logger.Error("RunWatchLoop: connector does not implement Watcher")
		return
	}
	runWatchLoop(ctx, c, w, fw.PollInterval(), fw.Checkpoint, fw.Metrics, fw.Ready, fw.Logger, fw.FetchCounter)
}

// runPoll executes a single poll against the given Connector,
// resetting the fetch counter, recording duration/result metrics,
// and propagating any error. Exit shape (transient vs permanent)
// is the Connector's responsibility via PermanentError.
func runPoll(ctx context.Context, c Connector, cp Checkpoint, m *Metrics, logger *slog.Logger, fc *FetchCounter) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if fc != nil {
		fc.Reset()
	}
	start := time.Now()
	err := c.Poll(ctx, cp)
	if m != nil {
		m.RecordPollDuration(time.Since(start))
		if err != nil {
			m.RecordPoll("error")
			if IsPermanent(err) {
				m.RecordError("permanent")
			} else {
				m.RecordError("transient")
			}
		} else {
			m.RecordPoll("success")
		}
	}
	return err
}

func runWatchLoop(ctx context.Context, c Connector, watcher Watcher, interval time.Duration, cp Checkpoint, m *Metrics, ready *atomic.Bool, logger *slog.Logger, fc *FetchCounter) {
	pollTicker := time.NewTicker(interval)
	defer pollTicker.Stop()

	var wg sync.WaitGroup

	for {
		if ctx.Err() != nil {
			break
		}

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
			wg.Wait()
			if err != nil {
				if IsPermanent(err) {
					logger.Error("permanent watch error", "error", err)
					os.Exit(1)
				}
				logger.Warn("watch error, will re-poll and retry", "error", err)
				// Wait with cancellation support instead of blocking sleep.
				select {
				case <-time.After(interval):
				case <-ctx.Done():
					return
				}
				if err := runPoll(ctx, c, cp, m, logger, fc); err != nil {
					if IsPermanent(err) {
						logger.Error("permanent error during re-poll", "error", err)
						os.Exit(1)
					}
					logger.Warn("re-poll error", "error", err)
				} else {
					ready.Store(true)
				}
			}

		case <-pollTicker.C:
			watchCancel()
			wg.Wait()
			logger.Info("periodic re-poll")
			if err := runPoll(ctx, c, cp, m, logger, fc); err != nil {
				if IsPermanent(err) {
					logger.Error("permanent error during re-poll", "error", err)
					os.Exit(1)
				}
				logger.Warn("re-poll error", "error", err)
			}
		}
	}
}

func runPollLoop(ctx context.Context, c Connector, interval time.Duration, cp Checkpoint, m *Metrics, ready *atomic.Bool, logger *slog.Logger, fc *FetchCounter) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info("scheduled poll")
			if err := runPoll(ctx, c, cp, m, logger, fc); err != nil {
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

// selectBackend chooses between HTTPStagingBackend and filesystem StagingWriter
// based on the provided configuration. ingestURL takes precedence over stagingDir.
// Returns (backend, writer, error) where writer is nil in HTTP mode.
func selectBackend(name, ingestURL, stagingDir string, logger *slog.Logger) (StagingBackend, *StagingWriter, error) {
	if ingestURL != "" {
		backend := NewHTTPStagingBackend(ingestURL, name, &http.Client{Timeout: 30 * time.Second})
		logger.Info("using HTTP ingest backend", "url", ingestURL)
		return backend, nil, nil
	}
	if stagingDir != "" {
		w, err := NewStagingWriter(stagingDir, name)
		if err != nil {
			return nil, nil, fmt.Errorf("init staging writer: %w", err)
		}
		w.CleanOrphans()
		logger.Info("using filesystem staging backend", "dir", stagingDir)
		return w, w, nil
	}
	return nil, nil, fmt.Errorf("either GLOVEBOX_INGEST_URL or staging dir must be set")
}
