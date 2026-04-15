package importer

import (
	"context"
	"errors"
	"fmt"

	"github.com/leftathome/glovebox/connector"
)

// RunConfig holds the per-invocation knobs RunOneShot consults. It is
// the importer-runtime analogue of connector.Options -- all the values
// that are specific to a single archive/command invocation rather than
// to the shared framework bootstrap.
type RunConfig struct {
	// SourcePath is the filesystem path to the archive (e.g. the
	// mbox file). Required.
	SourcePath string

	// FilterPath is the path to the user-authored filter JSON.
	// Empty means "no filter; default action per implementation".
	FilterPath string

	// ResumeOverride mirrors the --resume CLI flag: nil means "use
	// the default rule in Decide", *true forces resume semantics,
	// *false forces a fresh start with state cleared.
	ResumeOverride *bool

	// SurveyOnly, when true, generates or refreshes the survey and
	// returns without running an import.
	SurveyOnly bool

	// RegenerateSurvey, when true, forces survey regeneration even
	// if an existing survey is fresh.
	RegenerateSurvey bool
}

// RunOneShot orchestrates a single importer run: ensure a fresh
// survey, short-circuit for survey-only / already-complete / require-
// explicit-resume states, load the filter, and delegate to the
// format-specific Import. The fw argument is the shared Framework
// produced by connector.NewFramework; RunOneShot uses its logger and
// leaves it to Import to reach for fw.Backend / fw.Matcher / etc. as
// needed through its own captured reference.
//
// Order of operations (per spec §3.1):
//  1. Survey (either use existing fresh one, or generate -- also
//     forced by RegenerateSurvey).
//  2. If SurveyOnly, return.
//  3. Load manifest and checkpoint; Decide(...) the resume action.
//  4. ExitComplete -> return nil. RequireExplicitResume without
//     override -> return an error. Override=false -> ClearState to
//     wipe manifest+checkpoint before proceeding.
//  5. Load filter.
//  6. Call Import with the resume decision.
//
// Errors from any step propagate wrapped with fmt.Errorf so callers
// can errors.Is against the underlying cause.
func RunOneShot(ctx context.Context, fw *connector.Framework, i Importer, cfg RunConfig) error {
	if fw == nil {
		return errors.New("importer: RunOneShot requires a non-nil Framework")
	}
	if i == nil {
		return errors.New("importer: RunOneShot requires a non-nil Importer")
	}
	if cfg.SourcePath == "" {
		return errors.New("importer: RunConfig.SourcePath is required")
	}

	log := fw.Logger
	if log != nil {
		log = log.With("source", cfg.SourcePath)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// --- 1) Survey --------------------------------------------------
	survey, err := ensureSurvey(ctx, i, cfg)
	if err != nil {
		return fmt.Errorf("importer: survey: %w", err)
	}

	// --- 2) Survey-only short-circuit ------------------------------
	if cfg.SurveyOnly {
		if log != nil {
			log.Info("survey-only mode: exiting after survey")
		}
		return nil
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// --- 3) Resume decision ---------------------------------------
	manifest, err := i.LoadManifest(cfg.SourcePath)
	if err != nil {
		return fmt.Errorf("importer: load manifest: %w", err)
	}
	var (
		status          ManifestStatus
		preservedOffset int64
		preservedIDs    []string
	)
	if manifest != nil {
		status = manifest.Status()
		preservedOffset = manifest.ByteOffset()
		preservedIDs = manifest.MessageIDs()
	}

	decision := Decide(manifest, cfg.ResumeOverride)
	if log != nil {
		log.Info("resume decision",
			"status", string(status),
			"byte_offset", preservedOffset,
			"action", decision.Action.String())
	}

	// --- 4) Branch on decision ------------------------------------
	switch decision.Action {
	case ExitComplete:
		if log != nil {
			log.Info("archive already imported; nothing to do")
		}
		return nil

	case RequireExplicitResume:
		return fmt.Errorf("importer: previous run ended in status %q; pass --resume=true to retry or --resume=false to start over", status)

	case Resume:
		// Fill in what the concrete manifest knows about the resume
		// point so Import receives a self-contained decision.
		decision.ByteOffset = preservedOffset
		decision.PreservedMessageIDs = preservedIDs

	case StartFresh:
		// If prior state exists (manifest or its embedded resume
		// checkpoint) and we're starting fresh, wipe it so the
		// implementation starts from zero without stale files
		// underfoot. Covers both explicit --resume=false and the
		// implicit "crashed in_progress" case.
		if manifest != nil {
			if err := i.ClearState(cfg.SourcePath); err != nil {
				return fmt.Errorf("importer: clear prior state: %w", err)
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// --- 5) Filter -------------------------------------------------
	filter, err := i.LoadFilter(cfg.FilterPath)
	if err != nil {
		return fmt.Errorf("importer: load filter: %w", err)
	}

	// --- 6) Import -------------------------------------------------
	if log != nil {
		log.Info("starting import",
			"action", decision.Action.String(),
			"byte_offset", decision.ByteOffset,
			"preserved_ids", len(decision.PreservedMessageIDs))
	}
	if err := i.Import(ctx, cfg.SourcePath, survey, filter, decision); err != nil {
		return fmt.Errorf("importer: import: %w", err)
	}
	return nil
}

// ensureSurvey returns a fresh survey for the source, running Survey
// only if necessary. The rules: if RegenerateSurvey is set, always
// regenerate; otherwise if a loaded survey exists and is not stale,
// reuse it; otherwise generate a new one.
func ensureSurvey(ctx context.Context, i Importer, cfg RunConfig) (SurveyFile, error) {
	if cfg.RegenerateSurvey {
		return i.Survey(ctx, cfg.SourcePath)
	}
	existing, err := i.LoadSurvey(cfg.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("load existing survey: %w", err)
	}
	if existing != nil {
		stale, err := existing.IsStale(cfg.SourcePath)
		if err != nil {
			return nil, fmt.Errorf("check survey staleness: %w", err)
		}
		if !stale {
			return existing, nil
		}
	}
	return i.Survey(ctx, cfg.SourcePath)
}
