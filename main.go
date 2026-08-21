package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/ingest/archives"
	"github.com/leftathome/glovebox/internal/ingest/auth"
	"github.com/leftathome/glovebox/internal/ingest/peer"
	gloveboxmetrics "github.com/leftathome/glovebox/internal/metrics"
	"github.com/leftathome/glovebox/internal/pipeline"
	"github.com/leftathome/glovebox/internal/routing"
	"github.com/leftathome/glovebox/internal/sanitizeapi"
	"github.com/leftathome/glovebox/internal/scan"
	"github.com/leftathome/glovebox/internal/staging"
	"github.com/leftathome/glovebox/internal/subject"
	"github.com/leftathome/glovebox/internal/watcher"
)

// Version is set via -ldflags at build time, e.g.:
//
//	go build -ldflags "-X main.Version=v1.2.3"
var Version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config.json")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// Load the known-subjects registry. config.Validate already parsed it for
	// validation; this is the retained load whose registry (subjects + the
	// enforce flag) drives the fail-closed subject-resolution gate.
	reg, err := subject.Load(cfg.SubjectsFile)
	if err != nil {
		log.Fatalf("load subjects registry: %v", err)
	}

	rulesFile, err := os.Open(cfg.RulesFile)
	if err != nil {
		log.Fatalf("open rules file %s: %v", cfg.RulesFile, err)
	}
	rules, err := engine.LoadRules(rulesFile)
	rulesFile.Close()
	if err != nil {
		log.Fatalf("load rules: %v", err)
	}
	log.Printf("loaded %d rules, quarantine threshold: %.2f", len(rules.Rules), rules.QuarantineThreshold)

	registry := detector.NewDefaultRegistry()

	logger, err := audit.NewLogger(cfg.AuditDir)
	if err != nil {
		log.Fatalf("init audit logger: %v", err)
	}
	defer logger.Close()

	m, err := gloveboxmetrics.New()
	if err != nil {
		log.Fatalf("init metrics: %v", err)
	}
	defer m.Shutdown()

	// Bind the OTel global meter provider to ours so subsystems that
	// reach for otel.Meter(...) (e.g. internal/ingest/archives.Telemetry)
	// emit through the same Prometheus exporter as the rest of glovebox.
	otel.SetMeterProvider(m.Provider())

	sc, err := scan.New(rules, registry)
	if err != nil {
		log.Fatalf("build scanner: %v", err)
	}

	pool := pipeline.NewWorkerPool(cfg.ScanWorkers, time.Duration(cfg.ScanTimeoutSeconds)*time.Second, sc)

	// Bound each delivery so a wedged file op on a networked staging/quarantine
	// mount cannot stall the lone result consumer and deadlock the pipeline
	// (glovebox-lnzp). On timeout the item is left in staging and surfaced via
	// the delivery_timeouts metric; the consumer moves on.
	deliver := pipeline.WithTimeout(
		func(resp pipeline.ScanResponse) error {
			return deliverResult(resp, cfg, reg, logger, rules.QuarantineThreshold, m)
		},
		time.Duration(cfg.DeliveryTimeoutSeconds)*time.Second,
		func(resp pipeline.ScanResponse) {
			m.DeliveryTimeouts.Add(context.Background(), 1)
			log.Printf("delivery timed out for %s (staging/quarantine mount stuck?)", resp.Item.DirPath)
		},
	)
	router := pipeline.NewRouter(deliver)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start metrics server. /healthz is an ACTIVE liveness probe that verifies
	// the delivery mount is writable (see health.go) so a stale bind-mount EIO
	// restarts the pod instead of silently stalling delivery; /readyz gates on
	// startup completion. Co-located with /metrics on cfg.MetricsPort, matching
	// the connector framework's health surface.
	ready := &atomic.Bool{}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", livenessHandler(cfg.AgentsDir))
	mux.HandleFunc("/readyz", readinessHandler(ready))
	metricsServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.MetricsPort),
		Handler: mux,
	}
	go func() {
		log.Printf("metrics server listening on :%d", cfg.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server error: %v", err)
		}
	}()

	// Start ingest HTTP server if enabled. The mux is shared between
	// the legacy /v1/ingest connector endpoint (spec 08) and the
	// spec 13 archive-delivery /v1/archives* surface; both bind to
	// cfg.Ingest.Port so the chart's startup probe + NetworkPolicy
	// continue to target a single port.
	var ingestHandler *ingest.Handler
	var ingestServer *http.Server
	var ingestTLSServer *http.Server
	if cfg.Ingest.Enabled {
		ingestHandler = ingest.NewHandler(cfg.StagingDir, cfg.Ingest, cfg.AgentAllowlist)

		ingestMetrics, err := ingest.NewIngestMetrics(m.Provider())
		if err != nil {
			log.Fatalf("init ingest metrics: %v", err)
		}
		ingestHandler.SetMetrics(ingestMetrics)

		if err := ingestHandler.InitQueueDepth(); err != nil {
			log.Fatalf("init ingest queue depth: %v", err)
		}
		ingestHandler.SetReady()

		ingestMux := http.NewServeMux()
		ingestMux.Handle("/v1/ingest", ingestHandler)

		// Spec 13 archive-delivery: bind /v1/archives* onto the same mux
		// before the http.Server starts serving. bootstrapArchives is
		// nil-tolerant — when Auth.Enabled / Archives.Enabled are off it
		// returns without mounting anything; when a startup check fails
		// it mounts a 503 fallback on /v1/archives* and lets the rest of
		// the process run.
		if err := bootstrapArchives(ctx, cfg, ingestMux); err != nil {
			log.Fatalf("bootstrap archive listener: %v", err)
		}

		// Spec sanitize-gate (glovebox-t6fz): mount the synchronous
		// classify endpoint POST /v1/sanitize on the same ingest mux,
		// behind bearer-token auth. Gated on Ingest.Auth.Enabled so an
		// auth-disabled dev deploy does not expose an unauthenticated
		// gate. sc is the shared scanner built above, so the gate
		// enforces exactly what the async daemon enforces.
		if sc != nil && cfg.Ingest.Auth.Enabled {
			store, rl, pr, err := buildIngestAuth(ctx, cfg)
			if err != nil {
				log.Fatalf("sanitize auth: %v", err)
			}
			// Typed-nil telemetry: auth.Middleware requires a non-nil
			// TelemetryHook (it calls RecordAuth unconditionally); a bare
			// nil interface would panic. *archives.Telemetry's Record*
			// methods are nil-safe, so a typed nil is a safe no-op sink.
			mw := auth.Middleware(store, rl, pr, (*archives.Telemetry)(nil))
			sanMux := http.NewServeMux()
			sanitizeapi.HandlerFromMux(sanitizeapi.NewSanitizeHandler(sc), sanMux)
			ingestMux.Handle("/v1/sanitize", mw(sanMux))
			log.Printf("glovebox sanitize gate mounted on /v1/sanitize (bearer auth)")
		}

		// Mutual TLS listener. Spec 08 section 3.10 left /v1/ingest
		// unauthenticated behind a NetworkPolicy podSelector -- a label
		// any workload can claim, not an identity -- so the handler took
		// metadata.source on faith. Under mTLS the claimed source must
		// match the connector the client certificate names.
		//
		// Modes stage the migration: permissive opens both listeners so
		// connectors can move one at a time (watch the transport label on
		// glovebox_items_received_total drain to zero), required opens
		// only this one.
		if cfg.Ingest.TLS.Active() {
			tlsHandler := ingest.NewHandler(cfg.StagingDir, cfg.Ingest, cfg.AgentAllowlist)
			tlsHandler.SetMetrics(ingestMetrics)
			if err := tlsHandler.InitQueueDepth(); err != nil {
				log.Fatalf("init mTLS ingest queue depth: %v", err)
			}
			tlsHandler.SetReady()
			tlsHandler.SetPeerEnforcement(cfg.Ingest.TLS.SourceMatchEnforced(), "mtls")

			tlsMux := http.NewServeMux()
			tlsMux.Handle("/v1/ingest", peer.Middleware(cfg.Ingest.TLS.EffectiveTrustDomain())(tlsHandler))

			srv, err := ingest.StartServerMTLS(tlsMux, ingest.MTLSOptions{
				Port:         cfg.Ingest.TLS.Port,
				Timeout:      time.Duration(cfg.Ingest.RequestTimeoutSeconds) * time.Second,
				CertFile:     cfg.Ingest.TLS.CertFile,
				KeyFile:      cfg.Ingest.TLS.KeyFile,
				ClientCAFile: cfg.Ingest.TLS.ClientCAFile,
			})
			if err != nil {
				log.Fatalf("build mTLS ingest listener: %v", err)
			}
			ingestTLSServer = srv
			go func() {
				log.Printf("ingest mTLS server listening on :%d (mode=%s, source-match=%v)",
					cfg.Ingest.TLS.Port, cfg.Ingest.TLS.Mode, cfg.Ingest.TLS.SourceMatchEnforced())
				if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
					log.Printf("ingest mTLS server error: %v", err)
				}
			}()
		}

		if !cfg.Ingest.TLS.PlaintextActive() {
			// required mode: the unauthenticated listener is simply not
			// opened, so there is no path left that skips peer identity.
			log.Printf("plaintext ingest listener not opened (ingest.tls.mode=%s)", cfg.Ingest.TLS.Mode)
		} else {
			ingestHandler.SetPeerEnforcement(false, "plaintext")
			ingestServer = &http.Server{
				Addr:    fmt.Sprintf(":%d", cfg.Ingest.Port),
				Handler: ingestMux,
				// ReadHeaderTimeout bounds ONLY the header-read phase (slowloris
				// protection). ReadTimeout/WriteTimeout are left 0 (unbounded) so a
				// multi-GB archive PATCH -- the listener advertises Tus-Max-Size
				// 30 GiB -- is not killed by a whole-request deadline (glovebox-dddn:
				// a 60s ReadTimeout force-closed any upload >60s). Per-route body
				// bounds remain in place: /v1/ingest via http.MaxBytesReader (size),
				// /v1/archives PATCH via the handler's patchBodyReader idle timeout.
				ReadHeaderTimeout: time.Duration(cfg.Ingest.RequestTimeoutSeconds) * time.Second,
			}
			go func() {
				log.Printf("ingest server listening on :%d", cfg.Ingest.Port)
				if err := ingestServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Printf("ingest server error: %v", err)
				}
			}()
		}
	}

	// Clean stale pending files from previous run
	routing.CleanStalePending(cfg.AgentsDir, cfg.AgentAllowlist)

	// Ensure directories exist
	for _, dir := range []string{cfg.StagingDir, cfg.QuarantineDir, cfg.AuditDir, cfg.FailedDir} {
		os.MkdirAll(dir, 0755)
	}
	mainNotifyDir := filepath.Join(cfg.SharedDir, "glovebox-notifications")
	os.MkdirAll(mainNotifyDir, 0755)

	// Start worker pool
	go pool.Run(ctx)

	// Start watcher -- feeds items into worker pool
	w := watcher.New(cfg.StagingDir, time.Duration(cfg.PollIntervalSeconds)*time.Second, func(dirPath string) {
		item, err := staging.ReadStagingItem(dirPath, cfg.AgentAllowlist)
		if err != nil {
			reason := staging.RejectReasonFromError(err)
			log.Printf("reject %s (%s): %v", dirPath, reason, err)
			routing.RouteReject(dirPath, reason, nil, logger)
			return
		}
		// Write pending placeholder for ordered items before scanning
		if item.Metadata.Ordered {
			inboxDir := filepath.Join(cfg.AgentsDir, item.Metadata.DestinationAgent, "workspace", "inbox")
			routing.WritePending(item, inboxDir)
		}
		pool.Input() <- pipeline.ScanRequest{Item: item}
	})

	// Consume scan results: route them and flush accumulated ordered items
	// every poll interval, not just at shutdown (glovebox-v815). Runs on a
	// single goroutine so the router's pending map is never accessed
	// concurrently; closed when pool.Output() closes during shutdown.
	consumerDone := make(chan struct{})
	go func() {
		pipeline.ConsumeResults(pool.Output(), router,
			time.Duration(cfg.PollIntervalSeconds)*time.Second,
			func(err error) { log.Printf("route error: %v", err) })
		close(consumerDone)
	}()

	go w.Run(ctx)

	// Periodically rescan items in failed/ directory
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				entries, err := os.ReadDir(cfg.FailedDir)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					itemDir := filepath.Join(cfg.FailedDir, e.Name())
					item, err := staging.ReadStagingItem(itemDir, cfg.AgentAllowlist)
					if err != nil {
						log.Printf("failed rescan: reject %s: %v", itemDir, err)
						routing.RouteReject(itemDir, staging.RejectReasonFromError(err), nil, logger)
						continue
					}
					select {
					case pool.Input() <- pipeline.ScanRequest{Item: item}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	log.Printf("glovebox %s started: watching %s, %d workers, timeout %ds",
		Version, cfg.StagingDir, cfg.ScanWorkers, cfg.ScanTimeoutSeconds)
	ready.Store(true)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Printf("received %s, shutting down", sig)

	cancel()

	// Wait for the result consumer to stop before the final flush so the
	// router's pending map is not accessed concurrently.
	<-consumerDone

	// Flush any ordered items accumulated since the last periodic flush.
	if err := router.Flush(); err != nil {
		log.Printf("flush error: %v", err)
	}

	// Clean pending files for in-flight items
	routing.CleanStalePending(cfg.AgentsDir, cfg.AgentAllowlist)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if ingestServer != nil {
		ingestServer.Shutdown(shutdownCtx)
	}
	if ingestTLSServer != nil {
		ingestTLSServer.Shutdown(shutdownCtx)
	}
	metricsServer.Shutdown(shutdownCtx)

	log.Println("glovebox stopped")
}

func removePendingForItem(resp pipeline.ScanResponse, cfg config.Config) {
	if resp.Item.Metadata.Ordered {
		itemID := filepath.Base(resp.Item.DirPath)
		inboxDir := filepath.Join(cfg.AgentsDir, resp.Item.Metadata.DestinationAgent, "workspace", "inbox")
		routing.RemovePending(itemID, inboxDir)
	}
}

func deliverResult(resp pipeline.ScanResponse, cfg config.Config, reg *subject.SubjectRegistry, logger *audit.Logger, threshold float64, m *gloveboxmetrics.Metrics) error {
	ctx := context.Background()
	notifyDir := filepath.Join(cfg.SharedDir, "glovebox-notifications")

	recordVerdict := func(verdict string) {
		attrs := metric.WithAttributes(
			attribute.String("verdict", verdict),
			attribute.String("destination", resp.Item.Metadata.DestinationAgent),
			attribute.String("source", resp.Item.Metadata.Source),
		)
		m.ItemsProcessed.Add(ctx, 1, attrs)
		m.ProcessingDuration.Record(ctx, resp.Duration.Seconds(),
			metric.WithAttributes(attribute.String("source", resp.Item.Metadata.Source)))
		for _, sig := range resp.Signals {
			m.SignalsTriggered.Add(ctx, 1,
				metric.WithAttributes(attribute.String("rule_name", sig.Name)))
		}
	}

	if resp.TimedOut {
		m.ScanTimeouts.Add(ctx, 1,
			metric.WithAttributes(attribute.String("source", resp.Item.Metadata.Source)))
		scanResult := engine.ScanResult{
			Signals:    resp.Signals,
			TotalScore: 0,
			Verdict:    engine.VerdictQuarantine,
		}
		removePendingForItem(resp, cfg)
		recordVerdict("quarantine")
		return routing.RouteQuarantine(resp.Item, scanResult, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "scan_timeout")
	}

	if resp.Err != nil {
		log.Printf("scan error for %s, moving to failed/: %v", resp.Item.DirPath, resp.Err)
		removePendingForItem(resp, cfg)
		return routing.RouteToFailed(resp.Item.DirPath, cfg.FailedDir, "scan_error")
	}

	// Check audit degraded mode -- quarantine everything if audit is broken
	if logger.InDegradedMode() {
		m.AuditFailures.Add(ctx, 1)
		scanResult := engine.ScanResult{
			Signals:    resp.Signals,
			TotalScore: 0,
			Verdict:    engine.VerdictQuarantine,
		}
		removePendingForItem(resp, cfg)
		recordVerdict("quarantine")
		return routing.RouteQuarantine(resp.Item, scanResult, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "audit_failure")
	}

	// Tag-driven quarantine override (spec 12 §12 / Q-EARLY): items marked
	// with parse_status by the connector are forensic artifacts and bypass
	// the scanner verdict directly to quarantine.
	if routing.ShouldForceQuarantineFromTag(&resp.Item.Metadata) {
		tagScanResult := engine.ScanResult{
			Signals:    resp.Signals,
			TotalScore: 0,
			Verdict:    engine.VerdictQuarantine,
		}
		removePendingForItem(resp, cfg)
		recordVerdict("quarantine")
		return routing.RouteQuarantine(resp.Item, tagScanResult, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "parse_status_tag")
	}

	// The Scanner already produced the audit-complete result (score/verdict from
	// the scored+boost split, with result.Signals carrying the full fired-signal
	// set). Use it directly. resp.Result is non-nil on every non-error, non-timeout
	// path; guard defensively so an unexpected nil falls back to a safe quarantine.
	if resp.Result == nil {
		scanResult := engine.ScanResult{
			Signals:    resp.Signals,
			TotalScore: 0,
			Verdict:    engine.VerdictQuarantine,
		}
		removePendingForItem(resp, cfg)
		recordVerdict("quarantine")
		return routing.RouteQuarantine(resp.Item, scanResult, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "missing_scan_result")
	}
	result := *resp.Result

	// Fail-closed subject-resolution gate (spec 15 sec 5.2-5.3). Runs after the
	// scan verdict is computed and BEFORE the pass/quarantine routing so the
	// destination copy + audit carry the resolved entity_id. A resolved
	// principal mutates resp.Item.Metadata in place and is persisted to disk;
	// a subjectless item is untouched; an unresolved subject quarantines when
	// the registry enforces, otherwise passes through with a warning.
	switch action, err := routing.SubjectGate(&resp.Item, reg); {
	case err != nil:
		log.Printf("subject gate error for %s, moving to failed/: %v", resp.Item.DirPath, err)
		removePendingForItem(resp, cfg)
		return routing.RouteToFailed(resp.Item.DirPath, cfg.FailedDir, "subject_gate_error")
	case action == routing.GateQuarantine:
		removePendingForItem(resp, cfg)
		recordVerdict("quarantine")
		return routing.RouteQuarantine(resp.Item, result, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, routing.ReasonSubjectUnresolved)
	case action == routing.GatePassUnresolved:
		slog.Warn("subject unresolved; enforcement off, passing through", "data_subject", resp.Item.Metadata.DataSubject)
	}

	if result.Verdict == engine.VerdictQuarantine {
		notifyDir := notifyDir
		removePendingForItem(resp, cfg)
		recordVerdict("quarantine")
		return routing.RouteQuarantine(resp.Item, result, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "threshold_exceeded")
	}

	destDir, err := routing.ValidateDestination(resp.Item.Metadata.DestinationAgent, cfg.AgentsDir, cfg.AgentAllowlist)
	if err != nil {
		removePendingForItem(resp, cfg)
		recordVerdict("reject")
		return routing.RouteReject(resp.Item.DirPath, err.Error(), &resp.Item.Metadata, logger)
	}

	err = routing.RoutePass(resp.Item, result, destDir, logger, resp.Duration)
	if err == nil {
		removePendingForItem(resp, cfg)
		recordVerdict("pass")
	}
	return err
}
