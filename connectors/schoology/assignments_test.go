package schoology

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
	schoologylib "github.com/leftathome/schoology-go"
)

// readAssignmentItems walks stagingDir and returns the parsed
// metadata.json for every committed item directory. Hidden / .tmp dirs
// are skipped. The name avoids collision with the parallel feed and
// messages processor tasks (see task brief).
func readAssignmentItems(t *testing.T, stagingDir string) []staging.ItemMetadata {
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
		path := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read metadata.json for %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("decode metadata.json for %s: %v", e.Name(), err)
		}
		out = append(out, meta)
	}
	return out
}

// sampleAssignment returns a minimally-populated *Assignment.
func sampleAssignment(id int64, title, course string) *schoologylib.Assignment {
	return &schoologylib.Assignment{
		ID:          id,
		Title:       title,
		CourseTitle: course,
		DueAt:       time.Date(2026, 5, 30, 23, 59, 0, 0, time.UTC),
		URL:         "/assignment/" + strings.ReplaceAll(title, " ", "_"),
		Status:      schoologylib.AssignmentStatusOverdue,
	}
}

// assignmentMatcher returns a RuleMatcher with the per-kid assignment
// rule plus a wildcard parse-failure rule so receipts have a home.
func assignmentMatcher(kidName string) *connector.RuleMatcher {
	return connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:" + kidName + ":assignment", Destination: "school"},
		{Match: "schoology-parse-failure:*", Destination: "school"},
	})
}

// TestProcessAssignments_StagesNewItems exercises the happy path: two
// assignments staged, errsLogged=false, checkpoint advanced to the
// highest ID, and the `course` tag survives the round-trip.
func TestProcessAssignments_StagesNewItems(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	kid := Kid{Name: "k1", SchoologyUID: 1001}
	matcher := assignmentMatcher(kid.Name)
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	assignments := []*schoologylib.Assignment{
		sampleAssignment(9100000001, "Read Chapter 3", "English 7"),
		sampleAssignment(9100000002, "Solve set 4", "Algebra I"),
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			if childUID != kid.SchoologyUID {
				t.Errorf("childUID = %d, want %d", childUID, kid.SchoologyUID)
			}
			return assignments, nil, nil
		},
	}

	staged, errsLogged := ProcessAssignments(
		context.Background(), client, writer, matcher, cp, dedup, kid, "v0.1.0",
	)

	if staged != 2 {
		t.Errorf("staged = %d, want 2", staged)
	}
	if errsLogged {
		t.Errorf("errsLogged = true, want false on clean poll")
	}
	if got := stagingDirCount(t, stagingDir); got != 2 {
		t.Errorf("staging dir has %d items, want 2", got)
	}

	last, err := LastSeenID(cp, "assignment", kid.Name)
	if err != nil {
		t.Fatalf("LastSeenID: %v", err)
	}
	if last != 9100000002 {
		t.Errorf("checkpoint = %d, want 9100000002 (highest staged id)", last)
	}

	// Inspect at least one item's tags to ensure the course tag was set.
	items := readAssignmentItems(t, stagingDir)
	if len(items) != 2 {
		t.Fatalf("readAssignmentItems returned %d items, want 2", len(items))
	}
	foundCourseTag := false
	for _, m := range items {
		if m.Source != "schoology" {
			t.Errorf("item source = %q, want %q", m.Source, "schoology")
		}
		if m.DestinationAgent != "school" {
			t.Errorf("item destination = %q, want %q", m.DestinationAgent, "school")
		}
		if v, ok := m.Tags["course"]; ok && (v == "English 7" || v == "Algebra I") {
			foundCourseTag = true
		}
		if m.Tags["status"] != string(schoologylib.AssignmentStatusOverdue) {
			t.Errorf("status tag = %q, want %q", m.Tags["status"], schoologylib.AssignmentStatusOverdue)
		}
	}
	if !foundCourseTag {
		t.Errorf("no item carried an expected course tag; tags=%v", items)
	}
}

// TestProcessAssignments_DedupesAcrossPolls confirms a second poll
// returning the same assignment IDs stages zero items (checkpoint guard
// blocks them) while still leaving the original item on disk.
func TestProcessAssignments_DedupesAcrossPolls(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	kid := Kid{Name: "k1", SchoologyUID: 1001}
	matcher := assignmentMatcher(kid.Name)
	cp := newTestCheckpoint(t)

	assignments := []*schoologylib.Assignment{
		sampleAssignment(9100000010, "Lab writeup", "Biology"),
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return assignments, nil, nil
		},
	}

	first, _ := ProcessAssignments(
		context.Background(), client, writer, matcher, cp, NewReceiptDedup(), kid, "v0.1.0",
	)
	if first != 1 {
		t.Fatalf("first poll staged %d, want 1", first)
	}
	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Fatalf("after first poll staging dir = %d, want 1", got)
	}

	second, errsLogged := ProcessAssignments(
		context.Background(), client, writer, matcher, cp, NewReceiptDedup(), kid, "v0.1.0",
	)
	if second != 0 {
		t.Errorf("second poll staged %d, want 0 (dedup)", second)
	}
	if errsLogged {
		t.Errorf("errsLogged = true on clean dedup poll")
	}
	if got := stagingDirCount(t, stagingDir); got != 1 {
		t.Errorf("after second poll staging dir = %d, want 1", got)
	}
}

// TestProcessAssignments_LibraryErrorEmitsReceipt verifies a top-level
// GetOverdueSubmissions failure produces a parse-failure receipt and
// short-circuits with staged=0.
func TestProcessAssignments_LibraryErrorEmitsReceipt(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	kid := Kid{Name: "k1", SchoologyUID: 1001}
	matcher := assignmentMatcher(kid.Name)
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	libErr := &schoologylib.Error{
		Code:    schoologylib.ErrCodeAuth,
		Op:      "GetOverdueSubmissions",
		Message: "session expired",
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return nil, nil, libErr
		},
	}

	staged, errsLogged := ProcessAssignments(
		context.Background(), client, writer, matcher, cp, dedup, kid, "v0.1.0",
	)
	if staged != 0 {
		t.Errorf("staged = %d, want 0", staged)
	}
	if !errsLogged {
		t.Errorf("errsLogged = false, want true on library error")
	}

	items := readAssignmentItems(t, stagingDir)
	receiptCount := 0
	for _, m := range items {
		if m.Source == "schoology-parse-failure" {
			receiptCount++
			if m.Tags["parser"] != "assignments" {
				t.Errorf("receipt parser tag = %q, want %q", m.Tags["parser"], "assignments")
			}
			if m.Tags["error_class"] != "auth" {
				t.Errorf("receipt error_class tag = %q, want %q", m.Tags["error_class"], "auth")
			}
		}
	}
	if receiptCount != 1 {
		t.Errorf("expected exactly 1 parse-failure receipt, got %d (items=%v)", receiptCount, items)
	}

	// Classifier sanity (also covers our unexported helper).
	if got := classifyAssignmentsErr(libErr); got != "auth" {
		t.Errorf("classifyAssignmentsErr(authErr) = %q, want %q", got, "auth")
	}
	if got := classifyAssignmentsErr(errors.New("plain")); got != "unknown" {
		t.Errorf("classifyAssignmentsErr(plain) = %q, want %q", got, "unknown")
	}
}

// TestProcessAssignments_ParseErrorsEmitOneReceiptPerPoll confirms that
// when the library returns successfully-parsed items plus a
// ParseErrors slice of multiple same-class failures, the items still
// stage and exactly ONE receipt is emitted (dedup collapses the
// duplicates).
func TestProcessAssignments_ParseErrorsEmitOneReceiptPerPoll(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	kid := Kid{Name: "k1", SchoologyUID: 1001}
	matcher := assignmentMatcher(kid.Name)
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	assignments := []*schoologylib.Assignment{
		sampleAssignment(9100000020, "Essay", "English 7"),
		sampleAssignment(9100000021, "Quiz", "History"),
	}
	parseErrs := schoologylib.ParseErrors{
		schoologylib.NewParseError("parseOverdueSubmissions: .upcoming-event", "missing assignment link"),
		schoologylib.NewParseError("parseOverdueSubmissions: .upcoming-event", "missing assignment link"),
		schoologylib.NewParseError("parseOverdueSubmissions: .upcoming-event", "missing assignment link"),
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return assignments, parseErrs, nil
		},
	}

	staged, errsLogged := ProcessAssignments(
		context.Background(), client, writer, matcher, cp, dedup, kid, "v0.1.0",
	)
	if staged != 2 {
		t.Errorf("staged = %d, want 2 (parse errors should not block successful items)", staged)
	}
	if !errsLogged {
		t.Errorf("errsLogged = false, want true with parse errors present")
	}

	items := readAssignmentItems(t, stagingDir)
	receiptCount := 0
	assignmentCount := 0
	for _, m := range items {
		switch m.Source {
		case "schoology-parse-failure":
			receiptCount++
		case "schoology":
			assignmentCount++
		}
	}
	if receiptCount != 1 {
		t.Errorf("expected exactly 1 parse-failure receipt (dedup collapses), got %d", receiptCount)
	}
	if assignmentCount != 2 {
		t.Errorf("expected 2 assignment items staged, got %d", assignmentCount)
	}
}

// TestProcessAssignments_NoRuleMatch verifies that an absent rule for
// `schoology:<kid>:assignment` short-circuits staging (count=0,
// errsLogged=false, nothing on disk).
func TestProcessAssignments_NoRuleMatch(t *testing.T) {
	writer, stagingDir := newTestWriter(t)
	kid := Kid{Name: "k1", SchoologyUID: 1001}
	// Matcher with no rule for schoology:k1:assignment.
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:other-kid:assignment", Destination: "school"},
	})
	cp := newTestCheckpoint(t)
	dedup := NewReceiptDedup()

	assignments := []*schoologylib.Assignment{
		sampleAssignment(9100000030, "Orphan", "Geography"),
	}
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return assignments, nil, nil
		},
	}

	staged, errsLogged := ProcessAssignments(
		context.Background(), client, writer, matcher, cp, dedup, kid, "v0.1.0",
	)
	if staged != 0 {
		t.Errorf("staged = %d, want 0 on no-rule-match", staged)
	}
	if errsLogged {
		t.Errorf("errsLogged = true; no-rule-match alone should not flip the flag")
	}
	if got := stagingDirCount(t, stagingDir); got != 0 {
		t.Errorf("staging dir has %d items, want 0", got)
	}
}
