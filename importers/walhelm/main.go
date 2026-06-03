// Command walhelm-importer drives a one-shot import of a finished
// staged archive directory into glovebox. The archive is a directory
// produced by the ingest pipeline (e.g. archives/<id>/), not a
// single flat file.
//
// Flow:
//   1. Parse CLI flags (or read GLOVEBOX_* env vars).
//   2. connector.NewFramework() bootstraps logger / config / rule
//      matcher / staging backend (HTTP if GLOVEBOX_INGEST_URL is set,
//      filesystem otherwise) / metrics / health server.
//   3. importer.RunOneShot() orchestrates the spec 09 §3.1 sequence:
//      ensure survey, decide resume action, load filter, hand off to
//      walhelmImporter.Import.
//   4. SIGTERM / SIGINT cancel the context; walhelmImporter.Import
//      writes manifest.status=interrupted so the next run can resume.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/importer"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable entry point: returns the exit code rather than
// calling os.Exit directly so integration tests can drive it without
// process-isolation.
func run(args []string) int {
	fs := flag.NewFlagSet("walhelm-importer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	source := fs.String("source", "", "path to the staged archive directory, e.g. archives/<id>/ (required)")
	filterPath := fs.String("filter", "", "path to filter JSON (optional)")
	ingestURL := fs.String("ingest-url", "", "glovebox ingest URL (overrides GLOVEBOX_INGEST_URL)")
	sourceName := fs.String("source-name", "walhelm", "value for ingest metadata source field")
	concurrency := fs.Int("concurrency", 8, "parallel ingest workers")
	surveyOnly := fs.Bool("survey-only", false, "generate/update survey only; skip ingest")
	regenerateSurvey := fs.Bool("regenerate-survey", false, "force survey regeneration")
	resumeFlag := fs.String("resume", "", `override automatic resume decision: "true", "false", or empty (default)`)
	configFile := fs.String("config", "", "path to config.json (default: $GLOVEBOX_IMPORTER_CONFIG or /etc/importer/config.json)")
	stateDir := fs.String("state-dir", "", "checkpoint state dir (default: $GLOVEBOX_STATE_DIR)")
	stagingDir := fs.String("staging-dir", "", "filesystem staging dir if no ingest-url (default: $GLOVEBOX_STAGING_DIR)")
	healthPort := fs.Int("health-port", 8080, "port for /healthz, /readyz, /metrics")
	fixedTagFlag := fs.String("fixed-tags", "", "comma-separated key=value pairs appended to every item's tag map")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *source == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --source is required")
		return 2
	}

	if *configFile == "" {
		*configFile = os.Getenv("GLOVEBOX_IMPORTER_CONFIG")
	}
	if *configFile == "" {
		*configFile = "/etc/importer/config.json"
	}
	if *stateDir == "" {
		*stateDir = os.Getenv("GLOVEBOX_STATE_DIR")
	}
	if *stagingDir == "" {
		*stagingDir = os.Getenv("GLOVEBOX_STAGING_DIR")
	}
	if *ingestURL != "" {
		// connector.selectBackend reads GLOVEBOX_INGEST_URL from env.
		// Set it from the flag so a single override path produces the
		// HTTP backend choice.
		_ = os.Setenv("GLOVEBOX_INGEST_URL", *ingestURL)
	}

	var resumeOverride *bool
	switch *resumeFlag {
	case "":
	case "true":
		t := true
		resumeOverride = &t
	case "false":
		f := false
		resumeOverride = &f
	default:
		fmt.Fprintln(os.Stderr, `ERROR: --resume must be "true", "false", or empty`)
		return 2
	}

	if *concurrency < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: --concurrency must be >= 1")
		return 2
	}

	var fixedTags []string
	if *fixedTagFlag != "" {
		for _, p := range strings.Split(*fixedTagFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				fixedTags = append(fixedTags, p)
			}
		}
	}

	ctx, cancel := signalContext()
	defer cancel()

	fw, err := connector.NewFramework(connector.Options{
		Name:       "walhelm-importer",
		StagingDir: *stagingDir,
		StateDir:   *stateDir,
		ConfigFile: *configFile,
		HealthPort: *healthPort,
		// No Connector / Setup -- one-shot importer pattern; the
		// framework's poll loop is not used.
	})
	if err != nil {
		slog.Error("framework bootstrap failed", "err", err)
		return 1
	}
	defer fw.Shutdown()

	imp := &walhelmImporter{
		fw:          fw,
		sourceName:  *sourceName,
		concurrency: *concurrency,
		fixedTags:   fixedTags,
	}

	cfg := importer.RunConfig{
		SourcePath:       *source,
		FilterPath:       *filterPath,
		ResumeOverride:   resumeOverride,
		SurveyOnly:       *surveyOnly,
		RegenerateSurvey: *regenerateSurvey,
	}

	if err := importer.RunOneShot(ctx, fw, imp, cfg); err != nil {
		// Cancellation is a clean exit -- we want the next run to
		// resume; a non-zero exit would imply an operator-actionable
		// failure. Distinguish via ctx.Err().
		if ctx.Err() != nil {
			slog.Warn("import cancelled; manifest left at status=interrupted for resume", "err", err)
			return 0
		}
		slog.Error("import failed", "err", err)
		return 1
	}
	return 0
}

// signalContext returns a context that is cancelled on SIGTERM /
// SIGINT.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		signal.Stop(ch)
		cancel()
	}()
	return ctx, cancel
}
