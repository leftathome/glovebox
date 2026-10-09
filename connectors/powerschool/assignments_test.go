package powerschool

import (
	"context"
	"errors"
	"testing"
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// due returns a UTC-midnight due date the way the library parses one.
func due(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// sampleAssignment returns an unscored, unflagged assignment due the day
// after testNow.
func sampleAssignment(id, title string) *pslib.Assignment {
	return &pslib.Assignment{
		ID:       id,
		CourseID: "00100",
		Title:    title,
		DueDate:  due(2026, 10, 8),
		Category: "Classwork",
		MaxScore: 10,
		Status:   pslib.StatusPending,
	}
}

func TestIsOutstanding(t *testing.T) {
	scored := func(a *pslib.Assignment) *pslib.Assignment {
		a.Score, a.Status = floatPtr(8), pslib.StatusGraded
		return a
	}
	flagged := func(a *pslib.Assignment, status pslib.AssignmentStatus, flags ...pslib.AssignmentFlag) *pslib.Assignment {
		a.Status, a.Flags = status, flags
		return a
	}
	cases := []struct {
		name string
		a    *pslib.Assignment
		want bool
	}{
		{"unscored and not collected", sampleAssignment("1", "a"), true},
		{"scored", scored(sampleAssignment("1", "a")), false},
		{"graded zero with nil score", flagged(sampleAssignment("1", "a"), pslib.StatusGraded), false},
		{"missing", flagged(sampleAssignment("1", "a"), pslib.StatusMissing, pslib.FlagMissing), true},
		{"scored but still flagged missing", flagged(scored(sampleAssignment("1", "a")), pslib.StatusMissing, pslib.FlagMissing), true},
		{"incomplete", flagged(sampleAssignment("1", "a"), pslib.StatusIncomplete, pslib.FlagIncomplete), true},
		{"collected, awaiting a score", flagged(sampleAssignment("1", "a"), pslib.StatusCollected, pslib.FlagCollected), false},
		{"exempt beats missing", flagged(sampleAssignment("1", "a"), pslib.StatusExempt, pslib.FlagExempt, pslib.FlagMissing), false},
		{"late but scored", flagged(scored(sampleAssignment("1", "a")), pslib.StatusGraded, pslib.FlagLate), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOutstanding(tc.a); got != tc.want {
				t.Errorf("isOutstanding = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssignmentFingerprint(t *testing.T) {
	base := assignmentFingerprint(sampleAssignment("1", "Lab report"))
	if base != assignmentFingerprint(sampleAssignment("1", "Lab report")) {
		t.Fatal("fingerprint is not deterministic")
	}
	mutations := map[string]func(a *pslib.Assignment){
		"score":      func(a *pslib.Assignment) { a.Score = floatPtr(7) },
		"zero score": func(a *pslib.Assignment) { a.Score = floatPtr(0) },
		"status":     func(a *pslib.Assignment) { a.Status = pslib.StatusGraded },
		"flag":       func(a *pslib.Assignment) { a.Flags = []pslib.AssignmentFlag{pslib.FlagMissing} },
		"due date":   func(a *pslib.Assignment) { a.DueDate = due(2026, 10, 9) },
		"max score":  func(a *pslib.Assignment) { a.MaxScore = 20 },
		"percent":    func(a *pslib.Assignment) { a.Percentage = floatPtr(70) },
		"letter":     func(a *pslib.Assignment) { a.LetterGrade = "C" },
		"title":      func(a *pslib.Assignment) { a.Title = "Lab report (revised)" },
		"category":   func(a *pslib.Assignment) { a.Category = "Lab" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			a := sampleAssignment("1", "Lab report")
			mutate(a)
			if assignmentFingerprint(a) == base {
				t.Errorf("changing %s did not change the fingerprint", name)
			}
		})
	}

	// Flag order is not a change.
	x, y := sampleAssignment("1", "Lab report"), sampleAssignment("1", "Lab report")
	x.Flags = []pslib.AssignmentFlag{pslib.FlagLate, pslib.FlagMissing}
	y.Flags = []pslib.AssignmentFlag{pslib.FlagMissing, pslib.FlagLate}
	if assignmentFingerprint(x) != assignmentFingerprint(y) {
		t.Error("flag order changed the fingerprint")
	}
	// A description edit is not a change (it is not in the fingerprint).
	z := sampleAssignment("1", "Lab report")
	z.Description = "edited"
	if assignmentFingerprint(z) != base {
		t.Error("description changed the fingerprint")
	}
}

func assignmentsClient(byCourse map[string][]*pslib.Assignment) *fakeClient {
	return &fakeClient{
		AssignmentsFunc: func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
			return byCourse[g.CourseID], nil
		},
	}
}

var testAssignmentsCfg = AssignmentsConfig{LookbackDays: 30}

// TestProcessAssignments_ChangeDetection walks one assignment through its
// life against a single checkpoint.
func TestProcessAssignments_ChangeDetection(t *testing.T) {
	grades := []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")}
	steps := []struct {
		name       string
		mutate     func(a *pslib.Assignment)
		wantStaged int
		wantTags   map[string]string
	}{
		{
			name:       "new assignment stages",
			mutate:     func(a *pslib.Assignment) {},
			wantStaged: 1,
			wantTags:   map[string]string{"change": "new", "outstanding": "true", "scored": "false", "status": "pending", "missing": "false"},
		},
		{
			name:       "unchanged assignment does not re-emit",
			mutate:     func(a *pslib.Assignment) {},
			wantStaged: 0,
		},
		{
			name: "marked missing re-emits",
			mutate: func(a *pslib.Assignment) {
				a.Status, a.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}
			},
			wantStaged: 1,
			wantTags:   map[string]string{"change": "updated", "missing": "true", "outstanding": "true", "flags": "missing", "status": "missing"},
		},
		{
			name: "scored late re-emits and is no longer outstanding",
			mutate: func(a *pslib.Assignment) {
				a.Status, a.Flags = pslib.StatusGraded, []pslib.AssignmentFlag{pslib.FlagLate}
				a.Score, a.Percentage, a.LetterGrade = floatPtr(7.5), floatPtr(75), "C"
			},
			wantStaged: 1,
			wantTags: map[string]string{
				"change": "updated", "missing": "false", "late": "true", "outstanding": "false", "scored": "true",
				"score": "7.5", "max_score": "10", "percent": "75", "letter_grade": "C", "flags": "late",
			},
		},
		{
			name: "same score again does not re-emit",
			mutate: func(a *pslib.Assignment) {
				a.Status, a.Flags = pslib.StatusGraded, []pslib.AssignmentFlag{pslib.FlagLate}
				a.Score, a.Percentage, a.LetterGrade = floatPtr(7.5), floatPtr(75), "C"
			},
			wantStaged: 0,
		},
		{
			name: "changed score re-emits",
			mutate: func(a *pslib.Assignment) {
				a.Status, a.Flags = pslib.StatusGraded, []pslib.AssignmentFlag{pslib.FlagLate}
				a.Score, a.Percentage, a.LetterGrade = floatPtr(9), floatPtr(90), "A-"
			},
			wantStaged: 1,
			wantTags:   map[string]string{"change": "updated", "score": "9", "percent": "90", "letter_grade": "A-"},
		},
	}

	env, _ := newTestEnv(t, &fakeClient{})
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			a := sampleAssignment("5001", "Lab report")
			step.mutate(a)
			stepEnv, dir := newTestEnv(t, assignmentsClient(map[string][]*pslib.Assignment{"00100": {a, nil}}))
			stepEnv.CP = env.CP

			res := ProcessAssignments(context.Background(), stepEnv, testKid, grades, testAssignmentsCfg)
			if res.Staged != step.wantStaged || res.Failed != 0 || res.AuthErr != nil {
				t.Fatalf("result = %+v, want Staged=%d", res, step.wantStaged)
			}
			items := readItems(t, dir)
			if len(items) != step.wantStaged {
				t.Fatalf("staged %d items, want %d", len(items), step.wantStaged)
			}
			if step.wantStaged == 0 {
				return
			}
			for k, v := range step.wantTags {
				if items[0].Meta.Tags[k] != v {
					t.Errorf("tag %s = %q, want %q", k, items[0].Meta.Tags[k], v)
				}
			}
		})
	}
}

func TestProcessAssignments_ItemShape(t *testing.T) {
	a := sampleAssignment("5001", "Lab report")
	a.Description = "Write up the density lab."
	env, dir := newTestEnv(t, assignmentsClient(map[string][]*pslib.Assignment{"00100": {a}}))
	grades := []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")}

	ProcessAssignments(context.Background(), env, testKid, grades, testAssignmentsCfg)
	items := readItems(t, dir)
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1", len(items))
	}
	meta := items[0].Meta
	if meta.Subject != "Lab report" || meta.Sender != "Example Science 7" || meta.DataSubject != "e_333333" {
		t.Errorf("meta = %+v", meta)
	}
	want := map[string]string{
		"kid": "k1", "surface": "assignment", "course": "Example Science 7", "category": "Classwork",
		"assignment_id": "5001", "max_score": "10",
		// A date-only due date is the start of that day in the
		// connector's timezone, not UTC midnight (which is the previous
		// evening in Seattle).
		"due_day": "2026-10-08", "due_date": "2026-10-08T00:00:00-07:00",
		"missing": "false", "late": "false", "incomplete": "false",
		"exempt": "false", "collected": "false", "absent": "false",
	}
	for k, v := range want {
		if meta.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, meta.Tags[k], v)
		}
	}
	for _, absent := range []string{"score", "percent", "letter_grade", "flags"} {
		if _, ok := meta.Tags[absent]; ok {
			t.Errorf("tag %s present, want absent", absent)
		}
	}
	if len(meta.Tags) > 32 {
		t.Errorf("%d tags exceeds the staging limit of 32", len(meta.Tags))
	}
	wantBody := "Assignment: Lab report\nCourse: Example Science 7\nCategory: Classwork\nDue: 2026-10-08\n" +
		"Status: pending\nPoints possible: 10\n\nWrite up the density lab.\n"
	if items[0].Body != wantBody {
		t.Errorf("body = %q, want %q", items[0].Body, wantBody)
	}
}

// TestProcessAssignments_Lookback covers the first-sight backlog rule and
// the promise that a baselined assignment still stages when it changes.
func TestProcessAssignments_Lookback(t *testing.T) {
	old := func(id, title string) *pslib.Assignment {
		a := sampleAssignment(id, title)
		a.DueDate = due(2026, 9, 1) // 36 days before testNow
		a.Score, a.Status = floatPtr(10), pslib.StatusGraded
		return a
	}
	oldMissing := old("3", "Old and missing")
	oldMissing.Score, oldMissing.Status = nil, pslib.StatusMissing
	oldMissing.Flags = []pslib.AssignmentFlag{pslib.FlagMissing}
	edge := old("4", "Due on the cutoff day")
	edge.DueDate = due(2026, 9, 7) // exactly 30 days before
	undated := sampleAssignment("5", "Undated")
	undated.DueDate = time.Time{}

	grades := []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")}
	env, dir := newTestEnv(t, assignmentsClient(map[string][]*pslib.Assignment{
		"00100": {old("1", "Old and graded"), sampleAssignment("2", "Recent"), oldMissing, edge, undated},
	}))

	res := ProcessAssignments(context.Background(), env, testKid, grades, testAssignmentsCfg)
	if res.Staged != 4 {
		t.Fatalf("staged = %d, want 4 (all but the old graded one)", res.Staged)
	}
	for _, it := range readItems(t, dir) {
		if it.Meta.Subject == "Old and graded" {
			t.Error("old graded assignment was staged on first sight")
		}
	}

	// The old graded assignment was baselined; a re-score now stages it.
	rescored := old("1", "Old and graded")
	rescored.Score = floatPtr(6)
	env2, dir2 := newTestEnv(t, assignmentsClient(map[string][]*pslib.Assignment{"00100": {rescored}}))
	env2.CP = env.CP
	if res := ProcessAssignments(context.Background(), env2, testKid, grades, testAssignmentsCfg); res.Staged != 1 {
		t.Fatalf("re-scored old assignment: staged = %d, want 1", res.Staged)
	}
	if items := readItems(t, dir2); items[0].Meta.Tags["change"] != "updated" || items[0].Meta.Tags["score"] != "6" {
		t.Errorf("tags = %v", items[0].Meta.Tags)
	}
}

func TestProcessAssignments_Courses(t *testing.T) {
	noLink := sampleGrade("", "Example Advisory", "")
	noLink.ScoresURL = ""
	grades := []*pslib.Grade{
		sampleGrade("00100", "Example Science 7", "B"),
		noLink,
		nil,
		sampleGrade("00200", "Example Math 7", "A"),
		sampleGrade("00300", "Example Art", "A"),
	}
	client := &fakeClient{
		AssignmentsFunc: func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
			switch g.CourseID {
			case "00200":
				return nil, errors.New("lookup returned HTML")
			case "00300":
				// One assignment shared by two sections: same id twice.
				return []*pslib.Assignment{sampleAssignment("7", "Shared A"), sampleAssignment("7", "Shared B")}, nil
			}
			return []*pslib.Assignment{sampleAssignment("7", "Science seven")}, nil
		},
	}
	env, dir := newTestEnv(t, client)

	res := ProcessAssignments(context.Background(), env, testKid, grades, testAssignmentsCfg)
	// Same assignment id in two courses and twice in one course: three
	// distinct records. The failed course is skipped, not fatal.
	if res.Staged != 3 || res.Failed != 1 || res.AuthErr != nil {
		t.Fatalf("result = %+v, want Staged=3 Failed=1", res)
	}
	if got := len(readItems(t, dir)); got != 3 {
		t.Errorf("staged %d items, want 3", got)
	}
	want := []string{"111111/00100", "111111/00200", "111111/00300"}
	if len(client.AssignmentsCalls) != len(want) {
		t.Fatalf("calls = %v, want %v", client.AssignmentsCalls, want)
	}
	for i := range want {
		if client.AssignmentsCalls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, client.AssignmentsCalls[i], want[i])
		}
	}
}

func TestProcessAssignments_AuthErrorStopsSurface(t *testing.T) {
	client := &fakeClient{
		AssignmentsFunc: func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
			return nil, pslib.ErrSessionExpired
		},
	}
	env, dir := newTestEnv(t, client)
	grades := []*pslib.Grade{
		sampleGrade("00100", "Example Science 7", "B"),
		sampleGrade("00200", "Example Math 7", "A"),
	}

	res := ProcessAssignments(context.Background(), env, testKid, grades, testAssignmentsCfg)
	if !errors.Is(res.AuthErr, pslib.ErrSessionExpired) {
		t.Fatalf("AuthErr = %v, want session expired", res.AuthErr)
	}
	if len(client.AssignmentsCalls) != 1 {
		t.Errorf("made %d lookups after an auth failure, want 1", len(client.AssignmentsCalls))
	}
	if got := len(readItems(t, dir)); got != 0 {
		t.Errorf("staged %d items, want 0", got)
	}
}

func TestProcessAssignments_CourseDelayHonoursCancel(t *testing.T) {
	env, _ := newTestEnv(t, assignmentsClient(nil))
	grades := []*pslib.Grade{
		sampleGrade("00100", "Example Science 7", "B"),
		sampleGrade("00200", "Example Math 7", "A"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		ProcessAssignments(ctx, env, testKid, grades, AssignmentsConfig{LookbackDays: 30, CourseDelayMS: 60000})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessAssignments did not return after ctx cancel during the course delay")
	}
}
