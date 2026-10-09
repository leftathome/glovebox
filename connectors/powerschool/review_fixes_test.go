package powerschool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// Review fixes for the three page surfaces. Everything here is synthetic.

// --- C1: teacher comments are for the guardians only (allowlist) --------

// rulesWithout returns cfg's rules minus the teacher_comment ones.
func rulesWithout(cfg Config, suffix string) []connector.Rule {
	var out []connector.Rule
	for _, r := range cfg.Rules {
		if !strings.HasSuffix(r.Match, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func commentRule(kid, subject string, audience ...string) connector.Rule {
	return connector.Rule{Match: "powerschool:" + kid + ":teacher_comment", Destination: "school", DataSubject: subject, Audience: audience}
}

// TestValidateConfig_TeacherCommentAudienceIsAnAllowlist: the only
// acceptable audience for a rule that can match a teacher_comment key is
// exactly ["guardians"]. Every other token -- alone or next to guardians --
// is a config error, whether or not the surface is enabled.
func TestValidateConfig_TeacherCommentAudienceIsAnAllowlist(t *testing.T) {
	cases := []struct {
		name     string
		audience []string
		ok       bool
	}{
		{"guardians", []string{"guardians"}, true},
		{"guardians twice", []string{"guardians", "guardians"}, true},
		{"subject", []string{"subject"}, false},
		{"household", []string{"household"}, false},
		{"public", []string{"public"}, false},
		{"siblings", []string{"siblings"}, false},
		{"caregivers", []string{"caregivers"}, false},
		{"an unknown token", []string{"teachers"}, false},
		{"guardians and subject", []string{"guardians", "subject"}, false},
		{"guardians and household", []string{"guardians", "household"}, false},
		{"guardians and public", []string{"guardians", "public"}, false},
		{"guardians and siblings", []string{"guardians", "siblings"}, false},
		{"guardians and caregivers", []string{"caregivers", "guardians"}, false},
		{"a differently cased token is not the allowed one", []string{"Guardians"}, false},
		{"a padded token is not the allowed one", []string{" guardians"}, false},
		{"an empty token", []string{"guardians", ""}, false},
		{"unset", nil, false},
		{"empty list", []string{}, false},
	}
	for _, enabled := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/surface enabled=%v", tc.name, enabled), func(t *testing.T) {
				cfg := validConfig()
				cfg.Surfaces.TeacherComments = boolPtr(enabled)
				cfg.Rules = append(rulesWithout(cfg, "k2:teacher_comment"), commentRule("k2", "e_444444", tc.audience...))
				err := ValidateConfig(&cfg)
				if tc.ok {
					if err != nil {
						t.Fatalf("ValidateConfig: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("audience %q accepted for a teacher_comment rule", tc.audience)
				}
				if !strings.Contains(err.Error(), "powerschool:k2:teacher_comment") || !strings.Contains(err.Error(), "guardians") {
					t.Errorf("error does not name the rule and the allowed audience: %v", err)
				}
			})
		}
	}
}

// TestValidateConfig_TeacherCommentWildcards: a rule that can match a
// teacher_comment key through a wildcard is held to the same allowlist,
// wherever it sits in the list; and a broader rule may not sit in front of
// the narrower one for that key, where first-match-wins would let it win.
func TestValidateConfig_TeacherCommentWildcards(t *testing.T) {
	wild := func(match string, audience ...string) connector.Rule {
		return connector.Rule{Match: match, Destination: "school", DataSubject: "e_333333", Audience: audience}
	}
	base := func() Config {
		cfg := validConfig()
		// Keep only k1 so a single wildcard can stand for "the kid".
		cfg.Kids = cfg.Kids[:1]
		return cfg
	}

	t.Run("wildcard-only configs", func(t *testing.T) {
		for _, match := range []string{"*", "powerschool:*", "powerschool:k1:*"} {
			for name, tc := range map[string]struct {
				audience []string
				ok       bool
			}{
				"guardians only":        {[]string{"guardians"}, true},
				"subject and guardians": {[]string{"subject", "guardians"}, false},
				"public":                {[]string{"public"}, false},
				"unset":                 {nil, false},
			} {
				t.Run(match+"/"+name, func(t *testing.T) {
					cfg := base()
					// No exact teacher_comment rule: only the wildcard can match the key.
					cfg.Rules = append(rulesWithout(cfg, ":teacher_comment"), wild(match, tc.audience...))
					err := ValidateConfig(&cfg)
					if tc.ok && err != nil {
						t.Fatalf("ValidateConfig: %v", err)
					}
					if !tc.ok && (err == nil || !strings.Contains(err.Error(), "teacher_comment")) {
						t.Fatalf("ValidateConfig = %v, want a teacher_comment audience error", err)
					}
				})
			}
		}
	})

	t.Run("both rule orders", func(t *testing.T) {
		exact := commentRule("k1", "e_333333", "guardians")
		broad := wild("*", "subject", "guardians")
		for name, rules := range map[string][]connector.Rule{
			"broad non-compliant rule after the exact one":  {exact, broad},
			"broad non-compliant rule before the exact one": {broad, exact},
		} {
			t.Run(name, func(t *testing.T) {
				cfg := base()
				others := rulesWithout(cfg, ":teacher_comment")
				if rules[0].Match == "*" {
					cfg.Rules = append([]connector.Rule{rules[0]}, append(others, rules[1])...)
				} else {
					cfg.Rules = append(append(others, rules[0]), rules[1])
				}
				if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "teacher_comment") {
					t.Fatalf("ValidateConfig = %v, want a teacher_comment audience error", err)
				}
			})
		}

		// A broad rule that is itself guardians-only is acceptable after
		// the exact rule, and refused in front of it: there it would win,
		// and the exact rule (and its data_subject) would be dead.
		compliantBroad := wild("*", "guardians")
		t.Run("broad compliant rule after the exact one", func(t *testing.T) {
			cfg := base()
			cfg.Rules = append(append(rulesWithout(cfg, ":teacher_comment"), exact), compliantBroad)
			if err := ValidateConfig(&cfg); err != nil {
				t.Fatalf("ValidateConfig: %v", err)
			}
		})
		t.Run("broad compliant rule before the exact one", func(t *testing.T) {
			cfg := base()
			cfg.Rules = append([]connector.Rule{compliantBroad, exact}, rulesWithout(cfg, ":teacher_comment")...)
			if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "teacher_comment") {
				t.Fatalf("ValidateConfig = %v, want an error: the broad rule shadows the teacher_comment rule", err)
			}
		})
	})
}

// --- C2: a dropped and re-enrolled section is two courses ---------------

func TestProcessAttendanceDays_ReEnrolledSectionDoesNotFlipFlop(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	cfg := AttendanceDaysConfig{LookbackDays: 36500}
	// The same course and period twice: enrolled in September and dropped,
	// then enrolled again in October. The page prints both rows, and for a
	// day in the gap each row has its own cell.
	page := &pslib.AttendancePage{
		Student: pageStudentOf("111111"),
		Legend:  attendanceLegend,
		Courses: []*pslib.AttendanceCourse{
			{CourseName: "EXAMPLE SCIENCE 7A", Expression: "1(A)", EnrollDate: day(2026, 9, 2), LeaveDate: day(2026, 9, 25),
				Days: []pslib.AttendanceDay{codeDay(day(2026, 9, 24), "ME")}},
			{CourseName: "EXAMPLE SCIENCE 7A", Expression: "1(A)", EnrollDate: day(2026, 10, 1), LeaveDate: day(2027, 6, 16),
				Days: []pslib.AttendanceDay{codeDay(day(2026, 9, 24), "UA")}},
		},
	}
	ctx := context.Background()
	if res := ProcessAttendanceDays(ctx, env, testKid, page, cfg); res.Staged != 2 {
		t.Fatalf("first pass staged %d, want 2 (one per enrolment)", res.Staged)
	}
	// Nothing changed. With one checkpoint key shared by both rows each
	// pass re-staged one of them as "updated", forever.
	for pass := 2; pass <= 4; pass++ {
		if res := ProcessAttendanceDays(ctx, env, testKid, page, cfg); res.Staged != 0 {
			t.Fatalf("pass %d staged %d item(s) although nothing changed", pass, res.Staged)
		}
	}
	for _, it := range readItems(t, dir) {
		if it.Meta.Tags["change"] != "new" {
			t.Errorf("item %q staged as %q", it.Meta.Subject, it.Meta.Tags["change"])
		}
	}
}

// --- C4: stored grades reordered on the page ----------------------------

func TestProcessTermGrades_ReorderIsNotAChange(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	cfg := TermGradesConfig{LookbackDays: 36500}
	row := func(school string, attempted float64, letter string) *pslib.HistoricalGrade {
		return &pslib.HistoricalGrade{DateCompleted: day(2026, 6, 1), DateCompletedText: "06/2026", GradeLevel: 6, School: school,
			CourseNumber: "ADV6000", CourseName: "ADVISORY", CreditEarned: attempted, CreditAttempted: attempted, LetterGrade: letter, IncludedInGPA: true}
	}
	// One course number stored three times for one month: two schools,
	// and two rows that differ in nothing but the grade.
	a, b, c1, c2 := row("Example ES", 0.25, "P"), row("Example MS", 0.5, "A"), row("Example HS", 1, "B"), row("Example HS", 1, "C")
	ctx := context.Background()

	if res := ProcessTermGrades(ctx, env, testKid, &pslib.GradeHistoryPage{Grades: []*pslib.HistoricalGrade{a, b, c1, c2}}, cfg); res.Staged != 4 {
		t.Fatalf("first pass staged %d, want 4", res.Staged)
	}
	// The same rows in another order (the page is sortable). The two rows
	// told apart by content keep their identity; nothing is staged.
	for _, order := range [][]*pslib.HistoricalGrade{{b, a, c1, c2}, {c1, c2, b, a}, {b, c1, a, c2}} {
		if res := ProcessTermGrades(ctx, env, testKid, &pslib.GradeHistoryPage{Grades: order}, cfg); res.Staged != 0 {
			t.Fatalf("a reordered page staged %d item(s)", res.Staged)
		}
	}
	// A real change to one of them is still seen, as that row's update.
	b2 := row("Example MS", 0.5, "A-")
	if res := ProcessTermGrades(ctx, env, testKid, &pslib.GradeHistoryPage{Grades: []*pslib.HistoricalGrade{b2, a, c1, c2}}, cfg); res.Staged != 1 {
		t.Fatalf("a corrected grade staged %d item(s), want 1", res.Staged)
	}
	updated := 0
	for _, it := range readItems(t, dir) {
		if it.Meta.Tags["change"] == "updated" {
			updated++
			if it.Meta.Tags["school"] != "Example MS" || it.Meta.Tags["grade"] != "A-" || it.Meta.Tags["previous_grade"] != "A" {
				t.Errorf("updated item tags = %v", it.Meta.Tags)
			}
		}
	}
	if updated != 1 {
		t.Errorf("%d updated items, want 1", updated)
	}
}

// --- C3: a refused term is retried --------------------------------------

func TestTurn_RefusedCommentTermIsRetriedNextPoll(t *testing.T) {
	client := extraFixture("111111")
	refuse := true
	inner := client.CommentsFunc
	client.CommentsFunc = func(ctx context.Context, term string) (*pslib.TeacherCommentsPage, error) {
		if term == "Q1" && refuse {
			return nil, pslib.ErrTermMismatch
		}
		return inner(ctx, term)
	}
	cfg := recentConfig()
	cfg.Kids = cfg.Kids[:1]
	cfg.Surfaces.Assignments = boolPtr(false)
	cfg.Surfaces.TermGrades = boolPtr(false)
	cfg.Surfaces.AttendanceDays = boolPtr(false)
	c, cp, dir := newWiredConnector(t, client, cfg)
	captureLogs(t)
	ctx := context.Background()

	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if n := len(itemsOf(readItems(t, dir), teacherCommentSurface)); n != 1 {
		t.Fatalf("first poll staged %d comment items, want 1 (S1; Q1 was refused)", n)
	}

	// Minutes later, not 24 hours: the refused term must be asked for
	// again, and only that one.
	refuse = false
	client.Log = nil
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	var commentCalls []string
	for _, call := range client.Log {
		if strings.HasPrefix(call, "comments") {
			commentCalls = append(commentCalls, call)
		}
	}
	if strings.Join(commentCalls, " ") != "comments:Q1" {
		t.Errorf("second poll comment requests = %v, want only the refused term Q1", commentCalls)
	}
	terms := map[string]bool{}
	for _, it := range itemsOf(readItems(t, dir), teacherCommentSurface) {
		terms[it.Meta.Tags["term"]] = true
	}
	if !terms["Q1"] || !terms["S1"] {
		t.Errorf("comment terms staged = %v, want Q1 and S1", terms)
	}

	// And once read, it is throttled like the rest.
	client.Log = nil
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	for _, call := range client.Log {
		if strings.HasPrefix(call, "comments") {
			t.Errorf("third poll re-read comments (%s) inside the minimum interval", call)
		}
	}
}

// --- C5: teacher comments ship disabled ---------------------------------

func TestSampleConfig_TeacherCommentsShipDisabled(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	ApplyDefaults(&cfg)
	if cfg.TeacherCommentsEnabled() {
		t.Error("the sample config enables teacher_comments; it must stay off until a real non-empty comment has been parsed from a live page")
	}
	var def Config
	ApplyDefaults(&def)
	if def.TeacherCommentsEnabled() || def.AttendanceDaysEnabled() || def.TermGradesEnabled() {
		t.Error("a default config enables one of the page surfaces")
	}
	// The sample still routes the key correctly, for the day it is enabled.
	rule, ok := connector.NewRuleMatcher(cfg.Rules).Match(MatchKey("k1", teacherCommentSurface))
	if !ok || strings.Join(rule.Audience, ",") != "guardians" {
		t.Errorf("sample teacher_comment rule audience = %v, want [guardians]", rule.Audience)
	}
}

// TestStage_TeacherCommentAudienceGuard: defence in depth behind
// ValidateConfig. A teacher_comment item whose resolved audience is not
// exactly ["guardians"] is not staged, is counted, and is logged without
// any of its content.
func TestStage_TeacherCommentAudienceGuard(t *testing.T) {
	const secret = "Zephyrine struggles with fractions."
	for name, audience := range map[string][]string{
		"subject and guardians":  {"subject", "guardians"},
		"household":              {"household"},
		"public":                 {"public"},
		"siblings and guardians": {"guardians", "siblings"},
		"unset":                  nil,
	} {
		t.Run(name, func(t *testing.T) {
			env, dir := newTestEnv(t, &fakeClient{})
			tel, rec := newRecordingTelemetry(t)
			env.Tel = tel
			// A matcher built from rules that never went through
			// ValidateConfig.
			env.Matcher = connector.NewRuleMatcher([]connector.Rule{commentRule("k1", "e_333333", audience...)})
			logs := captureLogs(t)

			res := ProcessTeacherComments(context.Background(), env, testKid,
				commentsFixture("111111", "Q1", true, secret), TeacherCommentsConfig{})
			if res.Staged != 0 || res.Failed == 0 {
				t.Errorf("result = %+v, want nothing staged and a failure counted", res)
			}
			if got := len(readItems(t, dir)); got != 0 {
				t.Fatalf("%d item(s) staged with audience %v", got, audience)
			}
			if _, ok := env.CP.Load(CheckpointKey(teacherCommentSurface, "k1", "SCI7001|1(A):Q1")); ok {
				t.Error("a comment that was not staged was checkpointed")
			}
			if rec.counters()["powerschool_surface_errors_total{error_class=audience_not_guardians_only,kid=k1,surface=teacher_comment}"] != 1 {
				t.Errorf("guard not counted: %v", rec.counters())
			}
			out := logs.String()
			if !strings.Contains(out, "not staged") {
				t.Errorf("guard not logged:\n%s", out)
			}
			for _, leak := range []string{"Zephyrine", "fractions", "Lindqvist"} {
				if strings.Contains(out, leak) {
					t.Errorf("log carries comment content (%s):\n%s", leak, out)
				}
			}
		})
	}

	t.Run("stage itself refuses", func(t *testing.T) {
		env, dir := newTestEnv(t, &fakeClient{})
		opts := itemOptions(env, connector.MatchResult{Destination: "school", DataSubject: "e_333333", Audience: []string{"subject", "guardians"}},
			testKid, teacherCommentSurface, ChangeNew, "EXAMPLE SCIENCE 7A", "Teacher comment", map[string]string{})
		if err := stage(context.Background(), env, teacherCommentSurface, opts, secret); err == nil {
			t.Fatal("stage accepted a teacher comment for an audience other than guardians")
		}
		if got := len(readItems(t, dir)); got != 0 {
			t.Fatalf("%d item(s) staged", got)
		}
		// The same options for another surface are fine.
		if err := stage(context.Background(), env, gradeSurface, opts, "Course: EXAMPLE SCIENCE 7A"); err != nil {
			t.Errorf("stage refused a grade item: %v", err)
		}
	})

	t.Run("guardians only is staged", func(t *testing.T) {
		env, dir := newTestEnv(t, &fakeClient{})
		res := ProcessTeacherComments(context.Background(), env, testKid, commentsFixture("111111", "Q1", true, secret), TeacherCommentsConfig{})
		if res.Staged != 1 || len(readItems(t, dir)) != 1 {
			t.Errorf("result = %+v", res)
		}
	})
}

// TestTurn_CommentTermsAreCapped (C3): however many terms the page's
// selector offers, at most teacher_comments.max_terms are read per kid per
// poll, and the cap is logged once.
func TestTurn_CommentTermsAreCapped(t *testing.T) {
	var offered []string
	for i := 1; i <= 40; i++ {
		offered = append(offered, fmt.Sprintf("T%d", i))
	}
	build := func(max int) (*fakeClient, Config) {
		client := extraFixture("111111")
		client.CommentsFunc = func(ctx context.Context, term string) (*pslib.TeacherCommentsPage, error) {
			p := commentsFixture(client.Selected, term, term != "", "Comment "+term)
			p.Terms = offered
			return p, nil
		}
		cfg := recentConfig()
		cfg.Surfaces.Assignments = boolPtr(false)
		cfg.Surfaces.TermGrades = boolPtr(false)
		cfg.Surfaces.AttendanceDays = boolPtr(false)
		cfg.TeacherComments.MaxTerms = max
		return client, cfg
	}
	termReads := func(client *fakeClient) int {
		n := 0
		for _, call := range client.Log {
			if strings.HasPrefix(call, "comments:T") {
				n++
			}
		}
		return n
	}

	t.Run("default cap of 12, two kids, logged once", func(t *testing.T) {
		client, cfg := build(0)
		var defaults Config
		ApplyDefaults(&defaults)
		if defaults.TeacherComments.MaxTerms != 12 {
			t.Fatalf("default max_terms = %d, want 12", defaults.TeacherComments.MaxTerms)
		}
		cfg.TeacherComments.MaxTerms = defaults.TeacherComments.MaxTerms
		c, cp, _ := newWiredConnector(t, client, cfg)
		logs := captureLogs(t)
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatal(err)
		}
		if got := termReads(client); got != 24 {
			t.Errorf("%d term pages read for two kids, want 12 each", got)
		}
		if got := strings.Count(logs.String(), "teacher_comments.max_terms"); got != 1 {
			t.Errorf("the cap was logged %d times, want once", got)
		}
	})
	t.Run("configured cap", func(t *testing.T) {
		client, cfg := build(3)
		cfg.Kids = cfg.Kids[:1]
		c, cp, _ := newWiredConnector(t, client, cfg)
		captureLogs(t)
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatal(err)
		}
		if got := termReads(client); got != 3 {
			t.Errorf("%d term pages read, want 3", got)
		}
	})
	t.Run("configured terms are capped too", func(t *testing.T) {
		client, cfg := build(2)
		cfg.Kids = cfg.Kids[:1]
		cfg.TeacherComments.Terms = []string{"T1", "T2", "T3", "T4"}
		c, cp, _ := newWiredConnector(t, client, cfg)
		captureLogs(t)
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatal(err)
		}
		if got := termReads(client); got != 2 {
			t.Errorf("%d term pages read, want 2", got)
		}
	})
	t.Run("a negative cap is a config error", func(t *testing.T) {
		cfg := validConfig()
		cfg.TeacherComments.MaxTerms = -1
		if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "max_terms") {
			t.Errorf("ValidateConfig = %v", err)
		}
	})
}
