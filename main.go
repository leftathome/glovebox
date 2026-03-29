package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/glovebox/internal/audit"
	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
	gloveboxmetrics "github.com/leftathome/glovebox/internal/metrics"
	"github.com/leftathome/glovebox/internal/pipeline"
	"github.com/leftathome/glovebox/internal/routing"
	"github.com/leftathome/glovebox/internal/staging"
	"github.com/leftathome/glovebox/internal/watcher"
)

func main() {
	configPath := flag.String("config", "", "path to config.json")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
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

	matchers, detectors := buildScanFuncs(rules, registry)

	// Pre-compute boost rules from config (static, don't rebuild per item)
	boostConfig := make(map[string]float64)
	for _, rule := range rules.Rules {
		if rule.Behavior == "weight_booster" {
			boostConfig[rule.Name] = rule.BoostFactor
		}
	}

	pool := pipeline.NewWorkerPool(cfg.ScanWorkers, time.Duration(cfg.ScanTimeoutSeconds)*time.Second)

	router := pipeline.NewRouter(func(resp pipeline.ScanResponse) error {
		return deliverResult(resp, cfg, logger, rules.QuarantineThreshold, boostConfig, m)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start metrics server
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
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
		pool.Input() <- pipeline.ScanRequest{
			Item:      item,
			Matchers:  matchers,
			Detectors: detectors,
		}
	})

	// Consume scan results and route them
	go func() {
		for resp := range pool.Output() {
			if err := router.Route(resp); err != nil {
				log.Printf("route error: %v", err)
			}
		}
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
					case pool.Input() <- pipeline.ScanRequest{
						Item:      item,
						Matchers:  matchers,
						Detectors: detectors,
					}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	log.Printf("glovebox v0.1.0 started: watching %s, %d workers, timeout %ds",
		cfg.StagingDir, cfg.ScanWorkers, cfg.ScanTimeoutSeconds)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Printf("received %s, shutting down", sig)

	cancel()

	// Flush ordered items
	if err := router.Flush(); err != nil {
		log.Printf("flush error: %v", err)
	}

	// Clean pending files for in-flight items
	routing.CleanStalePending(cfg.AgentsDir, cfg.AgentAllowlist)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	metricsServer.Shutdown(shutdownCtx)

	log.Println("glovebox stopped")
}

func makeMatcherScanFunc(m engine.Matcher, rule engine.Rule) engine.ScanFunc {
	return func(content []byte) ([]engine.Signal, error) {
		results, err := m.Match(content, rule.Patterns)
		if err != nil || len(results) == 0 {
			return nil, err
		}
		matched := make([]string, len(results))
		for i, r := range results {
			matched[i] = fmt.Sprintf("%s at %d", r.Pattern, r.Position)
		}
		return []engine.Signal{{
			Name:    rule.Name,
			Weight:  rule.Weight,
			Matched: strings.Join(matched, "; "),
		}}, nil
	}
}

func buildScanFuncs(rules engine.RuleConfig, registry *detector.Registry) ([]engine.ScanFunc, []engine.ScanFunc) {
	var matchers []engine.ScanFunc
	var detectors []engine.ScanFunc

	for _, rule := range rules.Rules {
		rule := rule
		switch rule.MatchType {
		case engine.MatchSubstring:
			matchers = append(matchers, makeMatcherScanFunc(engine.SubstringMatcher{}, rule))

		case engine.MatchSubstringCaseInsensitive:
			matchers = append(matchers, makeMatcherScanFunc(engine.CaseInsensitiveMatcher{}, rule))

		case engine.MatchRegex:
			m, err := engine.NewRegexMatcher(rule.Patterns)
			if err != nil {
				log.Fatalf("compile regex for rule %s: %v", rule.Name, err)
			}
			matchers = append(matchers, makeMatcherScanFunc(m, rule))

		case engine.MatchCustomDetector:
			d, ok := registry.Get(rule.Detector)
			if !ok {
				log.Fatalf("unknown detector %q for rule %s", rule.Detector, rule.Name)
			}
			detectors = append(detectors, func(content []byte) ([]engine.Signal, error) {
				signals, err := d.Detect(content)
				if err != nil {
					return nil, err
				}
				// Override signal name and weight with rule config values
				for i := range signals {
					signals[i].Name = rule.Name
					signals[i].Weight = rule.Weight
				}
				return signals, nil
			})
		}
	}

	return matchers, detectors
}

func removePendingForItem(resp pipeline.ScanResponse, cfg config.Config) {
	if resp.Item.Metadata.Ordered {
		itemID := filepath.Base(resp.Item.DirPath)
		inboxDir := filepath.Join(cfg.AgentsDir, resp.Item.Metadata.DestinationAgent, "workspace", "inbox")
		routing.RemovePending(itemID, inboxDir)
	}
}

func deliverResult(resp pipeline.ScanResponse, cfg config.Config, logger *audit.Logger, threshold float64, boostConfig map[string]float64, m *gloveboxmetrics.Metrics) error {
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

	// Separate boost signals from scoring signals in a single pass
	var boosts []engine.BoostRule
	var scoringSignals []engine.Signal
	for _, sig := range resp.Signals {
		if factor, ok := boostConfig[sig.Name]; ok {
			boosts = append(boosts, engine.BoostRule{Name: sig.Name, BoostFactor: factor})
		} else {
			scoringSignals = append(scoringSignals, sig)
		}
	}

	result := engine.ScoreSignals(scoringSignals, boosts, threshold)
	result.Signals = resp.Signals // Preserve all signals including boosts for audit

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
