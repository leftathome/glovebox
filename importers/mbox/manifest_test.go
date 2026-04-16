package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/importer"
)

// newFullManifest returns a manifest populated with non-zero values in every
// field, used by round-trip tests to verify that nothing is silently dropped.
func newFullManifest(t *testing.T) *ImportManifestV1 {
	t.Helper()
	start := time.Date(2026, 4, 11, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 11, 11, 30, 0, 0, time.UTC)
	mtime := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)

	return &ImportManifestV1{
		SchemaVersion:      SchemaVersion,
		Kind:               Kind,
		SourcePath:         "/data/takeout.mbox",
		SourceSize:         12_583_279_104,
		SourceMtime:        mtime,
		SourceName:         "takeout-2026-04-11",
		StatusValue:        validatedStatus(importer.StatusComplete),
		TimestampStart:     start,
		TimestampEnd:       &end,
		SurveyRef:          "takeout.mbox.survey.v1.json",
		FilterRef:          "takeout.mbox.filter.json",
		FilterRulesApplied: json.RawMessage(`[{"match":{"label":"Spam"},"action":"exclude"}]`),
		Counts: Counts{
			MessagesSeen:         124800,
			MessagesIngested:     58312,
			MessagesFiltered:     64203,
			MessagesErrored:      14,
			MessagesDedupSkipped: 2271,
			BytesProcessed:       6_291_456_000,
		},
		FilterHitCounts: map[string]int{
			"rule_0_label_Spam": 48231,
			"default_excluded":  0,
		},
		DestinationRuleHitCounts: map[string]int{
			"rule_0_folder_INBOX": 45231,
		},
		MessageIDsIngested: []string{"<a@example.com>", "<b@example.com>"},
		Errors: []ErrorEntry{
			{ByteOffset: 123, MessageID: "", Reason: "malformed headers"},
			{ByteOffset: 456, MessageID: "<c@example.com>", Reason: "ingest 500"},
		},
		TruncatedErrorCount: 0,
		ResumeState: ResumeState{
			ByteOffset:            6_291_456_000,
			LastIngestedMessageID: "<xyz@example.com>",
		},
	}
}

func TestManifestPath(t *testing.T) {
	got := ManifestPath("/foo/bar.mbox")
	want := "/foo/bar.mbox.import-manifest.v1.json"
	if got != want {
		t.Fatalf("ManifestPath: got %q want %q", got, want)
	}
}

func TestRoundTripPreservesAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mbox.import-manifest.v1.json")

	orig := newFullManifest(t)

	if err := orig.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	// time.Time fields may normalize locations; compare via Equal before the
	// DeepEqual pass, then zero them out for the structural comparison.
	if !orig.SourceMtime.Equal(loaded.SourceMtime) {
		t.Errorf("SourceMtime mismatch: orig %v loaded %v", orig.SourceMtime, loaded.SourceMtime)
	}
	if !orig.TimestampStart.Equal(loaded.TimestampStart) {
		t.Errorf("TimestampStart mismatch")
	}
	if orig.TimestampEnd == nil || loaded.TimestampEnd == nil {
		t.Fatalf("TimestampEnd pointer lost: orig=%v loaded=%v", orig.TimestampEnd, loaded.TimestampEnd)
	}
	if !orig.TimestampEnd.Equal(*loaded.TimestampEnd) {
		t.Errorf("TimestampEnd mismatch")
	}

	// Normalize times for reflect comparison (Equal treats UTC == equivalent
	// non-UTC but DeepEqual does not).
	normalize := func(m *ImportManifestV1) {
		m.SourceMtime = m.SourceMtime.UTC()
		m.TimestampStart = m.TimestampStart.UTC()
		if m.TimestampEnd != nil {
			u := m.TimestampEnd.UTC()
			m.TimestampEnd = &u
		}
	}
	normalize(orig)
	normalize(loaded)

	// FilterRulesApplied is json.RawMessage which may re-serialize with
	// different whitespace; canonicalize both sides before comparing.
	canon := func(raw json.RawMessage) string {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("canonicalize FilterRulesApplied: %v", err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("re-marshal FilterRulesApplied: %v", err)
		}
		return string(b)
	}
	if canon(orig.FilterRulesApplied) != canon(loaded.FilterRulesApplied) {
		t.Errorf("FilterRulesApplied mismatch: orig %s loaded %s",
			orig.FilterRulesApplied, loaded.FilterRulesApplied)
	}
	orig.FilterRulesApplied = nil
	loaded.FilterRulesApplied = nil

	if !reflect.DeepEqual(orig, loaded) {
		t.Errorf("round-trip mismatch:\norig   = %#v\nloaded = %#v", orig, loaded)
	}
}

func TestRoundTripNilTimestampEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")

	m := &ImportManifestV1{
		SchemaVersion:            SchemaVersion,
		Kind:                     Kind,
		StatusValue:              validatedStatus(importer.StatusInProgress),
		TimestampStart:           time.Now().UTC(),
		TimestampEnd:             nil,
		FilterHitCounts:          map[string]int{},
		DestinationRuleHitCounts: map[string]int{},
		MessageIDsIngested:       []string{},
		Errors:                   []ErrorEntry{},
	}
	if err := m.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if loaded.TimestampEnd != nil {
		t.Errorf("expected nil TimestampEnd, got %v", *loaded.TimestampEnd)
	}

	// Verify the JSON encodes "timestamp_end": null, not missing.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `"timestamp_end":null`) {
		t.Errorf("expected timestamp_end rendered as null, got:\n%s", raw)
	}
}

func TestRoundTripEmptyCollections(t *testing.T) {
	// The collection fields (MessageIDsIngested, Errors, FilterHitCounts,
	// DestinationRuleHitCounts, FilterRulesApplied) all carry omitempty,
	// so empty/nil values drop from the serialized form entirely and
	// round-trip as nil on load. This test documents that behavior -- it
	// matters for the on-disk size of in-flight manifests.
	dir := t.TempDir()

	emptyPath := filepath.Join(dir, "empty.json")
	empty := &ImportManifestV1{
		SchemaVersion:      SchemaVersion,
		Kind:               Kind,
		StatusValue:        validatedStatus(importer.StatusInProgress),
		MessageIDsIngested: []string{},
		Errors:             []ErrorEntry{},
	}
	if err := empty.Write(emptyPath); err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	data, _ := os.ReadFile(emptyPath)
	// With omitempty, empty slices/maps are dropped; we accept either
	// "field missing entirely" OR "field present as []/null" to keep the
	// test robust across future JSON library behavior, but the current
	// behavior is "absent".
	if strings.Contains(string(data), `"message_ids_ingested":[`) && !strings.Contains(string(data), `"message_ids_ingested":[]`) {
		t.Errorf("message_ids_ingested appears with non-empty content: %s", data)
	}

	loaded, err := LoadManifest(emptyPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	// Either nil or empty non-nil is acceptable after the omitempty round-trip.
	if len(loaded.MessageIDsIngested) != 0 {
		t.Errorf("expected empty MessageIDsIngested after round-trip, got %#v", loaded.MessageIDsIngested)
	}
	if len(loaded.Errors) != 0 {
		t.Errorf("expected empty Errors after round-trip, got %#v", loaded.Errors)
	}

	// Nil slices behave the same as empty slices under omitempty.
	nilPath := filepath.Join(dir, "nil.json")
	nilM := &ImportManifestV1{
		SchemaVersion: SchemaVersion,
		Kind:          Kind,
		StatusValue:   validatedStatus(importer.StatusInProgress),
	}
	if err := nilM.Write(nilPath); err != nil {
		t.Fatalf("Write nil: %v", err)
	}
	nilLoaded, err := LoadManifest(nilPath)
	if err != nil {
		t.Fatalf("LoadManifest nil: %v", err)
	}
	if nilLoaded.MessageIDsIngested != nil {
		t.Errorf("expected nil slice to round-trip as nil, got %#v", nilLoaded.MessageIDsIngested)
	}
	if nilLoaded.Errors != nil {
		t.Errorf("expected nil errors to round-trip as nil, got %#v", nilLoaded.Errors)
	}
}

func TestStatusEnumMarshalUnmarshal(t *testing.T) {
	cases := []importer.ManifestStatus{
		importer.StatusInProgress,
		importer.StatusComplete,
		importer.StatusInterrupted,
		importer.StatusFailed,
	}
	for _, s := range cases {
		s := s
		t.Run(string(s), func(t *testing.T) {
			m := &ImportManifestV1{
				SchemaVersion: SchemaVersion,
				Kind:          Kind,
				StatusValue:   validatedStatus(s),
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(data), `"status":"`+string(s)+`"`) {
				t.Errorf("expected status %q in output, got %s", s, data)
			}
			var back ImportManifestV1
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if importer.ManifestStatus(back.StatusValue) != s {
				t.Errorf("status mismatch: got %q want %q", back.StatusValue, s)
			}
		})
	}
}

func TestStatusUnmarshalRejectsUnknown(t *testing.T) {
	raw := []byte(`{"status":"not-a-real-status"}`)
	var m ImportManifestV1
	err := json.Unmarshal(raw, &m)
	if err == nil {
		t.Fatalf("expected error for unknown status, got nil")
	}
	if !strings.Contains(err.Error(), "unknown status") {
		t.Errorf("expected 'unknown status' in error, got: %v", err)
	}

	// Also verify the higher-level LoadManifest path propagates the error.
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadManifest(path); err == nil {
		t.Fatalf("LoadManifest: expected error for unknown status, got nil")
	}
}

func TestAtomicWriteLeavesNoPartialOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}

	// Path strategy:
	//   - As a non-root user: use a directory chmoded 0500 so OpenFile of the
	//     tmp file fails. This is the exact spec-mentioned scenario.
	//   - As root (CI containers, dev loops): chmod bits are ignored, so we
	//     instead point Write at a path whose parent directory does not
	//     exist, which also forces OpenFile to fail. Both branches verify the
	//     same invariant: no partial file at the target, no `.tmp-*` sibling.
	parent := t.TempDir()
	var target, watchDir string

	if os.Geteuid() == 0 {
		// Parent doesn't exist => OpenFile fails at tmp creation.
		target = filepath.Join(parent, "does-not-exist", "m.json")
		watchDir = parent
	} else {
		roDir := filepath.Join(parent, "ro")
		if err := os.Mkdir(roDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		target = filepath.Join(roDir, "m.json")
		if err := os.WriteFile(target, []byte(`{"pre":"existing"}`), 0o644); err != nil {
			t.Fatalf("seed target: %v", err)
		}
		if err := os.Chmod(roDir, 0o500); err != nil {
			t.Fatalf("chmod ro: %v", err)
		}
		// Restore perms so t.TempDir cleanup and the post-write scan work.
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
		watchDir = roDir
	}

	m := newFullManifest(t)
	if err := m.Write(target); err == nil {
		t.Fatalf("expected Write to fail, got nil")
	}

	// Permit inspection for the non-root branch.
	if os.Geteuid() != 0 {
		if err := os.Chmod(watchDir, 0o700); err != nil {
			t.Fatalf("chmod restore for inspection: %v", err)
		}
	}

	entries, err := os.ReadDir(watchDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", watchDir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("partial temp file lingered: %s", e.Name())
		}
	}

	// Non-root branch additionally verifies the pre-existing target is
	// untouched by the failed atomic write.
	if os.Geteuid() != 0 {
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if string(got) != `{"pre":"existing"}` {
			t.Errorf("target was mutated despite failed write: %s", got)
		}
	}
}

func TestErrorCap(t *testing.T) {
	m := &ImportManifestV1{}
	for i := 0; i < 1500; i++ {
		m.AddError(ErrorEntry{ByteOffset: int64(i), Reason: "test"})
	}
	if len(m.Errors) != DefaultErrorCap {
		t.Errorf("Errors len: got %d want %d", len(m.Errors), DefaultErrorCap)
	}
	if m.TruncatedErrorCount != 500 {
		t.Errorf("TruncatedErrorCount: got %d want 500", m.TruncatedErrorCount)
	}
	// The first DefaultErrorCap entries should be the retained ones (FIFO
	// append, cap-on-overflow); the 1001st AddError is dropped.
	if m.Errors[0].ByteOffset != 0 {
		t.Errorf("first retained entry: got offset %d want 0", m.Errors[0].ByteOffset)
	}
	if m.Errors[DefaultErrorCap-1].ByteOffset != int64(DefaultErrorCap-1) {
		t.Errorf("last retained entry: got offset %d want %d",
			m.Errors[DefaultErrorCap-1].ByteOffset, DefaultErrorCap-1)
	}
}

func TestIsStatusTerminal(t *testing.T) {
	cases := map[importer.ManifestStatus]bool{
		importer.StatusInProgress:        false,
		importer.StatusComplete:          true,
		importer.StatusInterrupted:       true,
		importer.StatusFailed:            true,
		importer.ManifestStatus("bogus"): false, // defensively false for unset/unknown in-memory value
	}
	for s, want := range cases {
		m := &ImportManifestV1{StatusValue: validatedStatus(s)}
		if got := m.IsStatusTerminal(); got != want {
			t.Errorf("IsStatusTerminal(%q): got %v want %v", s, got, want)
		}
	}
}

func TestLoadManifestNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.json")
	_, err := LoadManifest(path)
	if err == nil {
		t.Fatalf("expected error loading missing manifest, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected errors.Is(err, fs.ErrNotExist) to be true, got: %v", err)
	}
}
