package schoology

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
	schoologylib "github.com/leftathome/schoology-go"
)

// readFeedItems reads every committed item's metadata.json from
// stagingDir and returns the parsed slice. Skips hidden (.tmp)
// directories. Named with a feed-specific prefix to avoid collisions
// with the parallel assignments/messages task helpers.
func readFeedItems(t *testing.T, stagingDir string) []staging.ItemMetadata {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var out []staging.ItemMetadata
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata.json for %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(b, &meta); err != nil {
			t.Fatalf("parse metadata.json for %s: %v", e.Name(), err)
		}
		out = append(out, meta)
	}
	return out
}

// feedRuleMatcher returns a matcher with both the feed and feed
// attachment rules for the given kid, plus the parse-failure
// catch-all.
func feedRuleMatcher(kid string) *connector.RuleMatcher {
	return connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:" + kid + ":feed", Destination: "school", Audience: []string{"guardians"}},
		{Match: "schoology:" + kid + ":attachment", Destination: "school", Audience: []string{"guardians"}},
		{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
	})
}

// feedPost builds a minimal *schoologylib.Post for tests.
func feedPost(edgeID, body, author, course string, ts time.Time) *schoologylib.Post {
	return &schoologylib.Post{
		EdgeID:     edgeID,
		Timestamp:  ts,
		AuthorName: author,
		Body:       body,
		PostedTo:   course,
	}
}

func TestProcessFeed_StagesNewPosts(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)
	ts := time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC)

	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return []*schoologylib.Post{
				feedPost("100", "Welcome to room 4", "Mr Adams", "Algebra I", ts),
				feedPost("101", "Permission slip due Friday", "Ms Smith", "Algebra I", ts),
			}, nil, nil
		},
	}
	dedup := NewReceiptDedup()
	kid := Kid{Name: "k1", SchoologyUID: 42}

	staged, attsStaged, errsLogged := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, dedup,
		kid, 8, "v0.1.0",
	)
	if staged != 2 {
		t.Errorf("staged = %d, want 2", staged)
	}
	if attsStaged != 0 {
		t.Errorf("attachmentsStaged = %d, want 0", attsStaged)
	}
	if errsLogged {
		t.Error("errsLogged should be false on clean poll")
	}
	if got := stagingDirCount(t, stagingDir); got != 2 {
		t.Fatalf("staging dir has %d items, want 2", got)
	}

	last, err := LastSeenID(cp, "feed", "k1")
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if last != 101 {
		t.Errorf("checkpoint = %d, want 101", last)
	}

	items := readFeedItems(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("readFeedItems returned %d items, want 2", len(items))
	}
	for _, it := range items {
		if it.Source != "schoology" {
			t.Errorf("Source = %q, want schoology", it.Source)
		}
		if it.ContentType != "text/plain" {
			t.Errorf("ContentType = %q, want text/plain", it.ContentType)
		}
		if it.DestinationAgent != "school" {
			t.Errorf("DestinationAgent = %q, want school", it.DestinationAgent)
		}
		if it.Tags["course"] != "Algebra I" {
			t.Errorf("course tag = %q, want Algebra I", it.Tags["course"])
		}
		if it.Tags["post_type"] != "post" {
			t.Errorf("post_type tag = %q, want post", it.Tags["post_type"])
		}
	}
}

func TestProcessFeed_DedupesAcrossPolls(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)
	ts := time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC)

	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return []*schoologylib.Post{
				feedPost("200", "hello", "Mr A", "Bio", ts),
			}, nil, nil
		},
	}
	kid := Kid{Name: "k1", SchoologyUID: 42}

	first, _, _ := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if first != 1 {
		t.Fatalf("first poll staged %d, want 1", first)
	}

	second, _, _ := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if second != 0 {
		t.Errorf("second poll staged %d, want 0 (dedup)", second)
	}
	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Errorf("staging dir has %d items, want 1", got)
	}
}

func TestProcessFeed_LibraryErrorEmitsReceipt(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)

	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, &schoologylib.Error{
				Code:    schoologylib.ErrCodeServer,
				Op:      "GetFeed",
				Message: "boom",
			}
		},
	}
	kid := Kid{Name: "k1", SchoologyUID: 42}

	staged, attsStaged, errsLogged := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if staged != 0 {
		t.Errorf("staged = %d, want 0", staged)
	}
	if attsStaged != 0 {
		t.Errorf("attachmentsStaged = %d, want 0", attsStaged)
	}
	if !errsLogged {
		t.Error("errsLogged should be true when top-level error emits a receipt")
	}
	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Fatalf("staging dir has %d items, want 1 (the receipt)", got)
	}

	items := readFeedItems(t, stagingDir)
	if len(items) != 1 || items[0].Source != "schoology-parse-failure" {
		t.Fatalf("expected one schoology-parse-failure item, got %+v", items)
	}
	if items[0].Tags["parser"] != "feed" {
		t.Errorf("parser tag = %q, want feed", items[0].Tags["parser"])
	}
	if items[0].Tags["target_kid"] != "k1" {
		t.Errorf("target_kid = %q, want k1", items[0].Tags["target_kid"])
	}
}

func TestProcessFeed_StagesAttachments(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)
	ts := time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC)

	post := feedPost("300", "Read the syllabus", "Ms B", "English I", ts)
	post.Attachments = []*schoologylib.Attachment{
		{ID: 9001, URL: "/attachment/9001/source/abc.pdf?checkad=1", Filename: "syllabus.pdf", MimeType: "application/pdf"},
	}

	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return []*schoologylib.Post{post}, nil, nil
		},
		DownloadFunc: func(ctx context.Context, url string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte("PDFBYTES"))), nil
		},
	}
	kid := Kid{Name: "k1", SchoologyUID: 42}

	staged, attsStaged, errsLogged := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if staged != 1 {
		t.Errorf("staged = %d, want 1", staged)
	}
	if attsStaged != 1 {
		t.Errorf("attachmentsStaged = %d, want 1", attsStaged)
	}
	if errsLogged {
		t.Error("errsLogged should be false")
	}
	if got := stagingDirCount(t, stagingDir); got != 2 {
		t.Errorf("staging dir has %d items, want 2 (post + attachment)", got)
	}

	// Attachment checkpoint advances under feed_attachment:k1.
	lastAtt, err := LastSeenID(cp, "feed_attachment", "k1")
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if lastAtt != 9001 {
		t.Errorf("feed_attachment checkpoint = %d, want 9001", lastAtt)
	}
}

func TestProcessFeed_NonNumericEdgeIDSkipped(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)
	ts := time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC)

	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return []*schoologylib.Post{
				feedPost("abc", "garbled", "Mr A", "Bio", ts),
			}, nil, nil
		},
	}
	kid := Kid{Name: "k1", SchoologyUID: 42}

	staged, attsStaged, errsLogged := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if staged != 0 {
		t.Errorf("staged = %d, want 0", staged)
	}
	if attsStaged != 0 {
		t.Errorf("attachmentsStaged = %d, want 0", attsStaged)
	}
	if errsLogged {
		t.Error("errsLogged should be false; per-item parse failures are not receipts")
	}
	if got := stagingDirCount(t, stagingDir); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
}

func TestProcessFeed_ParseErrorsEmitReceipt(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)
	ts := time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC)

	parseErrs := schoologylib.ParseErrors{
		schoologylib.NewParseError("parseFeed: li", "missing timestamp attr on li#1"),
		schoologylib.NewParseError("parseFeed: li", "missing timestamp attr on li#2"),
	}
	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return []*schoologylib.Post{
				feedPost("400", "valid", "Mr A", "Bio", ts),
			}, parseErrs, nil
		},
	}
	kid := Kid{Name: "k1", SchoologyUID: 42}

	staged, _, errsLogged := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if staged != 1 {
		t.Errorf("staged = %d, want 1", staged)
	}
	if !errsLogged {
		t.Error("errsLogged should be true")
	}

	items := readFeedItems(t, stagingDir)
	receipts := 0
	posts := 0
	for _, it := range items {
		switch it.Source {
		case "schoology-parse-failure":
			receipts++
		case "schoology":
			posts++
		}
	}
	if receipts != 1 {
		t.Errorf("expected exactly 1 parse-failure receipt, got %d (items=%+v)", receipts, items)
	}
	if posts != 1 {
		t.Errorf("expected 1 staged post, got %d", posts)
	}
}

// TestProcessFeed_ParseErrorsAffectedCount confirms that N>1 row-parse
// failures in one poll produce exactly ONE receipt whose affected_count
// tag equals N (the final tally), not 1 (the first observation).
func TestProcessFeed_ParseErrorsAffectedCount(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	matcher := feedRuleMatcher("k1")
	cp := newTestCheckpoint(t)

	const n = 3
	parseErrs := schoologylib.ParseErrors{
		schoologylib.NewParseError("parseFeed: li", "missing timestamp attr on li#1"),
		schoologylib.NewParseError("parseFeed: li", "missing timestamp attr on li#2"),
		schoologylib.NewParseError("parseFeed: li", "missing timestamp attr on li#3"),
	}
	client := &fakeClient{
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, parseErrs, nil
		},
	}
	kid := Kid{Name: "k1", SchoologyUID: 42}

	_, _, errsLogged := ProcessFeed(
		context.Background(), client, writer, matcher, cp, nil, NewReceiptDedup(),
		kid, 8, "v0.1.0",
	)
	if !errsLogged {
		t.Error("errsLogged should be true")
	}

	items := readFeedItems(t, stagingDir)
	var receipts []staging.ItemMetadata
	for _, it := range items {
		if it.Source == "schoology-parse-failure" {
			receipts = append(receipts, it)
		}
	}
	if len(receipts) != 1 {
		t.Fatalf("expected exactly 1 parse-failure receipt, got %d (items=%+v)", len(receipts), items)
	}
	if got := receipts[0].Tags["affected_count"]; got != strconv.Itoa(n) {
		t.Errorf("affected_count = %q, want %q (final tally, not first observation)", got, strconv.Itoa(n))
	}
}

// Sanity: the *Error we construct in TestProcessFeed_LibraryErrorEmitsReceipt
// is a non-auth/non-parse case so classifyLibError falls through to the
// Code switch. errors.As against *schoologylib.Error must succeed.
func TestClassifyLibError_ServerCode(t *testing.T) {
	err := &schoologylib.Error{Code: schoologylib.ErrCodeServer, Message: "x"}
	if got := classifyLibError(err); got != "server" {
		t.Errorf("classifyLibError = %q, want server", got)
	}
	if got := classifyLibError(errors.New("plain")); got != "unknown" {
		t.Errorf("plain error classifyLibError = %q, want unknown", got)
	}
}
