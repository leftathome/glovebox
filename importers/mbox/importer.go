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

	// Worker pool (extracted into workers.go for testability;
	// glovebox-1s3). The pool calls one IngestFunc per message; this
	// closure is the production wiring through the connector
	// framework's HTTPStagingBackend (which owns retry / 4xx-vs-5xx /
	// 429+Retry-After semantics).
	var mu sync.Mutex // guards mf + dedup + inflight; held briefly per update
	// inflight tracks the start byte offset of every message dispatched to the
	// pool that has not yet produced a result. On interruption the resume point
	// is the LOW-WATER mark over this set (not the producer's high-water scan
	// position), so scanned-but-not-yet-ingested messages are re-read and
	// re-attempted on resume rather than skipped (glovebox-92qr). Cross-run
	// dedup then drops the ones already confirmed -> at-least-once.
	inflight := make(map[int64]struct{})
	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: m.concurrency,
		QueueSize:   m.concurrency * 2,
		Ingest: func(_ context.Context, msg *Message) error {
			opts, err := BuildItemOptions(msg, m.fw.Matcher, m.sourceName, m.fixedTags)
			if err != nil {
				return fmt.Errorf("build_item_options: %w", err)
			}
			item, err := m.fw.Backend.NewItem(opts)
			if err != nil {
				return fmt.Errorf("backend_new_item: %w", err)
			}
			if err := item.WriteContent(msg.Raw); err != nil {
				return fmt.Errorf("write_content: %w", err)
			}
			if err := item.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			return nil
		},
	})
	pool.Start(ctx)

	// Collector goroutine: receives per-message outcomes and applies
	// them to the manifest + dedup tracker. Single-writer for the
	// mf.Counts.MessagesIngested / Errored / MessageIDsIngested /
	// DestinationRuleHitCounts fields; the producer (below) writes a
	// disjoint field set so the mutex is uncontended in steady state
	// but kept for correctness when flushProgress reads the whole
	// struct under the same lock.
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for r := range pool.Results() {
			switch r.Status {
			case StatusOK:
				mu.Lock()
				mf.Counts.MessagesIngested++
				mf.ResumeState.LastIngestedMessageID = r.Message.MessageID
				if r.Message.MessageID != "" {
					dedup.Add(r.Message.MessageID)
					mf.MessageIDsIngested = append(mf.MessageIDsIngested, r.Message.MessageID)
				}
				delete(inflight, r.Message.ByteOffset)
				// DestinationRuleHitCounts is per the matched
				// destination; we'd need to plumb it from the pool
				// to know it. Deferred -- the rule-hit metric is
				// computed at BuildItemOptions time but discarded
				// here. Acceptable for v1; a richer WorkerResult
				// could carry it forward.
				mu.Unlock()
			case StatusFailed:
				m.recordError(&mu, mf, r.Message, r.Err.Error())
				mu.Lock()
				delete(inflight, r.Message.ByteOffset)
				mu.Unlock()
			case StatusCancelled:
				// Pool drained on cancellation; intentionally do not
				// bump any manifest counter. CRUCIALLY, leave this
				// message in inflight: it was dispatched but never
				// ingested, so it must hold the low-water resume mark
				// back to its offset. The next run re-reads from there
				// and re-attempts it (dedup drops any that did make it
				// through) -- this is what makes resume at-least-once
				// (glovebox-92qr).
			}
		}
	}()

	produceErr := func() error {
		defer close(pool.Jobs())
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

			// Mark in flight BEFORE dispatch so a worker can never emit a
			// result before the producer has recorded the message. This
			// message has already advanced consumedOffset (the producer
			// high-water) past itself, so it MUST stay in inflight until a
			// worker confirms it ingested -- whether it is dispatched and
			// later cancelled, or never dispatched because ctx was cancelled
			// here, it needs re-reading on resume and so must hold the
			// low-water mark back to its offset (glovebox-92qr).
			mu.Lock()
			inflight[msg.ByteOffset] = struct{}{}
			mu.Unlock()
			select {
			case pool.Jobs() <- msg:
			case <-ctx.Done():
				return ctx.Err()
			}

			// Periodic manifest flush + checkpoint write. Persist the
			// low-water resume offset (earliest still-in-flight message),
			// not the producer high-water, so a crash between flushes
			// cannot skip queued-but-uningested messages (glovebox-92qr).
			if seenN-lastWriteAtCount >= manifestWriteEveryN {
				lastWriteAtCount = seenN
				mu.Lock()
				off := resumeOffsetLocked(consumedOffset, inflight)
				mu.Unlock()
				m.flushProgress(path, mf, off, &mu, log)
			}

			select {
			case <-ticker.C:
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

	// Wait for the collector to drain (which happens once pool.Jobs is
	// closed and every worker has emitted its result + the closer
	// goroutine has closed pool.Results).
	<-collectorDone

	scanErr := scanner.Err()
	mu.Lock()
	// The collector has drained (every result processed), so inflight now
	// holds exactly the messages that were dispatched/scanned but never
	// confirmed ingested (cancelled or never-dispatched on interruption).
	// Resume from the low-water mark over that set so none are skipped; on a
	// clean EOF the set is empty and this is just consumedOffset (glovebox-92qr).
	mf.ResumeState.ByteOffset = resumeOffsetLocked(consumedOffset, inflight)
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

// (ingestOne removed; the IngestFunc closure inside Import + the
// collector goroutine that consumes pool.Results now own that
// per-message responsibility. See workers.go for the pool API.)

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
// resumeOffsetLocked returns the byte offset the next run should resume from:
// the low-water mark over all still-in-flight messages (dispatched or scanned
// but not yet confirmed ingested), or consumed -- the producer high-water --
// when nothing is in flight. Resuming from this point and relying on cross-run
// dedup to drop already-confirmed messages makes interruption at-least-once
// (glovebox-92qr). Erring smaller is always safe (it only re-reads more);
// caller must hold the mutex guarding inflight.
func resumeOffsetLocked(consumed int64, inflight map[int64]struct{}) int64 {
	off := consumed
	for o := range inflight {
		if o < off {
			off = o
		}
	}
	return off
}

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
		SchemaVersion:  manifestSchemaVersion,
		Kind:           manifestKind,
		SourcePath:     sourcePath,
		SourceSize:     sourceSize,
		SourceMtime:    sourceMtime,
		SourceName:     sourceName,
		StatusValue:    validatedStatus(importer.StatusInProgress),
		TimestampStart: time.Now().UTC(),
		SurveyRef:      filepath.Base(SurveyPath(sourcePath)),
		FilterRef:      "",
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
