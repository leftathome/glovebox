// importer.go -- concrete mboxImporter implementing importer.Importer
// for use by main.go's RunOneShot call. See spec 09 §3 for the contract
// each method satisfies; this file is the wiring between the spec-09
// pipeline stages (parser / filter / dedup / ingest / manifest /
// checkpoint / survey) and the importer-package orchestration.
//
// Bounded concurrency is a small inline worker pool; the dedicated
// pool with retry semantics is tracked separately as glovebox-1s3 and
// can replace this when it lands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/importer"
)

const (
	manifestSchemaVersion = "import-manifest-v1"
	manifestKind          = "import"
	surveyTopN            = 20
	manifestWriteEveryN   = 1000
)

// mboxImporter implements importer.Importer for spec 09 mbox archives.
type mboxImporter struct {
	fw          *connector.Framework
	sourceName  string
	concurrency int
	// fixedTags appended to every item's tag map (origin_archive is
	// added automatically by ingest.BuildItemOptions). Operator-set
	// via --fixed-tag flag for now; empty slice means none.
	fixedTags []string
}

// Survey scans the entire mbox and writes the v1 survey sidecar. The
// sidecar is what the next run's resume logic consults to decide
// whether the archive has been modified since.
func (m *mboxImporter) Survey(ctx context.Context, path string) (importer.SurveyFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open source for survey: %w", err)
	}
	defer f.Close()

	scanner := NewScanner(f)
	sv, err := Aggregate(scanner, surveyTopN)
	if err != nil {
		return nil, fmt.Errorf("aggregate survey: %w", err)
	}

	dest := SurveyPath(path)
	if err := sv.Write(dest); err != nil {
		return nil, fmt.Errorf("write survey: %w", err)
	}
	m.fw.Logger.Info("survey written",
		"path", dest,
		"messages", sv.TotalMessages,
		"size_bytes", sv.SourceSize)
	return sv, nil
}

func (m *mboxImporter) LoadSurvey(path string) (importer.SurveyFile, error) {
	sv, err := LoadSurvey(SurveyPath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sv, nil
}

func (m *mboxImporter) LoadManifest(path string) (importer.Manifest, error) {
	mf, err := LoadManifest(ManifestPath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mf, nil
}

func (m *mboxImporter) LoadFilter(filterPath string) (importer.FilterConfig, error) {
	if filterPath == "" {
		return nil, nil
	}
	fc, err := LoadFilter(filterPath)
	if err != nil {
		return nil, fmt.Errorf("load filter %s: %w", filterPath, err)
	}
	return fc, nil
}

func (m *mboxImporter) ClearState(path string) error {
	for _, p := range []string{ManifestPath(path), CheckpointPath(path)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", p, err)
		}
	}
	return nil
}

// Import is the meat: open the source, seek to the resume offset if
// any, dedup against prior message-ids, apply the filter, ship each
// surviving message through the staging backend, and persist progress
// state (manifest + checkpoint) periodically. Returns nil on natural
// EOF, ctx.Err() on cancellation; either way the manifest is flushed
// before return so a follow-on run can resume.
func (m *mboxImporter) Import(
	ctx context.Context,
	path string,
	survey importer.SurveyFile,
	filterCfg importer.FilterConfig,
	decision importer.ResumeDecision,
) error {
	log := m.fw.Logger.With("source", path)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer f.Close()

	// Seek to resume offset if any. parser.Scanner consumes from the
	// current file position, so a seek leaves it consuming the right
	// region of the archive.
	if decision.ByteOffset > 0 {
		if _, err := f.Seek(decision.ByteOffset, 0); err != nil {
			return fmt.Errorf("seek to resume offset %d: %w", decision.ByteOffset, err)
		}
		log.Info("resuming from offset", "byte_offset", decision.ByteOffset)
	}

	scanner := NewScanner(f)
	dedup := NewDedupSet(decision.PreservedMessageIDs)
	// Track the byte offset of the last fully-consumed message so the
	// final checkpoint write is accurate. Scanner doesn't expose its
	// internal position; computing from msg.ByteOffset + msg.Size is
	// the equivalent and survives ctx cancellation cleanly.
	consumedOffset := decision.ByteOffset

	// Build (or update) the manifest skeleton.
	mf := newOrLoadManifest(path, decision, m.sourceName, survey, filterCfg)
	defer func() {
		// Best-effort final flush; ignore the error path so we don't
		// shadow a genuine import error from the caller's perspective.
		_ = mf.Write(ManifestPath(path))
	}()

	var fcSnapshot *FilterConfig
	if filterCfg != nil {
		if fc, ok := filterCfg.(*FilterConfig); ok {
			fcSnapshot = fc
		}
	}

	// Worker pool.
	type job struct {
		msg *Message
	}
	jobs := make(chan job, m.concurrency*2)
	var workerWG sync.WaitGroup
	var mu sync.Mutex // guards mf + dedup updates from worker goroutines

	for i := 0; i < m.concurrency; i++ {
		workerWG.Add(1)
		go func(workerID int) {
			defer workerWG.Done()
			for j := range jobs {
				m.ingestOne(ctx, j.msg, mf, dedup, &mu, log.With("worker", workerID))
			}
		}(i)
	}

	produceErr := func() error {
		defer close(jobs)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastWriteAtCount := 0
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !scanner.Scan() {
				return nil
			}
			msg := scanner.Message()

			consumedOffset = msg.ByteOffset + int64(msg.Size)
			mu.Lock()
			mf.Counts.MessagesSeen++
			mf.Counts.BytesProcessed += int64(msg.Size)
			seenN := mf.Counts.MessagesSeen
			mu.Unlock()

			// Filter (if configured).
			if fcSnapshot != nil {
				action, _, ruleKey := fcSnapshot.Evaluate(msg)
				if action == "exclude" {
					mu.Lock()
					mf.Counts.MessagesFiltered++
					if mf.FilterHitCounts == nil {
						mf.FilterHitCounts = make(map[string]int)
					}
					mf.FilterHitCounts[ruleKey]++
					mu.Unlock()
					continue
				}
			}

			// Dedup by Message-ID; messages with no Message-ID get
			// through (they can't collide with anything).
			if msg.MessageID != "" {
				mu.Lock()
				seen := dedup.Seen(msg.MessageID)
				mu.Unlock()
				if seen {
					mu.Lock()
					mf.Counts.MessagesDedupSkipped++
					mu.Unlock()
					continue
				}
			}

			select {
			case jobs <- job{msg: msg}:
			case <-ctx.Done():
				return ctx.Err()
			}

			// Periodic manifest flush + checkpoint write.
			if seenN-lastWriteAtCount >= manifestWriteEveryN {
				lastWriteAtCount = seenN
				m.flushProgress(path, mf, msg.ByteOffset+int64(msg.Size), &mu, log)
			}

			select {
			case <-ticker.C:
				// Light progress log; piggy-back to surface activity.
				mu.Lock()
				log.Info("import progress",
					"seen", mf.Counts.MessagesSeen,
					"ingested", mf.Counts.MessagesIngested,
					"filtered", mf.Counts.MessagesFiltered,
					"dedup_skipped", mf.Counts.MessagesDedupSkipped,
					"errored", mf.Counts.MessagesErrored)
				mu.Unlock()
			default:
			}
		}
	}()

	// Wait for the producer to finish (or fail) before closing jobs.
	workerWG.Wait()

	scanErr := scanner.Err()
	mu.Lock()
	mf.ResumeState.ByteOffset = consumedOffset
	if produceErr != nil {
		mf.StatusValue = validatedStatus(importer.StatusInterrupted)
	} else if scanErr != nil {
		mf.StatusValue = validatedStatus(importer.StatusFailed)
	} else {
		now := time.Now().UTC()
		mf.TimestampEnd = &now
		mf.StatusValue = validatedStatus(importer.StatusComplete)
	}
	mu.Unlock()

	log.Info("import finished",
		"status", string(mf.StatusValue),
		"seen", mf.Counts.MessagesSeen,
		"ingested", mf.Counts.MessagesIngested,
		"filtered", mf.Counts.MessagesFiltered,
		"dedup_skipped", mf.Counts.MessagesDedupSkipped,
		"errored", mf.Counts.MessagesErrored,
		"bytes_processed", mf.Counts.BytesProcessed)

	if produceErr != nil {
		return produceErr
	}
	if scanErr != nil {
		return fmt.Errorf("scan: %w", scanErr)
	}
	return nil
}

// ingestOne builds the staging item from a single message, posts it
// to the backend, and records the outcome in the manifest under mu.
// Errors are captured per-message; we do NOT abort the whole import
// on a per-message failure (HTTPStagingBackend already handles its
// own retry).
func (m *mboxImporter) ingestOne(
	ctx context.Context,
	msg *Message,
	mf *ImportManifestV1,
	dedup *DedupSet,
	mu *sync.Mutex,
	log *slog.Logger,
) {
	if err := ctx.Err(); err != nil {
		return
	}
	opts, err := BuildItemOptions(msg, m.fw.Matcher, m.sourceName, m.fixedTags)
	if err != nil {
		m.recordError(mu, mf, msg, fmt.Sprintf("build_item_options: %v", err))
		return
	}
	item, err := m.fw.Backend.NewItem(opts)
	if err != nil {
		m.recordError(mu, mf, msg, fmt.Sprintf("backend_new_item: %v", err))
		return
	}
	if err := item.WriteContent(msg.Raw); err != nil {
		m.recordError(mu, mf, msg, fmt.Sprintf("write_content: %v", err))
		return
	}
	if err := item.Commit(); err != nil {
		m.recordError(mu, mf, msg, fmt.Sprintf("commit: %v", err))
		return
	}

	mu.Lock()
	mf.Counts.MessagesIngested++
	mf.ResumeState.LastIngestedMessageID = msg.MessageID
	if msg.MessageID != "" {
		dedup.Add(msg.MessageID)
		mf.MessageIDsIngested = append(mf.MessageIDsIngested, msg.MessageID)
	}
	if opts.DestinationAgent != "" {
		if mf.DestinationRuleHitCounts == nil {
			mf.DestinationRuleHitCounts = make(map[string]int)
		}
		mf.DestinationRuleHitCounts[opts.DestinationAgent]++
	}
	mu.Unlock()
}

func (m *mboxImporter) recordError(mu *sync.Mutex, mf *ImportManifestV1, msg *Message, reason string) {
	mu.Lock()
	defer mu.Unlock()
	mf.Counts.MessagesErrored++
	mf.AddError(ErrorEntry{
		ByteOffset: msg.ByteOffset,
		MessageID:  msg.MessageID,
		Reason:     reason,
	})
}

// flushProgress writes the manifest + checkpoint files to disk so that
// a SIGTERM right after this can be resumed. Best-effort; logs but
// does not abort on failure.
func (m *mboxImporter) flushProgress(sourcePath string, mf *ImportManifestV1, offset int64, mu *sync.Mutex, log *slog.Logger) {
	mu.Lock()
	mf.ResumeState.ByteOffset = offset
	mu.Unlock()

	if err := mf.Write(ManifestPath(sourcePath)); err != nil {
		log.Warn("flush manifest failed", "err", err)
	}
	cp := &Checkpoint{
		ByteOffset:            offset,
		LastIngestedMessageID: mf.ResumeState.LastIngestedMessageID,
	}
	if err := cp.Write(CheckpointPath(sourcePath)); err != nil {
		log.Warn("flush checkpoint failed", "err", err)
	}
}

// newOrLoadManifest returns an existing manifest (if resuming) or
// constructs a fresh skeleton. The caller's RunOneShot already
// decided we should proceed; this is just the "make sure mf is
// populated" wrapper.
func newOrLoadManifest(
	sourcePath string,
	decision importer.ResumeDecision,
	sourceName string,
	survey importer.SurveyFile,
	filterCfg importer.FilterConfig,
) *ImportManifestV1 {
	existing, _ := LoadManifest(ManifestPath(sourcePath))
	if existing != nil && decision.Action == importer.Resume {
		// Reset status to in-progress for the resumed run; leave
		// counters + message-id history intact.
		existing.StatusValue = validatedStatus(importer.StatusInProgress)
		return existing
	}

	sourceSize, sourceMtime := statSource(sourcePath)
	mf := &ImportManifestV1{
		SchemaVersion:   manifestSchemaVersion,
		Kind:            manifestKind,
		SourcePath:      sourcePath,
		SourceSize:      sourceSize,
		SourceMtime:     sourceMtime,
		SourceName:      sourceName,
		StatusValue:     validatedStatus(importer.StatusInProgress),
		TimestampStart:  time.Now().UTC(),
		SurveyRef:       filepath.Base(SurveyPath(sourcePath)),
		FilterRef:       "",
	}
	if filterCfg != nil {
		if raw, err := json.Marshal(filterCfg); err == nil {
			mf.FilterRulesApplied = raw
		}
	}
	_ = survey // (held for future use -- e.g. snapshot per-rule counts upfront)
	return mf
}

func statSource(path string) (int64, time.Time) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}
	}
	return st.Size(), st.ModTime().UTC()
}
