package main

// Survey generation for the mbox importer.
//
// A survey is a streaming, ingest-free aggregate report over an mbox source:
// per-label counts, top-N list IDs and direct senders, and coarse date/size
// histograms. The survey is the single source of truth for filter authoring
// (see docs/specs/09-mbox-importer-design.md §3.3) and is consulted by
// RunOneShot to decide whether a prior survey is stale for the current
// source file (§3.3.2).
//
// This file defines the on-disk survey schema (§3.3.1), the Aggregate
// function that streams a *Scanner into a filled SurveyV1, and atomic
// Write / LoadSurvey / IsStale helpers. The atomic write strategy mirrors
// manifest.go; keep the two in sync if one changes.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/leftathome/glovebox/importer"
)

// SurveySchemaVersion is the on-disk schema version written into every
// survey produced by this package.
const SurveySchemaVersion = "1.0"

// surveySuffix is appended to the source path to form the survey path.
const surveySuffix = ".survey.v1.json"

// DefaultSurveyTopN is the default cap for the ListIDs and Senders arrays
// when the importer binary drives Aggregate. Callers may pass a different
// value directly. See spec §3.3.1 ("top N without List-Id, configurable,
// default 100").
const DefaultSurveyTopN = 100

// Date histogram bucket labels. Spec §3.3.1 enumerates pre-2005 through
// 2020-2025; we extend with post-2025 to cover messages dated in the
// current decade and unknown_date for zero-value times (missing or
// unparseable Date headers).
const (
	BucketPre2005    = "pre-2005"
	Bucket2005to2010 = "2005-2010"
	Bucket2010to2015 = "2010-2015"
	Bucket2015to2020 = "2015-2020"
	Bucket2020to2025 = "2020-2025"
	BucketPost2025   = "post-2025"
	BucketUnknown    = "unknown_date"
)

// Size histogram bucket labels. Spec §3.3.1 lists lt_10kb through gt_10mb.
const (
	SizeLt10KB    = "lt_10kb"
	Size10KBto1MB = "10kb_to_1mb"
	Size1MBto10MB = "1mb_to_10mb"
	SizeGt10MB    = "gt_10mb"
)

// SurveyV1 is the on-disk shape of an mbox survey (§3.3.1).
type SurveyV1 struct {
	SchemaVersion string `json:"schema_version"`

	SourcePath  string    `json:"source_path"`
	SourceSize  int64     `json:"source_size"`
	SourceMtime time.Time `json:"source_mtime"`

	SurveyStartedAt   time.Time `json:"survey_started_at"`
	SurveyCompletedAt time.Time `json:"survey_completed_at"`

	TotalMessages     int   `json:"total_messages"`
	TotalBytes        int64 `json:"total_bytes"`
	MalformedMessages int   `json:"malformed_messages"`

	Labels        map[string]int `json:"labels"`
	ListIDs       []ListIDCount  `json:"list_ids"`
	Senders       []SenderCount  `json:"senders"`
	DateHistogram map[string]int `json:"date_histogram"`
	SizeHistogram map[string]int `json:"size_histogram"`
}

// ListIDCount is one entry in SurveyV1.ListIDs: a List-Id value and the
// number of messages that carried it.
type ListIDCount struct {
	ListID string `json:"list_id"`
	Count  int    `json:"count"`
}

// SenderCount is one entry in SurveyV1.Senders: a from-address and the
// number of messages (without a List-Id) that came from it.
type SenderCount struct {
	Address string `json:"address"`
	Count   int    `json:"count"`
}

// Compile-time assertion that SurveyV1 satisfies the importer.SurveyFile
// contract so RunOneShot can call IsStale on it without a concrete-type
// import.
var _ importer.SurveyFile = (*SurveyV1)(nil)

// SurveyPath returns the conventional survey sidecar path for a given
// mbox source, `<sourcePath>.survey.v1.json`.
func SurveyPath(sourcePath string) string {
	return sourcePath + surveySuffix
}

// Aggregate consumes every message from scanner and returns a filled
// SurveyV1. topN caps ListIDs and Senders (sorted by count descending);
// pass 0 or a negative value to disable truncation.
//
// Aggregate stamps SurveyStartedAt at the first call and SurveyCompletedAt
// at return; it leaves the source-file fields (SourcePath, SourceSize,
// SourceMtime) unset so the caller can stat the source at the right
// instant and populate them. The staleness check (IsStale) only works
// once those fields are populated.
//
// Aggregate returns the scanner's fatal error, if any, otherwise nil.
func Aggregate(scanner *Scanner, topN int) (*SurveyV1, error) {
	survey := &SurveyV1{
		SchemaVersion:   SurveySchemaVersion,
		SurveyStartedAt: time.Now().UTC(),
		Labels:          make(map[string]int),
		DateHistogram:   make(map[string]int),
		SizeHistogram:   make(map[string]int),
	}

	listCounts := make(map[string]int)
	senderCounts := make(map[string]int)

	for scanner.Scan() {
		m := scanner.Message()

		survey.TotalMessages++
		survey.TotalBytes += int64(m.Size)
		if m.HeaderParseError != nil {
			survey.MalformedMessages++
		}

		for _, label := range m.GmailLabels {
			survey.Labels[label]++
		}

		if m.ListID != "" {
			listCounts[m.ListID]++
		} else if m.From != "" {
			// Senders list is "top N for messages without List-Id" per
			// spec §3.3.1; messages missing both a From and a List-Id
			// simply don't contribute to either top-N.
			senderCounts[m.From]++
		}

		survey.DateHistogram[dateBucket(m.Date)]++
		survey.SizeHistogram[sizeBucket(m.Size)]++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mbox survey: scan: %w", err)
	}

	survey.ListIDs = topListIDs(listCounts, topN)
	survey.Senders = topSenders(senderCounts, topN)

	survey.SurveyCompletedAt = time.Now().UTC()
	return survey, nil
}

// dateBucket maps a message Date into the fixed bucket label set. Zero
// values (missing or unparseable Date headers) land in BucketUnknown.
func dateBucket(t time.Time) string {
	if t.IsZero() {
		return BucketUnknown
	}
	y := t.Year()
	switch {
	case y < 2005:
		return BucketPre2005
	case y < 2010:
		return Bucket2005to2010
	case y < 2015:
		return Bucket2010to2015
	case y < 2020:
		return Bucket2015to2020
	case y < 2025:
		return Bucket2020to2025
	default:
		return BucketPost2025
	}
}

// sizeBucket maps a message size in bytes into the fixed bucket label set.
func sizeBucket(size int) string {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case size < 10*kb:
		return SizeLt10KB
	case size < 1*mb:
		return Size10KBto1MB
	case size < 10*mb:
		return Size1MBto10MB
	default:
		return SizeGt10MB
	}
}

// topListIDs converts a List-Id counter map into a slice sorted by count
// descending (ties broken by list_id ascending for deterministic output)
// and truncated to topN. A topN of 0 or negative disables truncation.
func topListIDs(counts map[string]int, topN int) []ListIDCount {
	out := make([]ListIDCount, 0, len(counts))
	for id, c := range counts {
		out = append(out, ListIDCount{ListID: id, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ListID < out[j].ListID
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

// topSenders is the Senders analog of topListIDs.
func topSenders(counts map[string]int, topN int) []SenderCount {
	out := make([]SenderCount, 0, len(counts))
	for addr, c := range counts {
		out = append(out, SenderCount{Address: addr, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Address < out[j].Address
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

// Write serializes the survey to path atomically (temp + rename). Mirrors
// the manifest.go strategy, including compact (non-indented) output --
// surveys over large mboxes can accumulate hundreds of top-N entries
// plus dense label histograms, and jq can pretty-print on demand.
func (s *SurveyV1) Write(path string) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("mbox survey: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf("%s.tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("mbox survey: create temp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("mbox survey: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("mbox survey: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("mbox survey: close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("mbox survey: rename: %w", err)
	}
	return nil
}

// LoadSurvey reads and parses the survey at path.
//
// If the file does not exist, the returned error satisfies errors.Is(err,
// fs.ErrNotExist) so callers can branch on "no survey yet" without caring
// whether the underlying error is *PathError or syscall.ENOENT. A missing
// survey is the normal "first run" state; callers should then run
// Aggregate and Write a fresh one.
func LoadSurvey(path string) (*SurveyV1, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s SurveyV1
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("mbox survey: parse %s: %w", path, err)
	}
	return &s, nil
}

// IsStale reports whether the file at sourcePath differs from what this
// survey was generated against. Staleness is a size-OR-mtime mismatch
// per spec §3.3.2. A stat error (missing file, permission denied, etc.)
// is returned verbatim so callers can distinguish "file gone" from
// "legitimately stale".
//
// mtime comparison uses time.Time.Equal so that the time.Time round-tripped
// through JSON (which normalizes to UTC at nanosecond precision) compares
// equal to a freshly-stat'd time with the same instant but a different
// Location.
func (s *SurveyV1) IsStale(sourcePath string) (bool, error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return false, err
	}
	if info.Size() != s.SourceSize {
		return true, nil
	}
	if !info.ModTime().Equal(s.SourceMtime) {
		return true, nil
	}
	return false, nil
}
