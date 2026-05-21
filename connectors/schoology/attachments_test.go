package schoology

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// stagingDirCount returns the number of committed (non-tmp, non-hidden)
// item directories in stagingDir.
func stagingDirCount(t *testing.T, stagingDir string) int {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		count++
	}
	return count
}

// newTestWriter builds a StagingWriter pointed at t.TempDir().
func newTestWriter(t *testing.T) (*connector.StagingWriter, string) {
	t.Helper()
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "schoology")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	return w, stagingDir
}

// newTestCheckpoint returns a real on-disk checkpoint rooted at t.TempDir().
func newTestCheckpoint(t *testing.T) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return cp
}

// matchAllMatcher returns a RuleMatcher with a single wildcard rule
// routing to "messaging" — minimal config so staging validation passes.
func matchAllMatcher() *connector.RuleMatcher {
	return connector.NewRuleMatcher([]connector.Rule{
		{Match: "*", Destination: "messaging"},
	})
}

// sampleAttachment returns a minimally-populated Attachment with the
// parent metadata filled in so staging-validation requirements are met
// (Sender, Subject, Timestamp must all be non-empty/non-zero).
func sampleAttachment(id int64, filename string) Attachment {
	return Attachment{
		ID:              id,
		URL:             "https://schoology.example/attachment/" + filename,
		Filename:        filename,
		MimeType:        "application/pdf",
		ParentSender:    "teacher@example.com",
		ParentSubject:   "Weekly newsletter",
		ParentTimestamp: time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC),
	}
}

// fixedBodyDownloader returns a DownloadFunc that yields the same body
// for every call. Useful for happy-path and dedup tests.
func fixedBodyDownloader(body []byte) func(ctx context.Context, url string) (io.ReadCloser, error) {
	return func(ctx context.Context, url string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

// TestProcessAttachments_HappyPath stages two attachments, both end up
// in the staging dir, and the checkpoint advances to the highest ID.
func TestProcessAttachments_HappyPath(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := matchAllMatcher()
	cp := newTestCheckpoint(t)
	client := &fakeClient{
		DownloadFunc: fixedBodyDownloader([]byte("hello world")),
	}

	atts := []Attachment{
		sampleAttachment(101, "syllabus.pdf"),
		sampleAttachment(102, "rubric.pdf"),
	}

	staged, skipped := ProcessAttachments(
		context.Background(),
		client,
		writer,
		matcher,
		cp,
		nil, // tel optional
		atts,
		/*parentID*/ 9001,
		/*parentType*/ "feed_post",
		/*matchKey*/ "feed",
		/*surface*/ "feed_attachments",
		/*scope*/ "kid1",
		/*maxSizeMB*/ 8,
	)

	if staged != 2 {
		t.Errorf("ProcessAttachments staged %d, want 2", staged)
	}
	if len(skipped) != 0 {
		t.Errorf("happy path should have no skipped attachments, got %v", skipped)
	}
	if got := stagingDirCount(t, stagingDir); got != 2 {
		t.Errorf("staging dir has %d items, want 2", got)
	}

	last, err := LastSeenID(cp, "feed_attachments", "kid1")
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if last != 102 {
		t.Errorf("checkpoint = %d, want 102", last)
	}
}

// TestProcessAttachments_SizeCap skips an oversize attachment without
// staging it and without advancing the checkpoint.
func TestProcessAttachments_SizeCap(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := matchAllMatcher()
	cp := newTestCheckpoint(t)
	// 2 MiB body, with a 1 MiB cap → should be rejected.
	big := bytes.Repeat([]byte("x"), 2*1024*1024)
	client := &fakeClient{
		DownloadFunc: fixedBodyDownloader(big),
	}

	atts := []Attachment{sampleAttachment(200, "huge.bin")}

	staged, skipped := ProcessAttachments(
		context.Background(),
		client,
		writer,
		matcher,
		cp,
		nil, // tel optional
		atts,
		/*parentID*/ 9001,
		/*parentType*/ "message",
		/*matchKey*/ "msg",
		/*surface*/ "message_attachments",
		/*scope*/ "",
		/*maxSizeMB*/ 1,
	)

	if staged != 0 {
		t.Errorf("ProcessAttachments staged %d, want 0", staged)
	}
	if len(skipped) != 1 || skipped[0].Reason != SkipSizeExceeded || skipped[0].ID != 200 {
		t.Errorf("expected one size_exceeded skip for id=200, got %v", skipped)
	}
	if got := stagingDirCount(t, stagingDir); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
	last, err := LastSeenID(cp, "message_attachments", "")
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if last != 0 {
		t.Errorf("checkpoint = %d, want 0 (oversize must not advance)", last)
	}
}

// TestProcessAttachments_Dedup ensures the second call with the same
// attachment ID is skipped by the checkpoint guard.
func TestProcessAttachments_Dedup(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := matchAllMatcher()
	cp := newTestCheckpoint(t)
	client := &fakeClient{
		DownloadFunc: fixedBodyDownloader([]byte("payload")),
	}

	atts := []Attachment{sampleAttachment(300, "notes.pdf")}

	first, firstSkipped := ProcessAttachments(
		context.Background(), client, writer, matcher, cp, nil,
		atts, 7, "feed_post", "feed", "feed_attachments", "kid2", 8,
	)
	if first != 1 {
		t.Fatalf("first call staged %d, want 1", first)
	}
	if len(firstSkipped) != 0 {
		t.Errorf("first call should have no skipped, got %v", firstSkipped)
	}

	second, secondSkipped := ProcessAttachments(
		context.Background(), client, writer, matcher, cp, nil,
		atts, 7, "feed_post", "feed", "feed_attachments", "kid2", 8,
	)
	if second != 0 {
		t.Errorf("second call staged %d, want 0 (dedup)", second)
	}
	// Dedup-skipped attachments are NOT failures; they should not appear
	// in the skipped slice (which is reserved for actionable failures).
	if len(secondSkipped) != 0 {
		t.Errorf("dedup-skipped should not appear in skipped slice, got %v", secondSkipped)
	}

	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Errorf("staging dir has %d items, want 1", got)
	}
}

// TestProcessAttachments_DownloadError skips the failing attachment but
// continues with the rest of the list.
func TestProcessAttachments_DownloadError(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := matchAllMatcher()
	cp := newTestCheckpoint(t)

	failURL := "https://schoology.example/attachment/broken.pdf"
	client := &fakeClient{
		DownloadFunc: func(ctx context.Context, url string) (io.ReadCloser, error) {
			if url == failURL {
				return nil, errors.New("network boom")
			}
			return io.NopCloser(bytes.NewReader([]byte("ok"))), nil
		},
	}

	bad := sampleAttachment(400, "broken.pdf")
	bad.URL = failURL
	good := sampleAttachment(401, "good.pdf")

	staged, skipped := ProcessAttachments(
		context.Background(), client, writer, matcher, cp, nil,
		[]Attachment{bad, good},
		9, "feed_post", "feed", "feed_attachments", "kid3", 8,
	)

	if staged != 1 {
		t.Errorf("ProcessAttachments staged %d, want 1", staged)
	}
	if len(skipped) != 1 || skipped[0].Reason != SkipDownloadError || skipped[0].ID != 400 {
		t.Errorf("expected one download_error skip for id=400, got %v", skipped)
	}
	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Errorf("staging dir has %d items, want 1", got)
	}

	// Checkpoint should reflect the successfully staged attachment.
	last, err := LastSeenID(cp, "feed_attachments", "kid3")
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if last != 401 {
		t.Errorf("checkpoint = %d, want 401", last)
	}

	// Sanity: the staged item's metadata file exists.
	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(stagingDir, e.Name(), "metadata.json")); err != nil {
			t.Errorf("metadata.json missing for %s: %v", e.Name(), err)
		}
	}
}

// TestProcessAttachments_NoRuleMatch skips an attachment when the
// matcher returns ok=false, records it in skipped, and does NOT
// advance the checkpoint.
func TestProcessAttachments_NoRuleMatch(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	// Matcher with an explicit rule that does NOT match our key.
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "totally-other-key", Destination: "messaging"},
	})
	cp := newTestCheckpoint(t)
	client := &fakeClient{
		DownloadFunc: fixedBodyDownloader([]byte("payload")),
	}

	atts := []Attachment{sampleAttachment(500, "orphan.pdf")}
	staged, skipped := ProcessAttachments(
		context.Background(), client, writer, matcher, cp, nil,
		atts, 11, "feed_post", "unmatched-key", "feed_attachments", "kid4", 8,
	)

	if staged != 0 {
		t.Errorf("staged %d, want 0", staged)
	}
	if len(skipped) != 1 || skipped[0].Reason != SkipNoRuleMatch {
		t.Errorf("expected one no_rule_match skip, got %v", skipped)
	}
	if got := stagingDirCount(t, stagingDir); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
	last, _ := LastSeenID(cp, "feed_attachments", "kid4")
	if last != 0 {
		t.Errorf("checkpoint must not advance on no-rule-match, got %d", last)
	}
}

// TestProcessAttachments_CorruptedCheckpoint exercises the
// ShouldStage-returns-error path. The attachment is skipped and the
// poll continues with the next attachment.
func TestProcessAttachments_CorruptedCheckpoint(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := matchAllMatcher()
	cp := newTestCheckpoint(t)
	// Corrupt the checkpoint for the first attachment's surface/scope.
	if err := cp.Save(CheckpointKey("feed_attachments", "kid5"), "not-a-number"); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		DownloadFunc: fixedBodyDownloader([]byte("ok")),
	}

	atts := []Attachment{sampleAttachment(600, "broken.pdf")}
	staged, skipped := ProcessAttachments(
		context.Background(), client, writer, matcher, cp, nil,
		atts, 12, "feed_post", "feed", "feed_attachments", "kid5", 8,
	)

	if staged != 0 {
		t.Errorf("staged %d, want 0", staged)
	}
	if len(skipped) != 1 || skipped[0].Reason != SkipCheckpointReadFailed {
		t.Errorf("expected one checkpoint_read_failed skip, got %v", skipped)
	}
	if got := stagingDirCount(t, stagingDir); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
}

// TestProcessAttachments_MetricsRecorded verifies the optional tel
// parameter is invoked for each skip type. Uses a real Telemetry; we
// cannot easily inspect counter state but we can confirm the call
// does not panic.
func TestProcessAttachments_MetricsRecorded(t *testing.T) {
	tel := NewTestTelemetry(t)

	writer, _ := newTestWriter(t)
	matcher := matchAllMatcher()
	cp := newTestCheckpoint(t)
	client := &fakeClient{
		DownloadFunc: func(ctx context.Context, url string) (io.ReadCloser, error) {
			return nil, errors.New("boom")
		},
	}

	atts := []Attachment{sampleAttachment(700, "any.pdf")}
	staged, skipped := ProcessAttachments(
		context.Background(), client, writer, matcher, cp, tel,
		atts, 13, "feed_post", "feed", "feed_attachments", "kid6", 8,
	)
	if staged != 0 || len(skipped) != 1 || skipped[0].Reason != SkipDownloadError {
		t.Errorf("expected one download_error skip; staged=%d skipped=%v", staged, skipped)
	}
}
