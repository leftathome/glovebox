package powerschool

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// Synthetic per-student facts. The two fixture students differ in every
// value a test asserts on, so a record staged under the wrong kid is
// visible as the wrong grade, not just the wrong label.
var (
	studentNumbers = map[string]string{"111111": "9000001", "111112": "9000002"}
	studentGrades  = map[string]string{"111111": "B+88", "111112": "A-91"}
	studentAPIIDs  = map[string]string{"111111": "999001", "111112": "999002"}
	kidOf          = map[string]string{"111111": "k1", "111112": "k2"}
	subjectOf      = map[string]string{"k1": "e_333333", "k2": "e_444444"}
)

// homeFixture is the home page as the library returns it: two students,
// the one with id `selected` explicitly marked and enriched, and one course
// row with that student's grade, attributed to that student. selected == ""
// is a page with no marker.
func homeFixture(selected string) *pslib.HomePage {
	students := []*pslib.Student{{ID: "111111", Name: "Student One"}, {ID: "111112", Name: "Student Two"}}
	for _, s := range students {
		if s.ID == selected {
			s.Selected = true
			s.StudentNumber = studentNumbers[s.ID]
		}
	}
	q1 := studentGrades[selected]
	if q1 == "" {
		q1 = "C70"
	}
	g := sampleGrade("00100", "Example Science 7", q1)
	g.Absences = 2
	g.StudentID = selected
	return &pslib.HomePage{Students: students, Grades: []*pslib.Grade{g}}
}

// pollFixture is a client with the portal's selection state: the home page
// shows whichever student is currently selected, a switch moves the
// selection, and each course's assignment lookup returns one missing
// assignment and stamps the grade with the selected student's scores-page
// id.
func pollFixture(selected string) *fakeClient {
	f := &fakeClient{Selected: selected}
	f.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
		return homeFixture(f.Selected), nil
	}
	f.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		g.StudentAPIID = studentAPIIDs[f.Selected]
		a := sampleAssignment("5001", "Lab report")
		a.Status, a.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}
		return []*pslib.Assignment{a}, nil
	}
	return f
}

// newWiredConnector builds a connector for cfg on a real staging writer,
// matcher, metrics and checkpoint, wired the way main.go wires it.
func newWiredConnector(t *testing.T, client PowerSchoolClient, cfg Config) (*PowerSchoolConnector, connector.Checkpoint, string) {
	t.Helper()
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "powerschool")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	return wireConnector(t, client, cfg, w), cp, stagingDir
}

// wireConnector builds and wires a connector on the given backend.
func wireConnector(t *testing.T, client PowerSchoolClient, cfg Config, backend connector.StagingBackend) *PowerSchoolConnector {
	t.Helper()
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	c, err := NewConnector(client, cfg, "v0.0.0-test")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	metrics, err := connector.NewMetrics("powerschool-connector-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	if err := c.Wire(connector.ConnectorContext{
		Backend: backend,
		Matcher: connector.NewRuleMatcher(cfg.Rules),
		Metrics: metrics,
	}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	return c
}

// recordMetrics swaps the connector's telemetry for one whose counters the
// test can read back.
func recordMetrics(t *testing.T, c *PowerSchoolConnector) *metricRecorder {
	t.Helper()
	tel, rec := newRecordingTelemetry(t)
	c.wireMu.Lock()
	c.tel = tel
	c.wireMu.Unlock()
	return rec
}

// testConfig is validConfig (two kids) without the inter-course delay.
func testConfig() Config {
	cfg := validConfig()
	cfg.Assignments.CourseDelayMS = 0
	return cfg
}

// oneKidConfig is testConfig with only k1.
func oneKidConfig() Config {
	cfg := testConfig()
	cfg.Kids = cfg.Kids[:1]
	return cfg
}

// fingerprintKeys are the checkpoint keys one fixture poll writes for kid.
func fingerprintKeys(kid string) []string {
	return []string{
		CheckpointKey(gradeSurface, kid, "00100"),
		CheckpointKey(attendanceSurface, kid, "00100"),
		CheckpointKey(assignmentSurface, kid, "00100:5001"),
	}
}

func assertNoFingerprints(t *testing.T, cp connector.Checkpoint, kids ...string) {
	t.Helper()
	for _, kid := range kids {
		for _, key := range fingerprintKeys(kid) {
			if v, ok := cp.Load(key); ok {
				t.Errorf("checkpoint %q = %q, want it absent", key, v)
			}
		}
	}
}

// assertKidItems checks that exactly the kids in want have items, three
// each (grade, attendance, assignment), and that every item carries its own
// kid's label, data subject and -- for the grade -- that kid's grade.
func assertKidItems(t *testing.T, items []stagedItem, want ...string) {
	t.Helper()
	perKid := map[string]map[string]bool{}
	for _, it := range items {
		kid := it.Meta.Tags["kid"]
		if perKid[kid] == nil {
			perKid[kid] = map[string]bool{}
		}
		perKid[kid][it.Meta.Tags["surface"]] = true
		if it.Meta.DataSubject != subjectOf[kid] {
			t.Errorf("item %q: kid=%q data_subject=%q, want %q", it.Meta.Subject, kid, it.Meta.DataSubject, subjectOf[kid])
		}
		if it.Meta.Tags["surface"] == "grade" {
			var studentID string
			for id, k := range kidOf {
				if k == kid {
					studentID = id
				}
			}
			if it.Meta.Tags["grade"] != studentGrades[studentID] {
				t.Errorf("grade item labelled %s carries grade %q, want %s's own %q",
					kid, it.Meta.Tags["grade"], kid, studentGrades[studentID])
			}
		}
	}
	if len(perKid) != len(want) || len(items) != 3*len(want) {
		t.Errorf("staged %d items for kids %v, want 3 each for %v", len(items), keysOf(perKid), want)
	}
	for _, kid := range want {
		for _, surface := range []string{"grade", "attendance", "assignment"} {
			if !perKid[kid][surface] {
				t.Errorf("no %s item staged for %s", surface, kid)
			}
		}
	}
}

func keysOf(m map[string]map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- every kid, one at a time (review items 1, 4, 9) --------------------

// TestPoll_ReadsEveryKidOneAtATime: the portal switches correctly. Each
// kid is selected, read and staged in turn, under its own label, whichever
// child the session started on.
func TestPoll_ReadsEveryKidOneAtATime(t *testing.T) {
	cases := []struct {
		name         string
		startOn      string
		wantSwitches []string
		wantHome     int
	}{
		// k1: read (already selected), confirm. k2: read, switch, read, confirm.
		{"session starts on the first child", "111111", []string{"111112"}, 5},
		// k1: read, switch, read, confirm. k2: read, switch, read, confirm.
		{"session starts on the second child", "111112", []string{"111111", "111112"}, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture(tc.startOn)
			c, cp, dir := newWiredConnector(t, client, testConfig())
			rec := recordMetrics(t, c)

			if err := c.Poll(context.Background(), cp); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			assertKidItems(t, readItems(t, dir), "k1", "k2")

			if strings.Join(client.SwitchCalls, ",") != strings.Join(tc.wantSwitches, ",") {
				t.Errorf("switches = %v, want %v", client.SwitchCalls, tc.wantSwitches)
			}
			// Never interleaved: all of k1's reads come before any of k2's.
			if strings.Join(client.AssignmentsCalls, ",") != "111111/00100,111112/00100" {
				t.Errorf("assignment lookups = %v, want k1's then k2's", client.AssignmentsCalls)
			}
			if client.HomeCalls != tc.wantHome {
				t.Errorf("home page read %d times, want %d", client.HomeCalls, tc.wantHome)
			}
			for k := range rec.counters() {
				if strings.HasPrefix(k, "powerschool_kids_skipped_total") || strings.HasPrefix(k, "powerschool_surface_errors_total") {
					t.Errorf("unexpected failure metric on a clean poll: %s", k)
				}
			}
		})
	}
}

// TestPoll_PortalIgnoresTheSwitch: the form post changes nothing. The kid
// whose student is already selected is read; the other is skipped with
// nothing staged -- above all, not the first child's records under the
// second child's label.
func TestPoll_PortalIgnoresTheSwitch(t *testing.T) {
	client := pollFixture("111111")
	client.IgnoreSwitch = true
	c, cp, dir := newWiredConnector(t, client, testConfig())
	rec := recordMetrics(t, c)
	logs := captureLogs(t)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	assertKidItems(t, readItems(t, dir), "k1")
	assertNoFingerprints(t, cp, "k2")
	if strings.Join(client.AssignmentsCalls, ",") != "111111/00100" {
		t.Errorf("assignment lookups = %v, want only k1's", client.AssignmentsCalls)
	}
	counters := rec.counters()
	if counters["powerschool_kids_skipped_total{kid=k2,reason=switch_not_confirmed}"] != 1 ||
		counters["powerschool_surface_errors_total{error_class=switch_not_confirmed,kid=k2,surface=home}"] != 1 {
		t.Errorf("switch_not_confirmed not recorded for k2: %v", counters)
	}
	if !strings.Contains(logs.String(), "error_class=switch_not_confirmed") {
		t.Errorf("no switch_not_confirmed log line:\n%s", logs.String())
	}
}

// TestPoll_SelectionFlipsMidRead: while k1's assignment pages are being
// read, the session's selected student changes (a parent using the portal,
// say). Every page k1's turn read may now be the other child's. The
// re-check at the end of the turn sees it and k1's poll is discarded whole;
// k2's own turn then proceeds normally.
func TestPoll_SelectionFlipsMidRead(t *testing.T) {
	client := pollFixture("111111")
	inner := client.AssignmentsFunc
	flipped := false
	client.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		out, err := inner(ctx, studentID, g)
		if !flipped {
			flipped = true
			client.Selected = "111112" // the selection moves under k1's read
		}
		return out, err
	}
	c, cp, dir := newWiredConnector(t, client, testConfig())
	rec := recordMetrics(t, c)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// k1: nothing at all -- not even the grades read before the flip.
	assertKidItems(t, readItems(t, dir), "k2")
	assertNoFingerprints(t, cp, "k1")
	counters := rec.counters()
	if counters["powerschool_kids_skipped_total{kid=k1,reason=selection_mismatch}"] != 1 {
		t.Errorf("selection_mismatch not recorded for k1: %v", counters)
	}
	for k := range counters {
		if strings.HasPrefix(k, "powerschool_items_total") && strings.Contains(k, "kid=k1") {
			t.Errorf("item metric recorded for the discarded kid: %s", k)
		}
	}
	// The connector did not "fix" the selection and carry on for k1.
	if len(client.SwitchCalls) != 0 {
		t.Errorf("switches = %v, want none (k2 was already selected when its turn came)", client.SwitchCalls)
	}

	// Next poll, nothing interferes: k1 stages as new.
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	assertKidItems(t, readItems(t, dir), "k1", "k2")
}

// twoCourses adds a second course to whatever home page the fixture serves.
func twoCourses(f *fakeClient) {
	inner := f.HomeFunc
	f.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
		p, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		second := sampleGrade("00200", "Example Math 7", "A")
		second.StudentID = f.Selected
		p.Grades = append(p.Grades, second)
		return p, nil
	}
}

// TestPoll_ScoresPagesOfTwoStudents: the home page and the first course's
// scores page belong to one student, the second course's to another, and
// the selection marker never moves (so the end-of-turn check alone would
// pass). The per-poll invariant -- every scores page names the same student
// -- discards the kid's poll, grades and attendance included.
func TestPoll_ScoresPagesOfTwoStudents(t *testing.T) {
	client := pollFixture("111111")
	twoCourses(client)
	apiIDs := []string{"999001", "999002"}
	call := 0
	client.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		g.StudentAPIID = apiIDs[call%len(apiIDs)]
		call++
		a := sampleAssignment("5001", "Lab report")
		a.Status, a.Flags = pslib.StatusMissing, []pslib.AssignmentFlag{pslib.FlagMissing}
		return []*pslib.Assignment{a}, nil
	}
	c, cp, dir := newWiredConnector(t, client, oneKidConfig())
	rec := recordMetrics(t, c)
	ctx := context.Background()

	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	if items := readItems(t, dir); len(items) != 0 {
		t.Fatalf("staged %d items from a poll whose pages disagreed, want 0: first subject %q", len(items), items[0].Meta.Subject)
	}
	if len(client.AssignmentsCalls) != 2 {
		t.Errorf("lookups = %v, want both courses read before deciding", client.AssignmentsCalls)
	}
	assertNoFingerprints(t, cp, "k1")
	for _, key := range []string{CheckpointKey(gradeSurface, "k1", "00200"), CheckpointKey(assignmentSurface, "k1", "00200:5001")} {
		if _, ok := cp.Load(key); ok {
			t.Errorf("checkpoint %q written by a discarded poll", key)
		}
	}
	counters := rec.counters()
	if counters["powerschool_kids_skipped_total{kid=k1,reason=selection_mismatch}"] != 1 ||
		counters["powerschool_surface_errors_total{error_class=selection_mismatch,kid=k1,surface=assignment}"] != 1 {
		t.Errorf("selection_mismatch not recorded: %v", counters)
	}

	// Consistent again: everything stages, as new.
	apiIDs = []string{"999001"}
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("second pollNow: %v", err)
	}
	items := readItems(t, dir)
	if len(items) != 5 { // 2 grades, 1 attendance, 2 assignments
		t.Fatalf("staged %d items after the consistent poll, want 5", len(items))
	}
	for _, it := range items {
		if it.Meta.Tags["change"] != "new" || it.Meta.Tags["kid"] != "k1" {
			t.Errorf("item %q tags = %v, want change=new kid=k1", it.Meta.Subject, it.Meta.Tags)
		}
	}
}

// A scores page that names no student is not evidence either.
func TestPoll_ScoresPageWithoutStudentIDIsDiscarded(t *testing.T) {
	client := pollFixture("111111")
	client.NoAPIID = true
	client.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
		return []*pslib.Assignment{sampleAssignment("5001", "Lab report")}, nil
	}
	c, cp, dir := newWiredConnector(t, client, testConfig())

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := len(readItems(t, dir)); got != 0 {
		t.Errorf("staged %d items, want 0", got)
	}
	assertNoFingerprints(t, cp, "k1", "k2")
}

// TestPoll_NoSelectionMarkerSkipsEveryKid: marker absent. Nothing is
// posted (a switch could not be confirmed), the poll is skipped for EVERY
// kid, and the portal is read once, not once per kid. That holds when the
// first student carries the page's details too -- what the pre-fix library
// returned, and what the connector used to read as "k1 is selected".
func TestPoll_NoSelectionMarkerSkipsEveryKid(t *testing.T) {
	for _, enrichFirst := range []bool{false, true} {
		name := "nobody enriched"
		if enrichFirst {
			name = "first student enriched without a marker"
		}
		t.Run(name, func(t *testing.T) {
			client := pollFixture("")
			client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
				p := homeFixture("")
				if enrichFirst {
					p.Students[0].StudentNumber = "9000002" // the OTHER child's number
				}
				return p, nil
			}
			c, cp, dir := newWiredConnector(t, client, testConfig())
			rec := recordMetrics(t, c)
			logs := captureLogs(t)

			if err := c.Poll(context.Background(), cp); err != nil {
				t.Fatalf("Poll returned %v, want nil (transient)", err)
			}
			if got := len(readItems(t, dir)); got != 0 {
				t.Errorf("staged %d items, want 0", got)
			}
			if len(client.AssignmentsCalls) != 0 || len(client.SwitchCalls) != 0 {
				t.Errorf("lookups %v / switches %v without a verified student", client.AssignmentsCalls, client.SwitchCalls)
			}
			if client.HomeCalls != 1 {
				t.Errorf("home page read %d times, want 1 (the remaining kids are skipped unread)", client.HomeCalls)
			}
			assertNoFingerprints(t, cp, "k1", "k2")
			rec.want(map[string]int64{
				"powerschool_polls_total{trigger_source=catch_up}":                                    1,
				"powerschool_surface_errors_total{error_class=selection_unknown,kid=k1,surface=home}": 1,
				"powerschool_kids_skipped_total{kid=k1,reason=selection_unknown}":                     1,
				"powerschool_kids_skipped_total{kid=k2,reason=selection_unknown}":                     1,
			})
			if !strings.Contains(logs.String(), "error_class=selection_unknown") {
				t.Errorf("log has no error_class=selection_unknown line:\n%s", logs.String())
			}
		})
	}
}

// TestPoll_FailsClosed covers the other ways a poll can be unable to
// attribute records to a kid. In each, nothing is staged and no scores page
// is read under a kid's label.
func TestPoll_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		home func(f *fakeClient) (*pslib.HomePage, error)
	}{
		{
			name: "home page read fails",
			home: func(f *fakeClient) (*pslib.HomePage, error) {
				return nil, pslib.WrapParseError(errors.New("no students found in HTML"), "students list")
			},
		},
		{
			name: "library returns nothing",
			home: func(f *fakeClient) (*pslib.HomePage, error) { return nil, nil },
		},
		{
			name: "two students are marked selected",
			home: func(f *fakeClient) (*pslib.HomePage, error) {
				p := homeFixture(f.Selected)
				p.Students[0].Selected, p.Students[1].Selected = true, true
				return p, nil
			},
		},
		{
			name: "the grades are attributed to the other student",
			home: func(f *fakeClient) (*pslib.HomePage, error) {
				p := homeFixture(f.Selected)
				for id := range kidOf {
					if id != f.Selected {
						p.Grades[0].StudentID = id
					}
				}
				return p, nil
			},
		},
		{
			name: "the grades are attributed to nobody",
			home: func(f *fakeClient) (*pslib.HomePage, error) {
				p := homeFixture(f.Selected)
				p.Grades[0].StudentID = ""
				return p, nil
			},
		},
		{
			name: "the configured kids are not on the account",
			home: func(f *fakeClient) (*pslib.HomePage, error) {
				g := sampleGrade("00100", "Example Science 7", "B")
				g.StudentID = "222222"
				return &pslib.HomePage{
					Students: []*pslib.Student{{ID: "222222", Name: "Student Four", Selected: true}},
					Grades:   []*pslib.Grade{g},
				}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture("111111")
			client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) { return tc.home(client) }
			c, cp, dir := newWiredConnector(t, client, testConfig())

			if err := c.Poll(context.Background(), cp); err != nil {
				t.Fatalf("Poll returned %v, want nil (transient)", err)
			}
			if got := len(readItems(t, dir)); got != 0 {
				t.Errorf("staged %d items, want 0", got)
			}
			if len(client.AssignmentsCalls) != 0 {
				t.Errorf("assignments looked up without a verified student: %v", client.AssignmentsCalls)
			}
			assertNoFingerprints(t, cp, "k1", "k2")
		})
	}
}

// TestPoll_StudentNumber: the optional per-kid student_number must equal
// the selected student's number parsed from the same page. A mismatch
// skips that kid only.
func TestPoll_StudentNumber(t *testing.T) {
	cases := []struct {
		name       string
		configured string // k1's student_number
		onPage     string // the number the page shows while k1's student is selected
		wantKids   []string
	}{
		{"not configured: the marker alone decides", "", "9000001", []string{"k1", "k2"}},
		{"configured and matching", "9000001", "9000001", []string{"k1", "k2"}},
		{"configured, page shows the other child's number", "9000001", "9000002", []string{"k2"}},
		{"configured, page shows no number", "9000001", "", []string{"k2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture("111111")
			client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
				p := homeFixture(client.Selected)
				if client.Selected == "111111" {
					p.Students[0].StudentNumber = tc.onPage
				}
				return p, nil
			}
			cfg := testConfig()
			cfg.Kids[0].StudentNumber = tc.configured
			c, cp, dir := newWiredConnector(t, client, cfg)
			rec := recordMetrics(t, c)
			logs := captureLogs(t)

			if err := c.Poll(context.Background(), cp); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			assertKidItems(t, readItems(t, dir), tc.wantKids...)
			mismatch := rec.counters()["powerschool_kids_skipped_total{kid=k1,reason=student_number_mismatch}"]
			if len(tc.wantKids) == 2 {
				if mismatch != 0 {
					t.Errorf("mismatch metric = %d on a matching poll", mismatch)
				}
				return
			}
			if mismatch != 1 {
				t.Errorf("student_number_mismatch metric = %d, want 1", mismatch)
			}
			if strings.Join(client.AssignmentsCalls, ",") != "111112/00100" {
				t.Errorf("lookups = %v, want none for the kid whose number did not match", client.AssignmentsCalls)
			}
			assertNoFingerprints(t, cp, "k1")
			if !strings.Contains(logs.String(), "reason=student_number_mismatch") {
				t.Errorf("no student_number_mismatch log line:\n%s", logs.String())
			}
			for _, number := range []string{"9000001", "9000002"} {
				if strings.Contains(logs.String(), number) {
					t.Errorf("log contains student number %s:\n%s", number, logs.String())
				}
			}
		})
	}
}

// --- change detection and checkpointing ---------------------------------

func TestPoll_SecondPollStagesNothingUntilSomethingChanges(t *testing.T) {
	client := pollFixture("111111")
	c, cp, dir := newWiredConnector(t, client, oneKidConfig())
	ctx := context.Background()

	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := len(readItems(t, dir)); got != 3 {
		t.Fatalf("after an unchanged second poll: %d items, want 3", got)
	}

	// The grade moves; only the grade re-emits.
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
		p := homeFixture("111111")
		p.Grades[0].Q1Grade = "A-91"
		return p, nil
	}
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("third poll: %v", err)
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

// TestPoll_StageFailureDoesNotAdvanceCheckpoint: while the staging backend
// refuses items, no fingerprint may be saved -- otherwise the record would
// be recorded as delivered and never staged. Once the backend recovers the
// same records stage, still as "new".
func TestPoll_StageFailureDoesNotAdvanceCheckpoint(t *testing.T) {
	stagingDir := t.TempDir()
	w, err := connector.NewStagingWriter(stagingDir, "powerschool")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	backend := &failingBackend{StagingBackend: w, Fail: true}
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	client := pollFixture("111111")
	c := wireConnector(t, client, oneKidConfig(), backend)
	rec := recordMetrics(t, c)
	ctx := context.Background()

	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("poll with a failing backend returned %v, want nil (transient)", err)
	}
	if backend.Attempts != 3 {
		t.Errorf("backend saw %d NewItem attempts, want 3 (grade, attendance, assignment)", backend.Attempts)
	}
	if got := len(readItems(t, stagingDir)); got != 0 {
		t.Fatalf("%d items on disk although the backend failed", got)
	}
	assertNoFingerprints(t, cp, "k1")
	for k := range rec.counters() {
		if strings.HasPrefix(k, "powerschool_items_total") {
			t.Errorf("item metric recorded for an item that was never staged: %s", k)
		}
	}

	backend.Fail = false
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("poll after recovery: %v", err)
	}
	items := readItems(t, stagingDir)
	if len(items) != 3 {
		t.Fatalf("staged %d items after the backend recovered, want 3", len(items))
	}
	for _, it := range items {
		if it.Meta.Tags["change"] != "new" {
			t.Errorf("item %q change = %q, want new", it.Meta.Subject, it.Meta.Tags["change"])
		}
	}
	for _, key := range fingerprintKeys("k1") {
		if _, ok := cp.Load(key); !ok {
			t.Errorf("checkpoint %q missing after a successful stage", key)
		}
	}
}

func TestPoll_DisabledSurfacesAreNotRead(t *testing.T) {
	client := pollFixture("111111")
	cfg := oneKidConfig()
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
	// Grades and attendance come from the page that confirmed the
	// selection; with nothing read after it there is nothing to re-check.
	if client.HomeCalls != 1 {
		t.Errorf("home page read %d times, want 1", client.HomeCalls)
	}
}

// --- escalation ---------------------------------------------------------

func TestPoll_SessionExpiredIsPermanent(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(c *fakeClient)
	}{
		{"at the home page", func(c *fakeClient) {
			c.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
		}},
		{"at the home page, as the login form", func(c *fakeClient) {
			c.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
				return nil, pslib.NewError(pslib.CodeSessionExpired, "login page returned", pslib.ErrSessionExpired)
			}
		}},
		{"at an assignment lookup", func(c *fakeClient) {
			c.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
				return nil, pslib.NewError(pslib.CodeSessionExpired, "session expired", nil)
			}
		}},
		{"at the end-of-turn confirmation", func(c *fakeClient) {
			inner := c.HomeFunc
			c.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
				if c.HomeCalls > 1 {
					return nil, pslib.ErrSessionExpired
				}
				return inner(ctx)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture("111111")
			tc.breakIt(client)
			c, cp, dir := newWiredConnector(t, client, testConfig())
			rejected := 0
			c.OnSessionRejected = func() { rejected++ }

			err := c.Poll(context.Background(), cp)
			if err == nil || !connector.IsPermanent(err) {
				t.Fatalf("Poll = %v, want a PermanentError", err)
			}
			if rejected != 1 {
				t.Errorf("OnSessionRejected called %d times, want 1", rejected)
			}
			// A session that died mid-poll stages nothing from that kid,
			// and the poll does not go on to the next one.
			if got := len(readItems(t, dir)); got != 0 {
				t.Errorf("staged %d items from a poll that ended in an auth failure", got)
			}
			if len(client.SwitchCalls) != 0 {
				t.Errorf("poll went on to switch students after an auth failure: %v", client.SwitchCalls)
			}
		})
	}
}

func TestPoll_FailureThresholdEscalates(t *testing.T) {
	client := pollFixture("111111")
	failing := true
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
		if failing {
			return nil, pslib.WrapNetworkError(errors.New("connection reset"))
		}
		return homeFixture(client.Selected), nil
	}
	cfg := testConfig()
	cfg.FailureThreshold = 3
	c, cp, _ := newWiredConnector(t, client, cfg)
	rejected := 0
	c.OnSessionRejected = func() { rejected++ }
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
			t.Fatalf("failed poll %d returned %v, want nil below the threshold", i, err)
		}
	}
	// A network failure is not about one kid: one request per poll, not
	// one per kid.
	if client.HomeCalls != 2 {
		t.Errorf("home page requested %d times over 2 failed polls, want 2", client.HomeCalls)
	}
	// A good poll resets the run.
	failing = false
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatalf("good poll: %v", err)
	}
	failing = true
	for i := 1; i <= 2; i++ {
		if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
			t.Fatalf("failed poll %d after reset returned %v", i, err)
		}
	}
	err := c.pollNow(ctx, cp, "scheduled")
	if err == nil || !connector.IsPermanent(err) {
		t.Fatalf("third consecutive failed poll = %v, want a PermanentError", err)
	}
	if rejected != 0 {
		t.Errorf("a non-auth escalation marked the session rejected (%d calls)", rejected)
	}
}

// --- what reaches the log (review item 5) -------------------------------

// leakyErr is what a library or transport failure can look like: a
// *url.Error whose URL carries the query, wrapping text that quotes the
// response. Every identifier in it is made up.
func leakyErr() error {
	return pslib.WrapNetworkError(&url.Error{
		Op:  "Get",
		URL: "https://ps.example.org/guardian/scores.html?frn=00412345678&schoolid=4321&begdate=09/03/2026",
		Err: errors.New("unexpected response: <td>Zephyrine Quillfeather</td> Student ID #: 9000002"),
	})
}

var leakMarkers = []string{
	"frn=", "00412345678", "schoolid", "begdate", "?",
	"Zephyrine", "Quillfeather", "9000002", "ps.example.org", "unexpected response",
}

func assertNoLeak(t *testing.T, what, text string) {
	t.Helper()
	for _, m := range leakMarkers {
		if strings.Contains(text, m) {
			t.Errorf("%s contains %q:\n%s", what, m, text)
		}
	}
}

// TestPoll_ErrorLogsCarryNoPII feeds the poll an error holding a query
// string and a child's name at each place a library error is handled, and
// reads back everything the package logged and returned.
func TestPoll_ErrorLogsCarryNoPII(t *testing.T) {
	cases := []struct {
		name      string
		breakIt   func(c *fakeClient)
		wantClass string
		wantErr   bool
	}{
		{
			name: "home page",
			breakIt: func(c *fakeClient) {
				c.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) { return nil, leakyErr() }
			},
			wantClass: "network",
		},
		{
			name: "assignment lookup",
			breakIt: func(c *fakeClient) {
				c.AssignmentsFunc = func(ctx context.Context, studentID string, g *pslib.Grade) ([]*pslib.Assignment, error) {
					return nil, leakyErr()
				}
			},
			wantClass: "network",
		},
		{
			name: "session rejected, with a leaky cause",
			breakIt: func(c *fakeClient) {
				c.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
					return nil, pslib.NewError(pslib.CodeSessionExpired, "rejected for Zephyrine Quillfeather", leakyErr())
				}
			},
			wantClass: "auth",
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pollFixture("111111")
			tc.breakIt(client)
			c, cp, _ := newWiredConnector(t, client, testConfig())
			logs := captureLogs(t)

			err := c.Poll(context.Background(), cp)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Poll = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				// The framework logs a permanent error verbatim.
				assertNoLeak(t, "returned error", err.Error())
			}
			out := logs.String()
			assertNoLeak(t, "log output", out)
			if !strings.Contains(out, "error_class="+tc.wantClass) {
				t.Errorf("log has no error_class=%s:\n%s", tc.wantClass, out)
			}
			if !strings.Contains(out, "error_detail=") {
				t.Errorf("log has no error_detail:\n%s", out)
			}
		})
	}
}

func TestDescribeErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"leaky url error", leakyErr(), "powerschool-go:NETWORK_ERROR > url:Get > *errors.errorString"},
		{"status", pslib.WrapNetworkError(&pslib.HTTPStatusError{Op: "GET /x?frn=1", StatusCode: 503, BodyLen: 42}), "powerschool-go:NETWORK_ERROR > http:503(42b)"},
		{"sentinel", pslib.ErrSessionExpired, "session_expired"},
		{"connector sentinel", ErrSelectionMismatch, "selection_mismatch"},
		{"hostile code", pslib.NewError(pslib.ErrorCode("X?frn=1 Zephyrine"), "m", nil), "powerschool-go:X_frn_1_Zephyrine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeErr(tc.err)
			if got != tc.want {
				t.Errorf("describeErr = %q, want %q", got, tc.want)
			}
			if strings.ContainsAny(got, "?=/&") {
				t.Errorf("describeErr let through query or path characters: %q", got)
			}
		})
	}
}

// --- wiring -------------------------------------------------------------

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
	// Pin the clock outside the window so the next poll is hours away.
	c.now = func() time.Time { return time.Date(2026, 10, 7, 9, 0, 0, 0, c.tz) }
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
	if got := len(readItems(t, dir)); got != 0 || client.HomeCalls != 0 {
		t.Errorf("Watch polled before its window: %d items, %d home reads", got, client.HomeCalls)
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
