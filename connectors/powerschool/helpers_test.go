package powerschool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
	pslib "github.com/leftathome/powerschool-go"
)

// fakeClient is a hand-rolled PowerSchoolClient for connector tests. Each
// field can be set per-test; nil defaults to a "not configured" error. It
// records every call so tests can assert what was (not) asked for.
//
// Like the library, a successful GetAssignments stamps the grade with the
// student id of the scores page it "read" (Grade.StudentAPIID). The fake
// uses fakeAPIID unless AssignmentsFunc set one itself, or NoAPIID is set.
//
// SelectStudent behaves like the library's on top of HomeFunc: read the
// page, and if another student is marked, "post the switcher form" -- which
// moves Selected unless IgnoreSwitch is set -- then read again and confirm.
// Fixtures built by pollFixture render the page from Selected, so the fake
// has the same server-side selection state the portal has.
type fakeClient struct {
	HomeFunc        func(ctx context.Context) (*pslib.HomePage, error)
	AssignmentsFunc func(ctx context.Context, studentID string, grade *pslib.Grade) ([]*pslib.Assignment, error)
	NoAPIID         bool

	// Selected is the portal-side selection (a student id).
	Selected string
	// IgnoreSwitch makes the "form post" change nothing.
	IgnoreSwitch bool

	HomeCalls        int
	SwitchCalls      []string // student id per form post
	AssignmentsCalls []string // "<studentID>/<courseID>" per call
}

func (f *fakeClient) SelectStudent(ctx context.Context, studentID string) (*pslib.HomePage, error) {
	page, err := f.GetHomePage(ctx)
	if err != nil {
		return nil, err
	}
	selected := page.SelectedStudent()
	if selected == nil {
		return nil, pslib.ErrSelectionUnknown
	}
	if selected.ID == studentID {
		return page, nil
	}
	known := false
	for _, s := range page.Students {
		if s != nil && s.ID == studentID {
			known = true
		}
	}
	if !known {
		return nil, pslib.ErrNotFound
	}
	f.SwitchCalls = append(f.SwitchCalls, studentID)
	if !f.IgnoreSwitch {
		f.Selected = studentID
	}
	page, err = f.GetHomePage(ctx)
	if err != nil {
		return nil, err
	}
	if now := page.SelectedStudent(); now == nil || now.ID != studentID {
		return nil, pslib.ErrSwitchNotConfirmed
	}
	return page, nil
}

// fakeAPIID is the synthetic scores-page student id the fake stamps.
const fakeAPIID = "999999"

func (f *fakeClient) GetHomePage(ctx context.Context) (*pslib.HomePage, error) {
	f.HomeCalls++
	if f.HomeFunc != nil {
		page, err := f.HomeFunc(ctx)
		if err == nil && page == nil {
			return nil, errors.New("fakeClient: HomeFunc returned no page")
		}
		return page, err
	}
	return nil, errors.New("fakeClient.GetHomePage not configured")
}

func (f *fakeClient) GetAssignments(ctx context.Context, studentID string, grade *pslib.Grade) ([]*pslib.Assignment, error) {
	f.AssignmentsCalls = append(f.AssignmentsCalls, studentID+"/"+grade.CourseID)
	if f.AssignmentsFunc == nil {
		return nil, errors.New("fakeClient.GetAssignments not configured")
	}
	out, err := f.AssignmentsFunc(ctx, studentID, grade)
	if err == nil && grade.StudentAPIID == "" && !f.NoAPIID {
		grade.StudentAPIID = fakeAPIID
	}
	return out, err
}

// Compile-time check: fake implements the interface.
var _ PowerSchoolClient = (*fakeClient)(nil)

// stagedItem is one committed staging item: its metadata and its body.
type stagedItem struct {
	Meta staging.ItemMetadata
	Body string
}

// readItems walks stagingDir and returns every committed item, sorted by
// subject so assertions are order-independent. Hidden / .tmp dirs are
// skipped.
func readItems(t *testing.T, stagingDir string) []stagedItem {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var out []stagedItem
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata.json for %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("decode metadata.json for %s: %v", e.Name(), err)
		}
		body, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "content.raw"))
		if err != nil {
			t.Fatalf("read content.raw for %s: %v", e.Name(), err)
		}
		out = append(out, stagedItem{Meta: meta, Body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Subject < out[j].Meta.Subject })
	return out
}

// testNow is the fixed clock for processor tests: a Wednesday afternoon in
// the connector's default timezone.
var testNow = time.Date(2026, 10, 7, 16, 0, 0, 0, time.UTC)

// testKid and testRules are the synthetic single-kid fixture most
// processor tests use.
var testKid = Kid{Name: "k1", StudentID: "111111"}

func testRules() []connector.Rule {
	return []connector.Rule{
		{Match: "powerschool:k1:grade", Destination: "school", DataSubject: "e_333333", Audience: []string{"subject", "guardians"}},
		{Match: "powerschool:k1:assignment", Destination: "school", DataSubject: "e_333333", Audience: []string{"subject", "guardians"}},
		{Match: "powerschool:k1:attendance", Destination: "school", DataSubject: "e_333333", Audience: []string{"subject", "guardians"}},
		{Match: "powerschool:k2:grade", Destination: "school", DataSubject: "e_444444", Audience: []string{"subject", "guardians"}},
		{Match: "powerschool:k2:assignment", Destination: "school", DataSubject: "e_444444", Audience: []string{"subject", "guardians"}},
		{Match: "powerschool:k2:attendance", Destination: "school", DataSubject: "e_444444", Audience: []string{"subject", "guardians"}},
	}
}

// newTestEnv returns an Env on a real on-disk StagingWriter and Checkpoint
// rooted at t.TempDir, plus the staging dir for readItems. Tel is nil,
// which also exercises every processor's nil-telemetry path.
func newTestEnv(t *testing.T, client PowerSchoolClient) (Env, string) {
	t.Helper()
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "powerschool")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	tz, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return Env{
		Client:  client,
		Writer:  w,
		Matcher: connector.NewRuleMatcher(testRules()),
		CP:      cp,
		TZ:      tz,
		Now:     func() time.Time { return testNow },
	}, stagingDir
}

// sampleGrade returns a synthetic course row.
func sampleGrade(courseID, name, q1 string) *pslib.Grade {
	return &pslib.Grade{
		CourseID:   courseID,
		ScoresURL:  "/guardian/scores.html?frn=" + courseID + "&fg=Q1",
		CourseName: name,
		Period:     "1(A)",
		Teacher:    "Teacher, Example",
		Q1Grade:    q1,
	}
}

func floatPtr(f float64) *float64 { return &f }
