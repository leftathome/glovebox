package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

	pool := pipeline.NewWorkerPool(cfg.ScanWorkers, time.Duration(cfg.ScanTimeoutSeconds)*time.Second)

	router := pipeline.NewRouter(func(resp pipeline.ScanResponse) error {
		return deliverResult(resp, cfg, logger, rules.QuarantineThreshold)
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
	notifyDir := cfg.SharedDir + "/glovebox-notifications"
	os.MkdirAll(notifyDir, 0755)

	// Start worker pool
	go pool.Run(ctx)

	// Start watcher -- feeds items into worker pool
	w := watcher.New(cfg.StagingDir, time.Duration(cfg.PollIntervalSeconds)*time.Second, func(dirPath string) {
		item, err := staging.ReadStagingItem(dirPath, cfg.AgentAllowlist)
		if err != nil {
			log.Printf("reject %s: %v", dirPath, err)
			var meta *staging.ItemMetadata
			routing.RouteReject(dirPath, "validation_failed", meta, logger)
			return
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

func buildScanFuncs(rules engine.RuleConfig, registry *detector.Registry) ([]engine.ScanFunc, []engine.ScanFunc) {
	var matchers []engine.ScanFunc
	var detectors []engine.ScanFunc

	for _, rule := range rules.Rules {
		rule := rule
		switch rule.MatchType {
		case engine.MatchSubstring:
			m := engine.SubstringMatcher{}
			matchers = append(matchers, func(content []byte) ([]engine.Signal, error) {
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
			})

		case engine.MatchSubstringCaseInsensitive:
			m := engine.CaseInsensitiveMatcher{}
			matchers = append(matchers, func(content []byte) ([]engine.Signal, error) {
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
			})

		case engine.MatchRegex:
			m, err := engine.NewRegexMatcher(rule.Patterns)
			if err != nil {
				log.Fatalf("compile regex for rule %s: %v", rule.Name, err)
			}
			matchers = append(matchers, func(content []byte) ([]engine.Signal, error) {
				results, err := m.Match(content, nil)
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
			})

		case engine.MatchCustomDetector:
			d, ok := registry.Get(rule.Detector)
			if !ok {
				log.Fatalf("unknown detector %q for rule %s", rule.Detector, rule.Name)
			}
			detectors = append(detectors, func(content []byte) ([]engine.Signal, error) {
				return d.Detect(content)
			})
		}
	}

	return matchers, detectors
}

func deliverResult(resp pipeline.ScanResponse, cfg config.Config, logger *audit.Logger, threshold float64) error {
	if resp.TimedOut {
		notifyDir := cfg.SharedDir + "/glovebox-notifications"
		scanResult := engine.ScanResult{
			Signals:    resp.Signals,
			TotalScore: 0,
			Verdict:    engine.VerdictQuarantine,
		}
		return routing.RouteQuarantine(resp.Item, scanResult, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "scan_timeout")
	}

	if resp.Err != nil {
		log.Printf("scan error for %s: %v", resp.Item.DirPath, resp.Err)
		return routing.RouteReject(resp.Item.DirPath, "scan_error", &resp.Item.Metadata, logger)
	}

	// Check audit degraded mode -- quarantine everything if audit is broken
	if logger.InDegradedMode() {
		notifyDir := cfg.SharedDir + "/glovebox-notifications"
		scanResult := engine.ScanResult{
			Signals:    resp.Signals,
			TotalScore: 0,
			Verdict:    engine.VerdictQuarantine,
		}
		return routing.RouteQuarantine(resp.Item, scanResult, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "audit_failure")
	}

	// Score signals
	var signals []engine.Signal
	var boosts []engine.BoostRule
	for _, sig := range resp.Signals {
		signals = append(signals, sig)
	}

	// Find boost rules from the scan signals
	for _, sig := range resp.Signals {
		if sig.Name == "non_english_content" {
			boosts = append(boosts, engine.BoostRule{Name: sig.Name, BoostFactor: 1.5})
		}
	}

	// Remove boost signals from scoring (they have weight 0 and are applied as multipliers)
	var scoringSignals []engine.Signal
	for _, sig := range signals {
		if sig.Name != "non_english_content" {
			scoringSignals = append(scoringSignals, sig)
		}
	}

	result := engine.ScoreSignals(scoringSignals, boosts, threshold)
	result.Signals = signals // Preserve all signals including boosts for audit

	if result.Verdict == engine.VerdictQuarantine {
		notifyDir := cfg.SharedDir + "/glovebox-notifications"
		return routing.RouteQuarantine(resp.Item, result, cfg.QuarantineDir, notifyDir, logger, threshold, resp.Duration, "threshold_exceeded")
	}

	destDir, err := routing.ValidateDestination(resp.Item.Metadata.DestinationAgent, cfg.AgentsDir, cfg.AgentAllowlist)
	if err != nil {
		return routing.RouteReject(resp.Item.DirPath, err.Error(), &resp.Item.Metadata, logger)
	}

	return routing.RoutePass(resp.Item, result, destDir, logger, resp.Duration)
}
