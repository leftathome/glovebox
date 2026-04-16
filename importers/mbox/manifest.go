package main

// Import session manifest for the mbox importer.
//
// This file defines the on-disk shape of `<source>.import-manifest.v1.json`
// along with a small set of helpers for reading, writing, and mutating it.
//
// Design references:
//   - docs/specs/09-mbox-importer-design.md §3.6 (schema), §3.6.1 (status),
//     §3.6.2 (errors cap).
//   - archiver spec 01 §4.3 (base manifest shape that this extends).
//
// Concurrency: these types are plain data. The worker pool guards concurrent
// mutation at a higher level (see bead glovebox-1s3); do not add locking here.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/leftathome/glovebox/importer"
)

// SchemaVersion is the on-disk schema version written into every manifest.
const SchemaVersion = "1.0"

// Kind is the archiver-pattern discriminator for an import manifest.
const Kind = "import"

// DefaultErrorCap is the maximum number of ErrorEntry values retained inline
// in the manifest. Overflows increment TruncatedErrorCount instead.
const DefaultErrorCap = 1000

// manifestSuffix is appended to the source path to form the manifest path.
const manifestSuffix = ".import-manifest.v1.json"

// The life-cycle status of an import run is represented by the shared
// importer.ManifestStatus enum. Transitions: StatusInProgress ->
// (StatusComplete | StatusInterrupted | StatusFailed); terminal states
// never transition further without a new run. See spec §3.6.1.

// validatedStatus is a JSON-level wrapper around importer.ManifestStatus
// that rejects unknown strings at parse time so a corrupt or future-schema
// manifest fails loud rather than being silently accepted. We only need
// the custom UnmarshalJSON; marshaling is delegated to the underlying
// string.
type validatedStatus importer.ManifestStatus

func (s validatedStatus) isValid() bool {
	switch importer.ManifestStatus(s) {
	case importer.StatusInProgress, importer.StatusComplete,
		importer.StatusInterrupted, importer.StatusFailed:
		return true
	}
	return false
}

func (s *validatedStatus) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	candidate := validatedStatus(raw)
	if !candidate.isValid() {
		return fmt.Errorf("mbox manifest: unknown status %q", raw)
	}
	*s = candidate
	return nil
}

// ImportManifestV1 is the v1 on-disk shape of an mbox import session.
//
// It extends the base archive-importer manifest (archiver spec 01 §4.3) with
// mbox-specific fields (message_ids_ingested, filter_hit_counts, etc.).
type ImportManifestV1 struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"` // always "import"

	SourcePath  string    `json:"source_path"`
	SourceSize  int64     `json:"source_size"`
	SourceMtime time.Time `json:"source_mtime"`
	SourceName  string    `json:"source_name"`

	// StatusValue holds the manifest life-cycle status. The field is
	// named StatusValue rather than Status so that the Status() method
	// below (which satisfies importer.Manifest) does not collide with
	// the field name. JSON encoding is unchanged: the `status` key is
	// preserved by the struct tag.
	StatusValue validatedStatus `json:"status"`
	TimestampStart time.Time       `json:"timestamp_start"`
	TimestampEnd   *time.Time      `json:"timestamp_end"` // nil until the run reaches a terminal state

	SurveyRef string `json:"survey_ref"`
	FilterRef string `json:"filter_ref"`

	// FilterRulesApplied is an opaque snapshot of the filter as it was at run
	// start. The concrete FilterRule type lives in a different bead
	// (glovebox-rmg); this package treats the value as raw JSON so that the
	// manifest can be round-tripped without importing the filter package.
	FilterRulesApplied json.RawMessage `json:"filter_rules_applied,omitempty"`

	Counts                   Counts         `json:"counts"`
	FilterHitCounts          map[string]int `json:"filter_hit_counts,omitempty"`
	DestinationRuleHitCounts map[string]int `json:"destination_rule_hit_counts,omitempty"`

	MessageIDsIngested []string `json:"message_ids_ingested,omitempty"`

	Errors              []ErrorEntry `json:"errors,omitempty"`
	TruncatedErrorCount int          `json:"truncated_error_count"`

	ResumeState ResumeState `json:"resume_state"`
}

// Counts aggregates per-message tallies for the run. Mirrors the base manifest
// counts block with mbox-specific keys (dedup skips, filtered-out messages).
type Counts struct {
	MessagesSeen         int   `json:"messages_seen"`
	MessagesIngested     int   `json:"messages_ingested"`
	MessagesFiltered     int   `json:"messages_filtered"`
	MessagesErrored      int   `json:"messages_errored"`
	MessagesDedupSkipped int   `json:"messages_dedup_skipped"`
	BytesProcessed       int64 `json:"bytes_processed"`
}

// ErrorEntry is one entry in the capped errors array.
type ErrorEntry struct {
	ByteOffset int64  `json:"byte_offset"`
	MessageID  string `json:"message_id"` // empty string when the message had no Message-ID header
	Reason     string `json:"reason"`
}

// ResumeState is the checkpoint mirror embedded in the manifest, sufficient to
// decide whether and where to resume after an interruption.
type ResumeState struct {
	ByteOffset            int64  `json:"byte_offset"`
	LastIngestedMessageID string `json:"last_ingested_message_id"`
}

// ManifestPath returns the conventional manifest path for a given mbox source.
// The convention is `<sourcePath>.import-manifest.v1.json`.
func ManifestPath(sourcePath string) string {
	return sourcePath + manifestSuffix
}

// LoadManifest reads and parses the manifest at path.
//
// If the file does not exist, the returned error satisfies errors.Is(err,
// fs.ErrNotExist) so callers can branch on "no manifest yet" without caring
// whether the underlying error is *PathError or syscall.ENOENT. A missing
// manifest is a normal "first run" state; callers should construct a fresh
// ImportManifestV1{} in that case.
func LoadManifest(path string) (*ImportManifestV1, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m ImportManifestV1
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("mbox manifest: parse %s: %w", path, err)
	}
	return &m, nil
}

// Write serializes the manifest to path atomically.
//
// Strategy: marshal, write to `<path>.tmp-<pid>-<unix-nanos>`, fsync, then
// os.Rename into place. os.Rename is atomic on POSIX for same-filesystem
// renames, so a kill -9 can occur at any point without leaving a partially
// written manifest. A kill during the tmp-file phase leaves behind a stray
// `.tmp-*` sibling which we best-effort clean up on our own error paths.
//
// The output is compact (no indentation): Write is called every N
// messages during an in-flight import, and pretty-printing a manifest
// with 25k+ message IDs produces a 2+ MB file. Tools like jq can
// pretty-print on demand if a human wants to read it.
func (m *ImportManifestV1) Write(path string) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("mbox manifest: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf("%s.tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("mbox manifest: create temp: %w", err)
	}
	// From this point on, any error path must remove the temp file.
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("mbox manifest: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("mbox manifest: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("mbox manifest: close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("mbox manifest: rename: %w", err)
	}
	return nil
}

// AddError appends entry to the Errors slice, capped at DefaultErrorCap.
// Overflows increment TruncatedErrorCount and are dropped. See spec §3.6.2.
//
// Returns true when the entry was appended, false when the cap had
// already been reached and TruncatedErrorCount was incremented instead.
// Callers can use the return value to, e.g., log the first retained
// error at INFO and suppress further identical ones.
func (m *ImportManifestV1) AddError(entry ErrorEntry) (appended bool) {
	if len(m.Errors) >= DefaultErrorCap {
		m.TruncatedErrorCount++
		return false
	}
	m.Errors = append(m.Errors, entry)
	return true
}

// IsStatusTerminal reports whether the manifest's Status is one of the
// terminal values (complete, interrupted, failed). Used by resume logic to
// decide whether the previous run ended cleanly.
func (m *ImportManifestV1) IsStatusTerminal() bool {
	switch importer.ManifestStatus(m.StatusValue) {
	case importer.StatusComplete, importer.StatusInterrupted, importer.StatusFailed:
		return true
	}
	return false
}

// The following three methods let *ImportManifestV1 satisfy the
// importer.Manifest interface (see importer/importer.go). RunOneShot
// consults them via that interface to drive the resume decision table
// in spec §3.1.1 without depending on the concrete mbox schema.

// Status returns the validated status field as the shared
// importer.ManifestStatus enum.
func (m *ImportManifestV1) Status() importer.ManifestStatus {
	return importer.ManifestStatus(m.StatusValue)
}

// ByteOffset returns the resume_state.byte_offset. A non-zero return is
// also the canonical "checkpoint exists" signal that importer.Decide
// consults (see importer/resume.go).
func (m *ImportManifestV1) ByteOffset() int64 {
	return m.ResumeState.ByteOffset
}

// MessageIDs returns the manifest's message_ids_ingested set, preserved
// across resume for dedup across the checkpoint boundary.
func (m *ImportManifestV1) MessageIDs() []string {
	return m.MessageIDsIngested
}

// Compile-time guarantee that *ImportManifestV1 satisfies the
// importer.Manifest contract RunOneShot depends on.
var _ importer.Manifest = (*ImportManifestV1)(nil)
