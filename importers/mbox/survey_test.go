package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// aggregateFromFile opens an mbox fixture, streams it through Aggregate,
// and returns the survey. The returned survey has SourcePath / SourceSize
// / SourceMtime unset because Aggregate only fills the streaming-derived
// fields; staleness-check tests populate them explicitly from os.Stat.
func aggregateFromFile(t *testing.T, path string, topN int) *SurveyV1 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	s := NewScanner(f)
	survey, err := Aggregate(s, topN)
	if err != nil {
		t.Fatalf("Aggregate %s: %v", path, err)
	}
	return survey
}

func TestSurveyPath(t *testing.T) {
	got := SurveyPath("/foo/bar.mbox")
	want := "/foo/bar.mbox.survey.v1.json"
	if got != want {
		t.Fatalf("SurveyPath: got %q want %q", got, want)
	}
}

func TestAggregate_SmallMbox_Counts(t *testing.T) {
	// small.mbox contains 7 well-formed messages. See testdata/small.mbox;
	// all messages parse cleanly so MalformedMessages must be zero.
	survey := aggregateFromFile(t, filepath.Join("testdata", "small.mbox"), 100)

	if survey.TotalMessages != 7 {
		t.Errorf("TotalMessages = %d, want 7", survey.TotalMessages)
	}
	if survey.MalformedMessages != 0 {
		t.Errorf("MalformedMessages = %d, want 0", survey.MalformedMessages)
	}
	if survey.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d, want > 0", survey.TotalBytes)
	}
	if survey.SchemaVersion != SurveySchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", survey.SchemaVersion, SurveySchemaVersion)
	}
	if survey.SurveyStartedAt.IsZero() {
		t.Errorf("SurveyStartedAt not set")
	}
	if survey.SurveyCompletedAt.IsZero() {
		t.Errorf("SurveyCompletedAt not set")
	}
	if survey.SurveyCompletedAt.Before(survey.SurveyStartedAt) {
		t.Errorf("SurveyCompletedAt (%v) before SurveyStartedAt (%v)",
			survey.SurveyCompletedAt, survey.SurveyStartedAt)
	}
}

func TestAggregate_SmallMbox_Labels(t *testing.T) {
	// Exactly one message in small.mbox carries X-Gmail-Labels with three
	// comma-separated values: INBOX, IMPORTANT, Category/Updates.
	survey := aggregateFromFile(t, filepath.Join("testdata", "small.mbox"), 100)

	want := map[string]int{
		"INBOX":            1,
		"IMPORTANT":        1,
		"Category/Updates": 1,
	}
	if !reflect.DeepEqual(survey.Labels, want) {
		t.Errorf("Labels = %v, want %v", survey.Labels, want)
	}
}

func TestAggregate_SmallMbox_ListIDs(t *testing.T) {
	// Two messages in small.mbox have list-ish headers: one List-Id
	// "dev.example.com" and one List-Post "mailto:list@example.com".
	survey := aggregateFromFile(t, filepath.Join("testdata", "small.mbox"), 100)

	ids := map[string]int{}
	for _, e := range survey.ListIDs {
		ids[e.ListID] = e.Count
	}
	if ids["dev.example.com"] != 1 {
		t.Errorf("expected dev.example.com = 1, got %v", ids)
	}
	if ids["mailto:list@example.com"] != 1 {
		t.Errorf("expected mailto:list@example.com = 1, got %v", ids)
	}
}

func TestAggregate_SmallMbox_SendersExcludesListMessages(t *testing.T) {
	// Senders only includes messages with no List-Id; small.mbox has
	// 7 messages total, 2 of which carry list headers, so the Senders
	// slice must have 5 entries.
	survey := aggregateFromFile(t, filepath.Join("testdata", "small.mbox"), 100)

	if got := len(survey.Senders); got != 5 {
		t.Errorf("Senders length = %d, want 5 (7 total - 2 list messages)", got)
	}
	for _, s := range survey.Senders {
		switch s.Address {
		case "dev-poster@example.com", "announcer@example.com":
			t.Errorf("Senders should exclude list-message sender %q", s.Address)
		}
	}
}

func TestAggregate_SmallMbox_DateAndSizeHistograms(t *testing.T) {
	// All 7 small.mbox messages are dated 2026-04-11, so they must all
	// land in the post-2025 bucket; every message body is tiny, so they
	// all fall in lt_10kb.
	survey := aggregateFromFile(t, filepath.Join("testdata", "small.mbox"), 100)

	if got := survey.DateHistogram[BucketPost2025]; got != 7 {
		t.Errorf("DateHistogram[%s] = %d, want 7 (full map: %v)",
			BucketPost2025, got, survey.DateHistogram)
	}
	if got := survey.SizeHistogram[SizeLt10KB]; got != 7 {
		t.Errorf("SizeHistogram[%s] = %d, want 7 (full map: %v)",
			SizeLt10KB, got, survey.SizeHistogram)
	}
}

func TestAggregate_MalformedMbox_CountsParseError(t *testing.T) {
	// malformed.mbox has 3 messages, exactly 1 of which trips
	// net/mail.ReadMessage (the middle one with colon-less headers).
	survey := aggregateFromFile(t, filepath.Join("testdata", "malformed.mbox"), 100)

	if survey.TotalMessages != 3 {
		t.Errorf("TotalMessages = %d, want 3", survey.TotalMessages)
	}
	if survey.MalformedMessages != 1 {
		t.Errorf("MalformedMessages = %d, want 1", survey.MalformedMessages)
	}
}

func TestAggregate_EmptyMbox(t *testing.T) {
	// empty.mbox is a zero-byte file; scanning must produce no messages
	// and a well-formed empty survey, not an error.
	survey := aggregateFromFile(t, filepath.Join("testdata", "empty.mbox"), 100)

	if survey.TotalMessages != 0 {
		t.Errorf("TotalMessages = %d, want 0", survey.TotalMessages)
	}
	if survey.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0", survey.TotalBytes)
	}
	if len(survey.ListIDs) != 0 {
		t.Errorf("ListIDs = %v, want empty", survey.ListIDs)
	}
	if len(survey.Senders) != 0 {
		t.Errorf("Senders = %v, want empty", survey.Senders)
	}
}

// buildMbox concatenates messages into a synthetic mbox stream suitable
// for feeding through NewScanner. Each entry is a full RFC 5322 message
// minus the From_ separator; buildMbox adds a separator and a trailing
// blank line between messages.
func buildMbox(messages []string) []byte {
	var buf bytes.Buffer
	for _, m := range messages {
		buf.WriteString("From MAILER-DAEMON Mon Apr 11 10:00:00 2026\n")
		buf.WriteString(m)
		if !strings.HasSuffix(m, "\n") {
			buf.WriteString("\n")
		}
		buf.WriteString("\n")
	}
	return buf.Bytes()
}

// makeMessage returns a message body with the given headers and a 1-line
// body. Used by the synthetic-mbox tests below.
func makeMessage(headers map[string]string, body string) string {
	var sb strings.Builder
	for k, v := range headers {
		fmt.Fprintf(&sb, "%s: %s\n", k, v)
	}
	sb.WriteString("\n")
	sb.WriteString(body)
	sb.WriteString("\n")
	return sb.String()
}

func TestAggregate_ListIDs_SortedByCountDescAndTopN(t *testing.T) {
	// Build five List-Ids with distinct counts: alpha=5, beta=3, gamma=2,
	// delta=1, epsilon=1. Top 3 must be alpha, beta, gamma (in that order).
	// The epsilon tie with delta is broken by list_id ascending so that
	// either truncation is deterministic, but it's not asserted here since
	// they fall outside the top 3.
	var messages []string
	counts := []struct {
		id    string
		count int
	}{
		{"alpha.example.com", 5},
		{"beta.example.com", 3},
		{"gamma.example.com", 2},
		{"delta.example.com", 1},
		{"epsilon.example.com", 1},
	}
	i := 0
	for _, c := range counts {
		for k := 0; k < c.count; k++ {
			messages = append(messages, makeMessage(map[string]string{
				"Message-ID": fmt.Sprintf("<msg-%d@example.com>", i),
				"From":       "s@example.com",
				"Date":       "Mon, 11 Apr 2026 10:00:00 +0000",
				"Subject":    "test",
				"List-Id":    "<" + c.id + ">",
			}, "body"))
			i++
		}
	}

	data := buildMbox(messages)
	s := NewScanner(bytes.NewReader(data))
	survey, err := Aggregate(s, 3)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	if len(survey.ListIDs) != 3 {
		t.Fatalf("ListIDs length = %d, want 3 (truncated); full list %v",
			len(survey.ListIDs), survey.ListIDs)
	}
	wantOrder := []ListIDCount{
		{ListID: "alpha.example.com", Count: 5},
		{ListID: "beta.example.com", Count: 3},
		{ListID: "gamma.example.com", Count: 2},
	}
	if !reflect.DeepEqual(survey.ListIDs, wantOrder) {
		t.Errorf("ListIDs order = %v, want %v", survey.ListIDs, wantOrder)
	}

	// Senders must be empty: every message carries a List-Id.
	if len(survey.Senders) != 0 {
		t.Errorf("Senders = %v, want empty (all messages had List-Id)", survey.Senders)
	}
}

func TestAggregate_Senders_ExcludesListMessages(t *testing.T) {
	// Mix: two plain messages from alice (no list), one list message from
	// alice (must not count), one plain from bob. Want Senders = [alice:2, bob:1].
	messages := []string{
		makeMessage(map[string]string{
			"Message-ID": "<1@x>", "From": "alice@example.com",
			"Date": "Mon, 11 Apr 2026 10:00:00 +0000", "Subject": "t",
		}, "b"),
		makeMessage(map[string]string{
			"Message-ID": "<2@x>", "From": "alice@example.com",
			"Date": "Mon, 11 Apr 2026 10:00:00 +0000", "Subject": "t",
		}, "b"),
		makeMessage(map[string]string{
			"Message-ID": "<3@x>", "From": "alice@example.com",
			"Date": "Mon, 11 Apr 2026 10:00:00 +0000", "Subject": "t",
			"List-Id": "<some.list.example.com>",
		}, "b"),
		makeMessage(map[string]string{
			"Message-ID": "<4@x>", "From": "bob@example.com",
			"Date": "Mon, 11 Apr 2026 10:00:00 +0000", "Subject": "t",
		}, "b"),
	}
	data := buildMbox(messages)
	s := NewScanner(bytes.NewReader(data))
	survey, err := Aggregate(s, 100)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	want := []SenderCount{
		{Address: "alice@example.com", Count: 2},
		{Address: "bob@example.com", Count: 1},
	}
	if !reflect.DeepEqual(survey.Senders, want) {
		t.Errorf("Senders = %v, want %v", survey.Senders, want)
	}
}

func TestDateBucket(t *testing.T) {
	cases := []struct {
		year int
		want string
	}{
		{1999, BucketPre2005},
		{2004, BucketPre2005},
		{2005, Bucket2005to2010},
		{2009, Bucket2005to2010},
		{2010, Bucket2010to2015},
		{2014, Bucket2010to2015},
		{2015, Bucket2015to2020},
		{2019, Bucket2015to2020},
		{2020, Bucket2020to2025},
		{2024, Bucket2020to2025},
		{2025, BucketPost2025},
		{2030, BucketPost2025},
	}
	for _, c := range cases {
		got := dateBucket(time.Date(c.year, 6, 1, 0, 0, 0, 0, time.UTC))
		if got != c.want {
			t.Errorf("dateBucket(%d) = %q, want %q", c.year, got, c.want)
		}
	}
	if got := dateBucket(time.Time{}); got != BucketUnknown {
		t.Errorf("dateBucket(zero) = %q, want %q", got, BucketUnknown)
	}
}

func TestSizeBucket(t *testing.T) {
	cases := []struct {
		size int
		want string
	}{
		{0, SizeLt10KB},
		{9 * 1024, SizeLt10KB},
		{10 * 1024, Size10KBto1MB},
		{1024*1024 - 1, Size10KBto1MB},
		{1024 * 1024, Size1MBto10MB},
		{10*1024*1024 - 1, Size1MBto10MB},
		{10 * 1024 * 1024, SizeGt10MB},
		{100 * 1024 * 1024, SizeGt10MB},
	}
	for _, c := range cases {
		got := sizeBucket(c.size)
		if got != c.want {
			t.Errorf("sizeBucket(%d) = %q, want %q", c.size, got, c.want)
		}
	}
}

func TestSurveyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mbox.survey.v1.json")

	mtime := time.Date(2026, 4, 11, 12, 34, 56, 0, time.UTC)
	orig := &SurveyV1{
		SchemaVersion:     SurveySchemaVersion,
		SourcePath:        "/data/takeout.mbox",
		SourceSize:        1_234_567,
		SourceMtime:       mtime,
		SurveyStartedAt:   time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC),
		SurveyCompletedAt: time.Date(2026, 4, 12, 9, 5, 0, 0, time.UTC),
		TotalMessages:     42,
		TotalBytes:        1_234_567,
		MalformedMessages: 1,
		Labels:            map[string]int{"INBOX": 30, "Sent": 12},
		ListIDs: []ListIDCount{
			{ListID: "alpha.example.com", Count: 10},
			{ListID: "beta.example.com", Count: 5},
		},
		Senders: []SenderCount{
			{Address: "alice@example.com", Count: 20},
		},
		DateHistogram: map[string]int{BucketPost2025: 42},
		SizeHistogram: map[string]int{SizeLt10KB: 42},
	}

	if err := orig.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := LoadSurvey(path)
	if err != nil {
		t.Fatalf("LoadSurvey: %v", err)
	}

	if !orig.SourceMtime.Equal(loaded.SourceMtime) {
		t.Errorf("SourceMtime mismatch: orig %v loaded %v", orig.SourceMtime, loaded.SourceMtime)
	}
	if !orig.SurveyStartedAt.Equal(loaded.SurveyStartedAt) {
		t.Errorf("SurveyStartedAt mismatch")
	}
	if !orig.SurveyCompletedAt.Equal(loaded.SurveyCompletedAt) {
		t.Errorf("SurveyCompletedAt mismatch")
	}

	// Normalize times so DeepEqual works even if JSON unmarshal picked a
	// different Location.
	norm := func(s *SurveyV1) {
		s.SourceMtime = s.SourceMtime.UTC()
		s.SurveyStartedAt = s.SurveyStartedAt.UTC()
		s.SurveyCompletedAt = s.SurveyCompletedAt.UTC()
	}
	norm(orig)
	norm(loaded)

	if !reflect.DeepEqual(orig, loaded) {
		t.Errorf("round-trip mismatch:\norig   = %#v\nloaded = %#v", orig, loaded)
	}
}

func TestWriteIsCompactNotIndented(t *testing.T) {
	// manifest.go uses compact JSON for size reasons; survey.go must match.
	// This test locks in that choice.
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	s := &SurveyV1{
		SchemaVersion: SurveySchemaVersion,
		Labels:        map[string]int{"A": 1, "B": 2},
		DateHistogram: map[string]int{},
		SizeHistogram: map[string]int{},
	}
	if err := s.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(raw, []byte("\n  ")) {
		t.Errorf("expected compact JSON (no indent), got:\n%s", raw)
	}
}

func TestLoadSurveyNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.json")
	_, err := LoadSurvey(path)
	if err == nil {
		t.Fatalf("expected error for missing survey, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected errors.Is(err, fs.ErrNotExist) to be true, got: %v", err)
	}
}

// writeFixtureMbox writes a small payload to path so IsStale tests have a
// real file to stat.
func writeFixtureMbox(t *testing.T, path string, payload []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes fixture: %v", err)
	}
}

func TestIsStale_Fresh(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mbox")
	payload := []byte("From MAILER-DAEMON Mon Apr 11 10:00:00 2026\nSubject: x\n\nhi\n")
	mtime := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	writeFixtureMbox(t, src, payload, mtime)

	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	s := &SurveyV1{
		SourcePath:  src,
		SourceSize:  info.Size(),
		SourceMtime: info.ModTime(),
	}
	stale, err := s.IsStale(src)
	if err != nil {
		t.Fatalf("IsStale: %v", err)
	}
	if stale {
		t.Errorf("expected not stale, got stale")
	}
}

func TestIsStale_SizeChanged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mbox")
	payload := []byte("hello world")
	mtime := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	writeFixtureMbox(t, src, payload, mtime)

	// Record a smaller size than the file actually has.
	s := &SurveyV1{
		SourcePath:  src,
		SourceSize:  1,
		SourceMtime: mtime,
	}
	stale, err := s.IsStale(src)
	if err != nil {
		t.Fatalf("IsStale: %v", err)
	}
	if !stale {
		t.Errorf("expected stale due to size mismatch, got fresh")
	}
}

func TestIsStale_MtimeChanged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mbox")
	payload := []byte("hello world")
	mtime := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	writeFixtureMbox(t, src, payload, mtime)

	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Record the correct size but an older mtime.
	s := &SurveyV1{
		SourcePath:  src,
		SourceSize:  info.Size(),
		SourceMtime: mtime.Add(-24 * time.Hour),
	}
	stale, err := s.IsStale(src)
	if err != nil {
		t.Fatalf("IsStale: %v", err)
	}
	if !stale {
		t.Errorf("expected stale due to mtime mismatch, got fresh")
	}
}

func TestIsStale_MissingSource(t *testing.T) {
	dir := t.TempDir()
	// Never create the file.
	missing := filepath.Join(dir, "does-not-exist.mbox")
	s := &SurveyV1{SourcePath: missing, SourceSize: 1, SourceMtime: time.Now()}
	_, err := s.IsStale(missing)
	if err == nil {
		t.Fatalf("expected error from IsStale on missing source, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected errors.Is(err, fs.ErrNotExist), got: %v", err)
	}
}
