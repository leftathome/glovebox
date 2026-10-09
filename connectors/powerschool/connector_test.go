package powerschool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// twoStudents is the account's student list as the library returns it,
// with `selected` enriched the way the library enriches the student the
// portal page is showing.
func twoStudents(selected string) []*pslib.Student {
	students := []*pslib.Student{{ID: "111111", Name: "Student One"}, {ID: "111112", Name: "Student Two"}}
	for _, s := range students {
		if s.ID == selected {
			s.StudentNumber = "9000001"
		}
	}
	return students
}

// pollFixture is a client that serves one course with one grade, two
// absences and one missing assignment for whichever student is selected.
func pollFixture(selected string) *fakeClient {
	return &fakeClient{
		StudentsFunc: func(ctx context.Context) ([]*pslib.Student, error) {
			return twoStudents(selected), nil
		},
		GradesFunc: func(ctx context.Context, studentID string) ([]*pslib.Grade, error) {
			g := sampleGrade("00100", "Example Science 7", "B+88")
			g.Absences = 2
			return []*pslib.Grade{g}, nil
		},
		AssignmentsFunc: func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
			a := sampleAssignment("5001", "Lab report")
			a.Status, a.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}
			return []*pslib.Assignment{a}, nil
		},
	}
}

// newWiredConnector builds a connector for cfg on a real staging writer,
// matcher, metrics and checkpoint, wired the way main.go wires it.
func newWiredConnector(t *testing.T, client PowerSchoolClient, cfg Config) (*PowerSchoolConnector, connector.Checkpoint, string) {
	t.Helper()
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	c, err := NewConnector(client, cfg, "v0.0.0-test")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "powerschool")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	metrics, err := connector.NewMetrics("powerschool-connector-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	if err := c.Wire(connector.ConnectorContext{
		Backend: w,
		Matcher: connector.NewRuleMatcher(cfg.Rules),
		Metrics: metrics,
	}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return c, cp, stagingDir
}

// testConfig is validConfig without the inter-course delay.
func testConfig() Config {
	cfg := validConfig()
	cfg.Assignments.CourseDelayMS = 0
	return cfg
}

func TestPoll_StagesSelectedKidOnly(t *testing.T) {
	client := pollFixture("111111")
	c, cp, dir := newWiredConnector(t, client, testConfig())

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	items := readItems(t, dir)
	if len(items) != 3 {
		t.Fatalf("staged %d items, want 3 (grade, attendance, assignment)", len(items))
	}
	surfaces := map[string]bool{}
	for _, it := range items {
		surfaces[it.Meta.Tags["surface"]] = true
		// Per-child isolation: nothing may carry the unselected kid's
		// label or subject.
		if it.Meta.Tags["kid"] != "k1" || it.Meta.DataSubject != "e_333333" {
			t.Errorf("item %q is labelled kid=%q data_subject=%q, want k1 / e_333333",
				it.Meta.Subject, it.Meta.Tags["kid"], it.Meta.DataSubject)
		}
		if it.Meta.Tier != string(connector.TierPersonal) && it.Meta.Tier != "" {
			t.Errorf("tier = %q", it.Meta.Tier)
		}
	}
	for _, s := range []string{"grade", "attendance", "assignment"} {
		if !surfaces[s] {
			t.Errorf("no %s item staged", s)
		}
	}
	// The unselected kid's student id was never even asked for.
	if len(client.GradesCalls) != 1 || client.GradesCalls[0] != "111111" {
		t.Errorf("GetGrades calls = %v, want [111111]", client.GradesCalls)
	}
	if len(client.AssignmentsCalls) != 1 || client.AssignmentsCalls[0] != "111111/00100" {
		t.Errorf("GetAssignments calls = %v", client.AssignmentsCalls)
	}
}

// TestPoll_OtherKidSelected is the mirror image: when the session shows
// the second student, the records are labelled k2 and k1 stages nothing.
func TestPoll_OtherKidSelected(t *testing.T) {
	client := pollFixture("111112")
	c, cp, dir := newWiredConnector(t, client, testConfig())

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	items := readItems(t, dir)
	if len(items) != 3 {
		t.Fatalf("staged %d items, want 3", len(items))
	}
	for _, it := range items {
		if it.Meta.Tags["kid"] != "k2" || it.Meta.DataSubject != "e_444444" {
			t.Errorf("item %q labelled kid=%q data_subject=%q, want k2 / e_444444",
				it.Meta.Subject, it.Meta.Tags["kid"], it.Meta.DataSubject)
		}
	}
	if len(client.GradesCalls) != 1 || client.GradesCalls[0] != "111112" {
		t.Errorf("GetGrades calls = %v, want [111112]", client.GradesCalls)
	}
}

func TestPoll_SecondPollStagesNothingUntilSomethingChanges(t *testing.T) {
	client := pollFixture("111111")
	c, cp, dir := newWiredConnector(t, client, testConfig())
	ctx := context.Background()

	if err := c.Poll(ctx, cp); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if err := c.Poll(ctx, cp); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if got := len(readItems(t, dir)); got != 3 {
		t.Fatalf("after an unchanged second poll: %d items, want 3", got)
	}

	// The grade moves; only the grade re-emits.
	client.GradesFunc = func(ctx context.Context, studentID string) ([]*pslib.Grade, error) {
		g := sampleGrade("00100", "Example Science 7", "A-91")
		g.Absences = 2
		return []*pslib.Grade{g}, nil
	}
	if err := c.Poll(ctx, cp); err != nil {
		t.Fatalf("third Poll: %v", err)
	}
	items := readItems(t, dir)
	if len(items) != 4 {
		t.Fatalf("after a changed grade: %d items, want 4", len(items))
	}
	updated := 0
	for _, it := range items {
		if it.Meta.Tags["change"] == "updated" {
			updated++
			if it.Meta.Tags["surface"] != "grade" || it.Meta.Tags["previous_grade"] != "B+88" {
				t.Errorf("updated item tags = %v", it.Meta.Tags)
			}
		}
	}
	if updated != 1 {
		t.Errorf("%d updated items, want 1", updated)
	}
}

func TestPoll_DisabledSurfacesAreNotRead(t *testing.T) {
	client := pollFixture("111111")
	cfg := testConfig()
	cfg.Surfaces.Assignments = boolPtr(false)
	cfg.Surfaces.Attendance = boolPtr(false)
	c, cp, dir := newWiredConnector(t, client, cfg)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	items := readItems(t, dir)
	if len(items) != 1 || items[0].Meta.Tags["surface"] != "grade" {
		t.Fatalf("items = %+v, want one grade item", items)
	}
	if len(client.AssignmentsCalls) != 0 {
		t.Errorf("assignments were looked up with the surface off: %v", client.AssignmentsCalls)
	}
}

// TestPoll_FailsClosed covers every way a poll can be unable to attribute
// records to a kid. In each, nothing is staged and no grades are fetched
// under a kid's label.
func TestPoll_FailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		students func(ctx context.Context) ([]*pslib.Student, error)
	}{
		{
			name: "student listing fails",
			students: func(ctx context.Context) ([]*pslib.Student, error) {
				return nil, errors.New("no students found in HTML")
			},
		},
		{
			name: "no student is marked selected",
			students: func(ctx context.Context) ([]*pslib.Student, error) {
				return twoStudents(""), nil
			},
		},
		{
			name: "two students look selected",
			students: func(ctx context.Context) ([]*pslib.Student, error) {
				s := twoStudents("111111")
				s[1].StudentNumber = "9000002"
				return s, nil
			},
		},
		{
			name: "the selected student is not a configured kid",
			students: func(ctx context.Context) ([]*pslib.Student, error) {
				return []*pslib.Student{{ID: "111111"}, {ID: "111112"}, {ID: "111113", StudentNumber: "9000003"}}, nil
			},
		},
		{
			name: "the configured kids are not on the account",
			students: func(ctx context.Context) ([]*pslib.Student, error) {
				return []*pslib.Student{{ID: "222222", StudentNumber: "9000004"}}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture("111111")
			client.StudentsFunc = tc.students
			c, cp, dir := newWiredConnector(t, client, testConfig())

			if err := c.Poll(context.Background(), cp); err != nil {
				t.Fatalf("Poll returned %v, want nil (transient)", err)
			}
			if got := len(readItems(t, dir)); got != 0 {
				t.Errorf("staged %d items, want 0", got)
			}
			if len(client.GradesCalls) != 0 {
				t.Errorf("grades fetched without a verified student: %v", client.GradesCalls)
			}
		})
	}
}

func TestPoll_SessionExpiredIsPermanent(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(c *fakeClient)
	}{
		{"at the student listing", func(c *fakeClient) {
			c.StudentsFunc = func(ctx context.Context) ([]*pslib.Student, error) { return nil, pslib.ErrSessionExpired }
		}},
		{"at the grades page", func(c *fakeClient) {
			c.GradesFunc = func(ctx context.Context, studentID string) ([]*pslib.Grade, error) {
				return nil, pslib.ErrSessionExpired
			}
		}},
		{"at an assignment lookup", func(c *fakeClient) {
			c.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
				return nil, pslib.NewError(pslib.CodeSessionExpired, "session expired", nil)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture("111111")
			tc.breakIt(client)
			c, cp, _ := newWiredConnector(t, client, testConfig())

			err := c.Poll(context.Background(), cp)
			if err == nil || !connector.IsPermanent(err) {
				t.Fatalf("Poll = %v, want a PermanentError", err)
			}
		})
	}
}

func TestPoll_FailureThresholdEscalates(t *testing.T) {
	client := pollFixture("111111")
	failing := true
	client.GradesFunc = func(ctx context.Context, studentID string) ([]*pslib.Grade, error) {
		if failing {
			return nil, pslib.WrapNetworkError(errors.New("connection reset"))
		}
		return []*pslib.Grade{sampleGrade("00100", "Example Science 7", "B")}, nil
	}
	cfg := testConfig()
	cfg.FailureThreshold = 3
	c, cp, _ := newWiredConnector(t, client, cfg)
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		if err := c.Poll(ctx, cp); err != nil {
			t.Fatalf("failed poll %d returned %v, want nil below the threshold", i, err)
		}
	}
	// A good poll resets the run.
	failing = false
	if err := c.Poll(ctx, cp); err != nil {
		t.Fatalf("good poll: %v", err)
	}
	failing = true
	for i := 1; i <= 2; i++ {
		if err := c.Poll(ctx, cp); err != nil {
			t.Fatalf("failed poll %d after reset returned %v", i, err)
		}
	}
	err := c.Poll(ctx, cp)
	if err == nil || !connector.IsPermanent(err) {
		t.Fatalf("third consecutive failed poll = %v, want a PermanentError", err)
	}
}

func TestPoll_NotWired(t *testing.T) {
	c, err := NewConnector(pollFixture("111111"), testConfig(), "v0.0.0-test")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	cp, _ := connector.NewCheckpoint(t.TempDir())
	if err := c.Poll(context.Background(), cp); err == nil {
		t.Error("Poll on an unwired connector returned nil")
	}
}

func TestWire_RequiresFrameworkResources(t *testing.T) {
	c, err := NewConnector(pollFixture("111111"), testConfig(), "v0.0.0-test")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	if err := c.Wire(connector.ConnectorContext{}); err == nil {
		t.Error("Wire accepted an empty ConnectorContext")
	}
}

func TestNewConnector(t *testing.T) {
	if _, err := NewConnector(nil, testConfig(), "v"); err == nil {
		t.Error("nil client accepted")
	}
	t.Setenv("POWERSCHOOL_TIMEZONE", "Not/AZone")
	if _, err := NewConnector(pollFixture("111111"), testConfig(), "v"); err == nil {
		t.Error("unresolvable timezone accepted")
	}
}

// TestWatch_StopsOnContextCancel: Watch must sleep until the next window
// and return promptly when the framework cancels it, without polling.
func TestWatch_StopsOnContextCancel(t *testing.T) {
	client := pollFixture("111111")
	c, cp, dir := newWiredConnector(t, client, testConfig())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, cp) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Watch = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}
	if got := len(readItems(t, dir)); got != 0 {
		t.Errorf("Watch polled before its window: %d items", got)
	}
}

func TestNextPoll_LandsInTheConfiguredWindow(t *testing.T) {
	c, _, _ := newWiredConnector(t, pollFixture("111111"), testConfig())
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, c.tz)
	for i := 0; i < 50; i++ {
		next := c.nextPoll(now)
		minutes := next.Hour()*60 + next.Minute()
		if next.Before(now) || minutes < 16*60+30 || minutes >= 18*60 || next.Day() != 7 {
			t.Fatalf("nextPoll = %s, want 2026-10-07 between 16:30 and 18:00", next)
		}
	}
}

var _ connector.Connector = (*PowerSchoolConnector)(nil)
var _ connector.Watcher = (*PowerSchoolConnector)(nil)
