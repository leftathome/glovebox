package schoology

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
	schoologylib "github.com/leftathome/schoology-go"
)

// integrationMatcher returns the spec-required routing rules:
//   - schoology:k1:assignment  -> school, data_subject=k1, audience=[household]
//   - schoology:k1:feed        -> school, data_subject=k1, audience=[household]
//   - schoology:k1:attachment  -> school, data_subject=k1, audience=[household]
//   - schoology:message        -> school, data_subject="", audience=[guardians]
//   - schoology-parse-failure:* -> school, data_subject="", audience=[guardians]
//
// (Per messages.go V1 scope, message-attachment isn't exercised: the
// library exposes inbox listings only, with no per-message attachments.)
func integrationMatcher() *connector.RuleMatcher {
	return connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
		{Match: "schoology:k1:feed", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
		{Match: "schoology:k1:attachment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
		{Match: "schoology:message", Destination: "school", Audience: []string{"guardians"}},
		{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
	})
}

// integrationConfig builds a minimal Config with one kid (k1, UID
// 11111111) and exercises ApplyDefaults + ValidateConfig so the test
// asserts a real production-shaped Config rather than a hand-tuned one.
func integrationConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 11111111}},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "08:00", End: "09:00"}},
		},
		Attachments: AttachmentsConfig{MaxSizeMB: 8},
	}
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	return cfg
}

// newIntegrationConnector wires a SchoologyConnector against the
// supplied fakeClient, the spec-required matcher, a real StagingWriter
// and Checkpoint rooted at t.TempDir, and a NewTestTelemetry-style
// Metrics. Returns the connector plus the staging directory path and
// the checkpoint so individual sub-tests can introspect on-disk state.
func newIntegrationConnector(t *testing.T, client *fakeClient) (*SchoologyConnector, string, connector.Checkpoint) {
	t.Helper()
	cfg := integrationConfig(t)
	c, err := NewConnector(client, cfg, "v0.1.0")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}

	writer, stagingDir := newTestWriter(t)
	matcher := integrationMatcher()
	cp := newTestCheckpoint(t)

	metrics, err := connector.NewMetrics("schoology-integration-" + t.Name())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })

	if err := c.Wire(connector.ConnectorContext{
		Writer:  writer,
		Matcher: matcher,
		Metrics: metrics,
	}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	return c, stagingDir, cp
}

// readAllItems walks stagingDir and returns the parsed metadata for
// every committed item directory. Hidden / .tmp entries are skipped.
// Mirrors readAssignmentItems / readFeedItems / readMessageItems but
// without the per-test-file naming because this file owns the whole
// integration view.
func readAllItems(t *testing.T, stagingDir string) []staging.ItemMetadata {
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
		raw, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata.json for %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("parse metadata.json for %s: %v", e.Name(), err)
		}
		out = append(out, meta)
	}
	return out
}

// integrationSampleAssignment constructs a fully-populated *Assignment
// with the given id and course so the test can assert on the course tag
// downstream.
func integrationSampleAssignment(id int64, title, course string) *schoologylib.Assignment {
	return &schoologylib.Assignment{
		ID:          id,
		Title:       title,
		CourseTitle: course,
		DueAt:       time.Date(2026, 5, 30, 23, 59, 0, 0, time.UTC),
		URL:         "/assignment/" + strings.ReplaceAll(title, " ", "_"),
		Status:      schoologylib.AssignmentStatusOverdue,
	}
}

// integrationFeedPost builds a minimal *schoologylib.Post for tests.
// EdgeID is a string of digits so ParseID succeeds.
func integrationFeedPost(edgeID, body, author, course string, ts time.Time) *schoologylib.Post {
	return &schoologylib.Post{
		EdgeID:     edgeID,
		Timestamp:  ts,
		AuthorName: author,
		Body:       body,
		PostedTo:   course,
	}
}

// integrationThread builds a populated MessageThread; subject and sender
// must be non-empty to pass staging validation.
func integrationThread(id int64, subject string) *schoologylib.MessageThread {
	return &schoologylib.MessageThread{
		ID:           id,
		Subject:      subject,
		SenderName:   "Ms. Teacher",
		SenderUID:    7777,
		LastActivity: time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC),
		Unread:       true,
		MessageCount: 3,
		URL:          "/messages/thread/" + subject,
	}
}

// TestIntegration_HappyPath exercises two consecutive successful polls
// against the fake client to confirm:
//
//   - Poll 1 stages 2 assignments + 3 feed posts + 2 feed attachments +
//     1 message thread = 8 items total, with the correct DataSubject /
//     Audience / tags per the spec routing rules.
//   - Checkpoint state advances to the highest-seen id for each
//     (surface, scope).
//   - Poll 2 with overlapping ids + 1 new assignment stages exactly 1
//     additional item (the new assignment) and the rest are deduped
//     by the per-surface checkpoint.
//
// No parse-failure receipts should appear on either poll.
func TestIntegration_HappyPath(t *testing.T) {
	ts := time.Date(2025, 9, 12, 10, 0, 0, 0, time.UTC)

	// Per-call counters let the test switch responses between polls
	// without race conditions; fakeClient is single-goroutine in this
	// test (one Poll call at a time) but atomics keep the pattern safe
	// and idiomatic.
	var (
		assignmentCalls int32
		feedCalls       int32
		inboxCalls      int32
	)

	pollOneAssignments := []*schoologylib.Assignment{
		integrationSampleAssignment(1001, "Read Chapter 3", "English 7"),
		integrationSampleAssignment(1002, "Solve set 4", "Algebra I"),
	}
	pollTwoAssignments := []*schoologylib.Assignment{
		// 1002 dedups against the checkpoint from poll 1; 1003 is new.
		integrationSampleAssignment(1002, "Solve set 4", "Algebra I"),
		integrationSampleAssignment(1003, "Lab writeup", "Biology"),
	}

	feedAttachments := []*schoologylib.Attachment{
		{ID: 3001, URL: "/attachment/3001/source/abc.pdf?checkad=1", Filename: "syllabus.pdf", MimeType: "application/pdf"},
		{ID: 3002, URL: "/attachment/3002/source/def.pdf?checkad=1", Filename: "rubric.pdf", MimeType: "application/pdf"},
	}
	// Build the three feed posts; the middle one carries two attachments
	// so attachments are exercised end-to-end (download + stage).
	feedPosts := []*schoologylib.Post{
		integrationFeedPost("2001", "Welcome to room 4", "Mr Adams", "Algebra I", ts),
		func() *schoologylib.Post {
			p := integrationFeedPost("2002", "Permission slip due Friday", "Ms Smith", "Algebra I", ts)
			p.Attachments = feedAttachments
			return p
		}(),
		integrationFeedPost("2003", "Reminder", "Mr Adams", "Algebra I", ts),
	}
	threads := []*schoologylib.MessageThread{
		integrationThread(4001, "Field trip permission"),
	}

	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			n := atomic.AddInt32(&assignmentCalls, 1)
			switch n {
			case 1:
				return pollOneAssignments, nil, nil
			case 2:
				return pollTwoAssignments, nil, nil
			}
			return nil, nil, nil
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			atomic.AddInt32(&feedCalls, 1)
			// Same feed contents on both polls so we exercise dedup on
			// the feed surface across calls.
			return feedPosts, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			atomic.AddInt32(&inboxCalls, 1)
			// Same inbox both polls; second poll must dedup.
			return threads, nil, nil
		},
		DownloadFunc: func(ctx context.Context, url string) (io.ReadCloser, error) {
			// Body is <10 bytes so the 8 MiB size cap is not hit and
			// every attachment commits.
			return io.NopCloser(bytes.NewReader([]byte("PDFBYTES"))), nil
		},
	}

	c, stagingDir, cp := newIntegrationConnector(t, client)

	// --- Poll 1 -------------------------------------------------------
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	// 2 assignments + 3 feed posts + 2 attachments + 1 message = 8.
	if got := stagingDirCount(t, stagingDir); got != 8 {
		t.Errorf("after poll 1: staging dir has %d items, want 8", got)
	}

	items := readAllItems(t, stagingDir)
	counts := categorizeItems(items)
	if counts.assignments != 2 {
		t.Errorf("poll 1 assignments = %d, want 2", counts.assignments)
	}
	if counts.feedPosts != 3 {
		t.Errorf("poll 1 feed posts = %d, want 3", counts.feedPosts)
	}
	if counts.attachments != 2 {
		t.Errorf("poll 1 attachments = %d, want 2", counts.attachments)
	}
	if counts.messages != 1 {
		t.Errorf("poll 1 messages = %d, want 1", counts.messages)
	}
	if counts.receipts != 0 {
		t.Errorf("poll 1 receipts = %d, want 0 (clean poll)", counts.receipts)
	}

	// Routing assertions: assignments / feed / attachments are k1 +
	// [household]; the message is "" + [guardians]. Tag presence
	// confirms each kind passed through its expected build path.
	for _, m := range items {
		assertRouting(t, m)
	}

	// Surface-specific tag presence: at least one item per surface
	// carries the tag the brief calls out as a smoke check.
	if !counts.hasCourseTag {
		t.Errorf("no assignment item carried a 'course' tag")
	}
	if !counts.hasPostTypeTag {
		t.Errorf("no feed item carried a 'post_type' tag")
	}
	if !counts.hasAttachmentParentTag {
		t.Errorf("no attachment item carried a 'parent_id' tag")
	}
	if !counts.hasThreadTag {
		t.Errorf("no message item carried a 'thread_id' tag")
	}

	// Checkpoint state after poll 1.
	assertCheckpoint(t, cp, "assignment", "k1", 1002)
	assertCheckpoint(t, cp, "feed", "k1", 2003)
	assertCheckpoint(t, cp, "feed_attachment", "k1", 3002)
	assertCheckpoint(t, cp, "message", "", 4001)

	// --- Poll 2 -------------------------------------------------------
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	// Only one new item should appear: assignment 1003. The rest dedup.
	if got := stagingDirCount(t, stagingDir); got != 9 {
		t.Errorf("after poll 2: staging dir has %d items, want 9 (8 + 1 new assignment)", got)
	}

	itemsAfter := readAllItems(t, stagingDir)
	countsAfter := categorizeItems(itemsAfter)
	if countsAfter.receipts != 0 {
		t.Errorf("poll 2 receipts = %d, want 0 (clean polls produce none)", countsAfter.receipts)
	}
	if countsAfter.assignments != 3 {
		t.Errorf("poll 2 assignments on disk = %d, want 3 (2 from poll 1 + 1 new)", countsAfter.assignments)
	}

	// Assignment checkpoint advanced; feed / attachment / message
	// checkpoints are unchanged.
	assertCheckpoint(t, cp, "assignment", "k1", 1003)
	assertCheckpoint(t, cp, "feed", "k1", 2003)
	assertCheckpoint(t, cp, "feed_attachment", "k1", 3002)
	assertCheckpoint(t, cp, "message", "", 4001)

	// The fake should have been called exactly twice on each surface.
	if got := atomic.LoadInt32(&assignmentCalls); got != 2 {
		t.Errorf("assignment polls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&feedCalls); got != 2 {
		t.Errorf("feed polls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&inboxCalls); got != 2 {
		t.Errorf("inbox polls = %d, want 2", got)
	}
}

// TestIntegration_ParseFailurePath confirms that a non-auth library
// error (ErrCodeParse from the assignments surface) does NOT escalate
// to a PermanentError -- it is converted to a parse-failure receipt and
// the poll continues. The receipt routes via the
// schoology-parse-failure:* catch-all and carries Audience=[guardians]
// + empty DataSubject per spec §11.3.
func TestIntegration_ParseFailurePath(t *testing.T) {
	parseErr := &schoologylib.Error{
		Code:    schoologylib.ErrCodeParse,
		Op:      "GetOverdueSubmissions",
		Message: "selector .upcoming-event vanished",
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return nil, nil, parseErr
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}

	c, stagingDir, cp := newIntegrationConnector(t, client)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("poll with parse error returned %v, want nil (parse errors are NOT permanent)", err)
	}

	items := readAllItems(t, stagingDir)
	var receipts []staging.ItemMetadata
	for _, m := range items {
		if m.Source == "schoology-parse-failure" {
			receipts = append(receipts, m)
		}
	}
	if len(receipts) != 1 {
		t.Fatalf("got %d parse-failure receipts, want 1 (items=%+v)", len(receipts), items)
	}

	r := receipts[0]
	if r.Tags["parser"] != "assignments" {
		t.Errorf("receipt parser tag = %q, want %q", r.Tags["parser"], "assignments")
	}
	if r.Tags["error_class"] != "parse" {
		t.Errorf("receipt error_class tag = %q, want %q", r.Tags["error_class"], "parse")
	}
	if r.DataSubject != "" {
		t.Errorf("receipt DataSubject = %q, want empty (spec §11.3)", r.DataSubject)
	}
	if len(r.Audience) != 1 || r.Audience[0] != "guardians" {
		t.Errorf("receipt Audience = %v, want [guardians]", r.Audience)
	}
	if r.DestinationAgent != "school" {
		t.Errorf("receipt DestinationAgent = %q, want %q", r.DestinationAgent, "school")
	}
}

// TestIntegration_AuthFailureEscalates confirms that a session-expired
// (ErrCodeAuth) failure on the assignments surface escalates to a
// PermanentError so the framework's RunWatchLoop exits the process.
// The error message must reference docs/AUTH-RECOVERY.md per spec
// §11.1 so the operator runbook is one grep away.
func TestIntegration_AuthFailureEscalates(t *testing.T) {
	authErr := &schoologylib.Error{
		Code:    schoologylib.ErrCodeAuth,
		Op:      "GetOverdueSubmissions",
		Message: "session expired",
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return nil, nil, authErr
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}

	c, _, cp := newIntegrationConnector(t, client)

	err := c.Poll(context.Background(), cp)
	if err == nil {
		t.Fatal("poll with auth error returned nil; want PermanentError")
	}
	if !connector.IsPermanent(err) {
		t.Errorf("poll error is not permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "AUTH-RECOVERY.md") {
		t.Errorf("error message %q must reference docs/AUTH-RECOVERY.md per spec §11.1", err.Error())
	}
	if !strings.Contains(err.Error(), "session expired") {
		t.Errorf("error message %q must mention 'session expired'", err.Error())
	}
}

// itemCounts aggregates per-surface item totals + tag-presence flags.
type itemCounts struct {
	assignments            int
	feedPosts              int
	attachments            int
	messages               int
	receipts               int
	hasCourseTag           bool
	hasPostTypeTag         bool
	hasAttachmentParentTag bool
	hasThreadTag           bool
}

// categorizeItems classifies staged metadata into the four content
// surfaces + receipts. Classification key: surfaces are disambiguated
// by Source first, then by a tag specific to each handler.
//
//   - schoology-parse-failure -> receipt
//   - source=schoology, tags has "thread_id" -> message
//   - source=schoology, tags has "course" but not "post_type" -> assignment
//   - source=schoology, tags has "post_type" -> feed post
//   - source=schoology, tags has "parent_id" or content-type starts with
//     application/ -> attachment (attachments.go sets parent_id +
//     parent_type and writes the binary content with MimeType)
func categorizeItems(items []staging.ItemMetadata) itemCounts {
	var c itemCounts
	for _, m := range items {
		if m.Source == "schoology-parse-failure" {
			c.receipts++
			continue
		}
		switch {
		case m.Tags["thread_id"] != "":
			c.messages++
			c.hasThreadTag = true
		case m.Tags["parent_id"] != "":
			c.attachments++
			c.hasAttachmentParentTag = true
		case m.Tags["post_type"] != "":
			c.feedPosts++
			c.hasPostTypeTag = true
			if m.Tags["course"] != "" {
				// Some feed posts also carry a course tag; don't flip
				// the assignment-specific course flag here.
			}
		default:
			// Assignments are the only remaining source=schoology
			// surface; they carry the "course" tag from
			// buildAssignmentItemOptions.
			c.assignments++
			if m.Tags["course"] != "" {
				c.hasCourseTag = true
			}
		}
	}
	return c
}

// assertRouting checks the DataSubject + Audience per the spec routing
// table. Receipts have their own rules and are checked separately by
// TestIntegration_ParseFailurePath.
func assertRouting(t *testing.T, m staging.ItemMetadata) {
	t.Helper()
	if m.Source == "schoology-parse-failure" {
		// Receipts: tested explicitly elsewhere.
		return
	}
	if m.DestinationAgent != "school" {
		t.Errorf("item %q: DestinationAgent = %q, want school", m.Subject, m.DestinationAgent)
	}
	switch {
	case m.Tags["thread_id"] != "":
		// Message: "" / [guardians].
		if m.DataSubject != "" {
			t.Errorf("message item: DataSubject = %q, want empty", m.DataSubject)
		}
		if len(m.Audience) != 1 || m.Audience[0] != "guardians" {
			t.Errorf("message item: Audience = %v, want [guardians]", m.Audience)
		}
	default:
		// Assignment / feed / attachment: k1 / [household].
		if m.DataSubject != "k1" {
			t.Errorf("item %q (source=%s): DataSubject = %q, want k1", m.Subject, m.Source, m.DataSubject)
		}
		if len(m.Audience) != 1 || m.Audience[0] != "household" {
			t.Errorf("item %q (source=%s): Audience = %v, want [household]", m.Subject, m.Source, m.Audience)
		}
	}
}

// assertCheckpoint reads a checkpoint surface/scope and fails the test
// if it does not match the expected value.
func assertCheckpoint(t *testing.T, cp connector.Checkpoint, surface, scope string, want int64) {
	t.Helper()
	got, err := LastSeenID(cp, surface, scope)
	if err != nil {
		t.Fatalf("LastSeenID(%s, %s): %v", surface, scope, err)
	}
	if got != want {
		t.Errorf("checkpoint %s/%s = %d, want %d", surface, scope, got, want)
	}
}
