package powerschool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// Every name, course, date and comment below is invented.

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func codeDay(date time.Time, code string) pslib.AttendanceDay {
	return pslib.AttendanceDay{Date: date, Kind: pslib.CellCode, Code: code, Raw: code}
}

// attendanceLegend is the legend as the library parses it from the page:
// the district's code scheme, CODE=CATEGORY-Description.
var attendanceLegend = []pslib.AttendanceCode{
	{Code: "FT", Category: "AUTH", Description: "Field Trip"}, {Code: "IA", Category: "AUTH", Description: "On Campus"},
	{Code: "OA", Category: "AUTH", Description: "Off Campus"}, {Code: "AA", Category: "EA", Description: "Approved Activity"},
	{Code: "EA", Category: "EA", Description: "Excused Absence"}, {Code: "FE", Category: "EA", Description: "Family Emergency"},
	{Code: "ME", Category: "EA", Description: "Medical/Sick"}, {Code: "QE", Category: "EA", Description: "Health Excused"},
	{Code: "SS", Category: "EA", Description: "Suspended"}, {Code: "EL", Category: "EL", Description: "Excused Late"},
	{Code: "LB", Category: "EL", Description: "Late Transport"}, {Code: "LE", Category: "LE", Description: "Left Early Excused"},
	{Code: "LU", Category: "LU", Description: "Left Early Unexcused"}, {Code: "UA", Category: "UA", Description: "Unexcused Absence"},
	{Code: "VU", Category: "UA", Description: "Unexcused Travel"}, {Code: "UL", Category: "UL", Description: "Unexcused Late"},
	{Code: "A", Category: "A", Description: "Absent"}, {Code: "L", Category: "L", Description: "Late"},
}

// pageStudentOf is the evidence a page of the given student carries: the
// id its switcher marks, and the heading name (for people only).
func pageStudentOf(studentID string) pslib.PageStudent {
	return pslib.PageStudent{SelectedID: studentID, HeadingName: studentHeadings[studentID]}
}

// attendanceFixture is a parsed attendance page marking the given student:
// one course, a few days around testNow (2026-10-07).
func attendanceFixture(studentID string, days ...pslib.AttendanceDay) *pslib.AttendancePage {
	if days == nil {
		days = []pslib.AttendanceDay{
			{Date: day(2026, 10, 5), Kind: pslib.CellPresent},
			codeDay(day(2026, 10, 6), "VU"),
			{Date: day(2026, 10, 7), Kind: pslib.CellNoMeeting, Raw: "-"},
			{Date: day(2026, 10, 8), Kind: pslib.CellUnmarked, Raw: "."},
			codeDay(day(2026, 10, 2), "L"),
		}
	}
	return &pslib.AttendancePage{
		Student: pageStudentOf(studentID),
		Legend:  attendanceLegend,
		Courses: []*pslib.AttendanceCourse{{CourseName: "EXAMPLE SCIENCE 7A", Expression: "1(A)", Teacher: "Lindqvist, Marta", Days: days}},
	}
}

func historyFixture(studentID, letter string) *pslib.GradeHistoryPage {
	return &pslib.GradeHistoryPage{Student: pageStudentOf(studentID), Grades: []*pslib.HistoricalGrade{
		{DateCompleted: day(2026, 6, 1), DateCompletedText: "06/2026", GradeLevel: 6, School: "Example ES",
			CourseNumber: "SCI6001", CourseName: "EXAMPLE SCIENCE 6", CreditEarned: 1, CreditAttempted: 1, LetterGrade: letter, IncludedInGPA: true},
	}}
}

func commentsFixture(studentID, term string, confirmed bool, comment string) *pslib.TeacherCommentsPage {
	return &pslib.TeacherCommentsPage{
		Student:       pageStudentOf(studentID),
		Term:          term,
		TermConfirmed: confirmed,
		Terms:         []string{"Q1", "S1"},
		Comments: []*pslib.TeacherComment{
			{Expression: "HR(A)", CourseNumber: "ADV7000", CourseName: "ADVISORY", Teacher: "Okafor, Daniel R", Comment: ""},
			{Expression: "1(A)", CourseNumber: "SCI7001", CourseName: "EXAMPLE SCIENCE 7A", Teacher: "Lindqvist, Marta", Comment: comment},
		},
	}
}

func itemsOf(items []stagedItem, surface string) []stagedItem {
	var out []stagedItem
	for _, it := range items {
		if it.Meta.Tags["surface"] == surface {
			out = append(out, it)
		}
	}
	return out
}

// --- config -------------------------------------------------------------

func allSurfacesConfig() Config {
	cfg := testConfig()
	cfg.Surfaces.AttendanceDays = boolPtr(true)
	cfg.Surfaces.TermGrades = boolPtr(true)
	cfg.Surfaces.TeacherComments = boolPtr(true)
	return cfg
}

func TestExtraSurfaces_OffByDefault(t *testing.T) {
	cfg := validConfig()
	if cfg.AttendanceDaysEnabled() || cfg.TermGradesEnabled() || cfg.TeacherCommentsEnabled() {
		t.Fatal("an extra surface is on without being switched on")
	}
	if cfg.AttendanceDays.LookbackDays != 30 || cfg.TermGrades.LookbackDays != 365 ||
		cfg.TermGrades.MinIntervalHours != 24 || cfg.TeacherComments.MinIntervalHours != 24 ||
		!cfg.TeacherComments.requireTermConfirmation() {
		t.Errorf("defaults = %+v / %+v / %+v", cfg.AttendanceDays, cfg.TermGrades, cfg.TeacherComments)
	}
	// And a poll with the defaults never asks for the extra pages.
	client := pollFixture("111111")
	c, cp, _ := newWiredConnector(t, client, testConfig())
	captureLogs(t)
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	for _, call := range client.Log {
		if call == "attendance" || call == "history" || strings.HasPrefix(call, "comments") {
			t.Errorf("extra page requested with its surface off: %v", client.Log)
		}
	}
}

func TestValidateConfig_ExtraSurfaces(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"all on", func(c *Config) {}, ""},
		{"enabled surface without a rule", func(c *Config) {
			var rules []connector.Rule
			for _, r := range c.Rules {
				if r.Match != "powerschool:k1:attendance_day" {
					rules = append(rules, r)
				}
			}
			c.Rules = rules
		}, `no rule matches "powerschool:k1:attendance_day"`},
		{"negative attendance lookback", func(c *Config) { c.AttendanceDays.LookbackDays = -1 }, "attendance_days.lookback_days"},
		{"negative term grade interval", func(c *Config) { c.TermGrades.MinIntervalHours = -1 }, "term_grades"},
		{"negative comment interval", func(c *Config) { c.TeacherComments.MinIntervalHours = -1 }, "teacher_comments.min_interval_hours"},
		{"terms listed", func(c *Config) { c.TeacherComments.Terms = []string{"Q1", "S1"} }, ""},
		{"malformed term", func(c *Config) { c.TeacherComments.Terms = []string{"Q1&x=1"} }, "teacher_comments.terms[0]"},
		{"duplicate term", func(c *Config) { c.TeacherComments.Terms = []string{"Q1", "q1"} }, "duplicate term"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := allSurfacesConfig()
			tc.mutate(&cfg)
			err := ValidateConfig(&cfg)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("ValidateConfig: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("ValidateConfig = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestSampleConfig_RoutesTeacherCommentsToGuardiansOnly: the shipped sample
// follows the owner's rule, for every kid.
func TestSampleConfig_RoutesTeacherCommentsToGuardiansOnly(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	ApplyDefaults(&cfg)
	if !cfg.AttendanceDaysEnabled() || !cfg.TermGradesEnabled() {
		t.Fatalf("sample config does not switch attendance_days and term_grades on: %+v", cfg.Surfaces)
	}
	matcher := connector.NewRuleMatcher(cfg.Rules)
	subjects := map[string]string{}
	for _, kid := range cfg.Kids {
		for _, surface := range []string{gradeSurface, attendanceSurface, assignmentSurface, attendanceDaySurface, termGradeSurface} {
			rule, ok := matcher.Match(MatchKey(kid.Name, surface))
			if !ok || strings.Join(rule.Audience, ",") != "subject,guardians" {
				t.Errorf("%s %s: audience %v, want [subject guardians]", kid.Name, surface, rule.Audience)
			}
			subjects[kid.Name] = rule.DataSubject
		}
		rule, ok := matcher.Match(MatchKey(kid.Name, teacherCommentSurface))
		if !ok || strings.Join(rule.Audience, ",") != "guardians" {
			t.Errorf("%s teacher_comment: audience %v, want [guardians] only", kid.Name, rule.Audience)
		}
		if rule.DataSubject == "" || rule.DataSubject != subjects[kid.Name] {
			t.Errorf("%s teacher_comment: data_subject %q, want the kid's own %q", kid.Name, rule.DataSubject, subjects[kid.Name])
		}
	}
}

// --- attendance_day -----------------------------------------------------

func TestProcessAttendanceDays(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	tel, rec := newRecordingTelemetry(t)
	env.Tel = tel
	cfg := AttendanceDaysConfig{LookbackDays: 30}
	ctx := context.Background()

	page := attendanceFixture("111111",
		pslib.AttendanceDay{Date: day(2026, 10, 5), Kind: pslib.CellPresent},
		codeDay(day(2026, 10, 6), "VU"),
		pslib.AttendanceDay{Date: day(2026, 10, 7), Kind: pslib.CellNoMeeting, Raw: "-"},
		pslib.AttendanceDay{Date: day(2026, 10, 8), Kind: pslib.CellUnmarked, Raw: "."},
		codeDay(day(2026, 10, 2), "L"),
		codeDay(day(2026, 10, 1), "ZZ"), // not in the legend
		codeDay(day(2026, 9, 30), "FT"), // an authorized activity
		codeDay(day(2026, 8, 20), "ME"), // older than the lookback
		pslib.AttendanceDay{Date: day(2026, 10, 9), Kind: pslib.CellUnmarked, Raw: "."},
	)

	res := ProcessAttendanceDays(ctx, env, testKid, page, cfg)
	if res.Staged != 4 || res.Failed != 0 {
		t.Fatalf("first pass = %+v, want 4 staged (VU, L, ZZ, FT)", res)
	}
	items := readItems(t, dir)
	byDate := map[string]stagedItem{}
	for _, it := range items {
		byDate[it.Meta.Tags["date"]] = it
	}
	vu, ok := byDate["2026-10-06"]
	if !ok {
		t.Fatalf("no item for 2026-10-06; items: %v", byDate)
	}
	wantTags := map[string]string{
		"kid": "k1", "surface": "attendance_day", "change": "new", "course": "EXAMPLE SCIENCE 7A", "period": "1(A)",
		"date": "2026-10-06", "code": "VU", "category": "UA", "code_description": "Unexcused Travel", "kind": "absence_unexcused",
	}
	for k, v := range wantTags {
		if vu.Meta.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, vu.Meta.Tags[k], v)
		}
	}
	if vu.Meta.DataSubject != "e_333333" || strings.Join(vu.Meta.Audience, ",") != "subject,guardians" {
		t.Errorf("routing = %q %v", vu.Meta.DataSubject, vu.Meta.Audience)
	}
	if !strings.Contains(vu.Body, "Code: VU") || !strings.Contains(vu.Body, "Unexcused Travel") || !strings.Contains(vu.Body, "Tuesday") {
		t.Errorf("body = %q", vu.Body)
	}
	if got := byDate["2026-10-02"].Meta.Tags; got["kind"] != "late_unexcused" || got["category"] != "L" {
		t.Errorf("late day tags = %v", got)
	}
	// A code the legend does not list: still staged, kind unknown.
	if zz := byDate["2026-10-01"]; zz.Meta.Tags["kind"] != "unknown" || zz.Meta.Tags["code"] != "ZZ" ||
		zz.Meta.Tags["code_description"] != "" || zz.Meta.Tags["category"] != "" {
		t.Errorf("unlisted code tags = %v, want kind=unknown with no category or description", zz.Meta.Tags)
	}
	// An authorized activity is staged, and tagged so it is not counted
	// as an absence.
	if ft := byDate["2026-09-30"]; ft.Meta.Tags["kind"] != "authorized" || ft.Meta.Tags["category"] != "AUTH" {
		t.Errorf("field trip tags = %v, want kind=authorized", ft.Meta.Tags)
	}
	for _, date := range []string{"2026-10-05", "2026-10-07", "2026-10-08", "2026-10-09", "2026-08-20"} {
		if _, staged := byDate[date]; staged {
			t.Errorf("an item was staged for %s (present / \"-\" / \".\" / before the lookback)", date)
		}
	}
	// The old day was baselined, not forgotten.
	if _, ok := env.CP.Load(CheckpointKey(attendanceDaySurface, "k1", "example science 7a|1(A):2026-08-20")); !ok {
		t.Error("the day before the lookback window was not baselined")
	}
	// "-" and "." are never staged, and they are counted.
	counters := rec.counters()
	if counters["powerschool_attendance_non_event_cells_total{cell=dash,kid=k1}"] != 1 ||
		counters["powerschool_attendance_non_event_cells_total{cell=dot,kid=k1}"] != 2 {
		t.Errorf("non-event cell counters = %v, want dash=1 dot=2", counters)
	}

	// Unchanged: nothing.
	if res := ProcessAttendanceDays(ctx, env, testKid, page, cfg); res.Staged != 0 {
		t.Fatalf("second pass staged %d items, want 0", res.Staged)
	}

	// The unexcused absence becomes excused; the baselined old day changes
	// too; a new absence appears.
	page.Courses[0].Days[1] = codeDay(day(2026, 10, 6), "EA")
	page.Courses[0].Days[7] = codeDay(day(2026, 8, 20), "UA")
	page.Courses[0].Days[0] = codeDay(day(2026, 10, 5), "A")
	if res := ProcessAttendanceDays(ctx, env, testKid, page, cfg); res.Staged != 3 {
		t.Fatalf("third pass = %+v, want 3 staged", res)
	}
	updated := 0
	for _, it := range readItems(t, dir) {
		if it.Meta.Tags["change"] != "updated" {
			continue
		}
		updated++
		switch it.Meta.Tags["date"] {
		case "2026-10-06":
			if it.Meta.Tags["code"] != "EA" || it.Meta.Tags["kind"] != "absence_excused" || it.Meta.Tags["previous_code"] != "VU" || !strings.Contains(it.Body, "Previously: VU") {
				t.Errorf("re-coded day tags = %v body %q", it.Meta.Tags, it.Body)
			}
		case "2026-08-20":
			if it.Meta.Tags["code"] != "UA" || it.Meta.Tags["previous_code"] != "ME" {
				t.Errorf("baselined day that changed: tags = %v", it.Meta.Tags)
			}
		default:
			t.Errorf("unexpected updated item for %s", it.Meta.Tags["date"])
		}
	}
	if updated != 2 {
		t.Errorf("%d updated items, want 2", updated)
	}
}

// TestAttendanceKinds: the default mapping from the legend's categories to
// the coarse kind tag, for every code in the legend as observed, and its
// override from config.
func TestAttendanceKinds(t *testing.T) {
	cfg := validConfig() // ApplyDefaults fills the default mapping
	want := map[string]string{
		"FT": "authorized", "IA": "authorized", "OA": "authorized",
		"AA": "absence_excused", "EA": "absence_excused", "FE": "absence_excused", "ME": "absence_excused",
		"QE": "absence_excused", "SS": "absence_excused",
		"EL": "late_excused", "LB": "late_excused",
		"LE": "left_early_excused", "LU": "left_early_unexcused",
		"UA": "absence_unexcused", "VU": "absence_unexcused", "A": "absence_unexcused",
		"UL": "late_unexcused", "L": "late_unexcused",
	}
	page := &pslib.AttendancePage{Legend: attendanceLegend}
	if len(attendanceLegend) != len(want) {
		t.Fatalf("legend fixture has %d codes, the expectations %d", len(attendanceLegend), len(want))
	}
	for code, kind := range want {
		entry, ok := page.Describe(code)
		if !ok {
			t.Fatalf("legend fixture lacks %s", code)
		}
		if got := cfg.AttendanceDays.kindOf(entry.Category); got != kind {
			t.Errorf("%s (category %s) -> kind %q, want %q", code, entry.Category, got, kind)
		}
	}
	for _, category := range []string{"", "ZZ", "NEW"} {
		if got := cfg.AttendanceDays.kindOf(category); got != "unknown" {
			t.Errorf("category %q -> kind %q, want unknown", category, got)
		}
	}

	t.Run("configurable", func(t *testing.T) {
		custom := AttendanceDaysConfig{Kinds: map[string][]string{"away": {"EA", "ua"}, "trip": {"AUTH"}}}
		for category, kind := range map[string]string{"EA": "away", "UA": "away", "AUTH": "trip", "L": "unknown"} {
			if got := custom.kindOf(category); got != kind {
				t.Errorf("custom: category %s -> %q, want %q", category, got, kind)
			}
		}
	})
	t.Run("validation", func(t *testing.T) {
		for name, kinds := range map[string]map[string][]string{
			"a category under two kinds": {"a": {"EA"}, "b": {"ea"}},
			"empty category":             {"a": {""}},
			"empty kind name":            {"": {"EA"}},
			"kind named unknown":         {"unknown": {"EA"}},
			"kind with a space":          {"not here": {"EA"}},
		} {
			cfg := validConfig()
			cfg.AttendanceDays.Kinds = kinds
			if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "attendance_days.kinds") {
				t.Errorf("%s: ValidateConfig = %v, want an attendance_days.kinds error", name, err)
			}
		}
		cfg := validConfig()
		cfg.AttendanceDays.Kinds = map[string][]string{"away": {"EA", "UA"}}
		if err := ValidateConfig(&cfg); err != nil {
			t.Errorf("a valid custom mapping was refused: %v", err)
		}
	})
}

func TestProcessAttendanceDays_KidsDoNotShareCheckpoints(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	cfg := AttendanceDaysConfig{LookbackDays: 30}
	ProcessAttendanceDays(context.Background(), env, testKid, attendanceFixture("111111"), cfg)
	res := ProcessAttendanceDays(context.Background(), env, Kid{Name: "k2", StudentID: "111112"}, attendanceFixture("111112"), cfg)
	if res.Staged != 2 {
		t.Fatalf("second kid staged %d, want its own 2 items", res.Staged)
	}
	subjects := map[string]int{}
	for _, it := range readItems(t, dir) {
		subjects[it.Meta.Tags["kid"]+"/"+it.Meta.DataSubject]++
	}
	if subjects["k1/e_333333"] != 2 || subjects["k2/e_444444"] != 2 {
		t.Errorf("items per kid/subject = %v", subjects)
	}
}

// --- term_grade ---------------------------------------------------------

func TestProcessTermGrades(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	cfg := TermGradesConfig{LookbackDays: 365}
	ctx := context.Background()

	page := &pslib.GradeHistoryPage{Grades: []*pslib.HistoricalGrade{
		{DateCompleted: day(2026, 6, 1), DateCompletedText: "06/2026", GradeLevel: 6, School: "Example ES",
			CourseNumber: "SCI6001", CourseName: "EXAMPLE SCIENCE 6", CreditEarned: 1, CreditAttempted: 1, LetterGrade: "A-", IncludedInGPA: true},
		{DateCompleted: day(2026, 1, 1), DateCompletedText: "01/2026", GradeLevel: 6, School: "Example ES",
			CourseNumber: "ADV6000", CourseName: "ADVISORY", CreditEarned: 0, CreditAttempted: 0.25, LetterGrade: "P", IncludedInGPA: false},
		{DateCompleted: day(2026, 6, 1), DateCompletedText: "06/2026", GradeLevel: 6, School: "Example ES",
			CourseNumber: "ADV6000", CourseName: "ADVISORY", CreditEarned: 0.25, CreditAttempted: 0.25, LetterGrade: "P", IncludedInGPA: false},
		{DateCompleted: day(2024, 6, 1), DateCompletedText: "06/2024", GradeLevel: 4, School: "Example ES",
			CourseNumber: "SCI4001", CourseName: "EXAMPLE SCIENCE 4", CreditEarned: 1, CreditAttempted: 1, LetterGrade: "B", IncludedInGPA: true},
		{CourseName: "EXAMPLE ART", CourseNumber: "ART6001", LetterGrade: "", DateCompletedText: "06/2026", DateCompleted: day(2026, 6, 1)},
		nil,
	}}

	res := ProcessTermGrades(ctx, env, testKid, page, cfg)
	if res.Staged != 3 || res.Failed != 0 {
		t.Fatalf("first pass = %+v, want 3 staged", res)
	}
	items := readItems(t, dir)
	var science stagedItem
	terms := map[string]int{}
	for _, it := range items {
		terms[it.Meta.Tags["course_number"]+"@"+it.Meta.Tags["term"]]++
		if it.Meta.Tags["course_number"] == "SCI6001" {
			science = it
		}
	}
	// One item per (course, completion month): the advisory course has two.
	if terms["SCI6001@2026-06"] != 1 || terms["ADV6000@2026-01"] != 1 || terms["ADV6000@2026-06"] != 1 || len(terms) != 3 {
		t.Errorf("items by course and term = %v", terms)
	}
	wantTags := map[string]string{
		"kid": "k1", "surface": "term_grade", "change": "new", "course": "EXAMPLE SCIENCE 6", "course_number": "SCI6001",
		"term": "2026-06", "date_completed": "06/2026", "grade": "A-", "credit_earned": "1", "credit_attempted": "1",
		"included_in_gpa": "true", "school": "Example ES", "grade_level": "6",
	}
	for k, v := range wantTags {
		if science.Meta.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, science.Meta.Tags[k], v)
		}
	}
	// The page has no percentages; none is invented.
	if _, has := science.Meta.Tags["percent"]; has {
		t.Errorf("a percent tag was staged: %v", science.Meta.Tags)
	}
	if strings.Join(science.Meta.Audience, ",") != "subject,guardians" || science.Meta.DataSubject != "e_333333" {
		t.Errorf("routing = %q %v", science.Meta.DataSubject, science.Meta.Audience)
	}

	if res := ProcessTermGrades(ctx, env, testKid, page, cfg); res.Staged != 0 {
		t.Fatalf("second pass staged %d, want 0", res.Staged)
	}

	// A stored grade is corrected, and the old baselined one too.
	page.Grades[0].LetterGrade = "A"
	page.Grades[3].LetterGrade = "B+"
	if res := ProcessTermGrades(ctx, env, testKid, page, cfg); res.Staged != 2 {
		t.Fatalf("third pass = %+v, want 2 staged", res)
	}
	for _, it := range readItems(t, dir) {
		if it.Meta.Tags["change"] != "updated" {
			continue
		}
		switch it.Meta.Tags["course_number"] {
		case "SCI6001":
			if it.Meta.Tags["grade"] != "A" || it.Meta.Tags["previous_grade"] != "A-" {
				t.Errorf("corrected grade tags = %v", it.Meta.Tags)
			}
		case "SCI4001":
			if it.Meta.Tags["grade"] != "B+" || it.Meta.Tags["previous_grade"] != "B" {
				t.Errorf("corrected old grade tags = %v", it.Meta.Tags)
			}
		default:
			t.Errorf("unexpected updated item: %v", it.Meta.Tags)
		}
	}
}

// --- teacher_comment ----------------------------------------------------

func TestProcessTeacherComments(t *testing.T) {
	env, dir := newTestEnv(t, &fakeClient{})
	var cfg TeacherCommentsConfig
	ctx := context.Background()

	page := commentsFixture("111111", "Q1", true, "Careful lab work this term.\nPlease review unit 2.")
	res := ProcessTeacherComments(ctx, env, testKid, page, cfg)
	if res.Staged != 1 {
		t.Fatalf("first pass = %+v, want 1 staged (the one non-empty comment)", res)
	}
	items := readItems(t, dir)
	it := items[0]
	wantTags := map[string]string{
		"kid": "k1", "surface": "teacher_comment", "change": "new", "course": "EXAMPLE SCIENCE 7A",
		"course_number": "SCI7001", "period": "1(A)", "term": "Q1", "teacher_present": "true",
	}
	for k, v := range wantTags {
		if it.Meta.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, it.Meta.Tags[k], v)
		}
	}
	// The teacher's name is in the body and in no tag.
	for k, v := range it.Meta.Tags {
		if strings.Contains(v, "Lindqvist") || strings.Contains(v, "Marta") {
			t.Errorf("tag %s carries the teacher's name: %q", k, v)
		}
	}
	if !strings.Contains(it.Body, "Teacher: Lindqvist, Marta") || !strings.Contains(it.Body, "Please review unit 2.") {
		t.Errorf("body = %q", it.Body)
	}
	if strings.Contains(it.Meta.Subject, "Lindqvist") || strings.Contains(it.Meta.Sender, "Lindqvist") {
		t.Errorf("subject/sender carry the teacher's name: %q / %q", it.Meta.Subject, it.Meta.Sender)
	}
	// Parents only.
	if strings.Join(it.Meta.Audience, ",") != "guardians" || it.Meta.DataSubject != "e_333333" {
		t.Errorf("routing = data_subject %q audience %v, want e_333333 / [guardians]", it.Meta.DataSubject, it.Meta.Audience)
	}

	if res := ProcessTeacherComments(ctx, env, testKid, page, cfg); res.Staged != 0 {
		t.Fatalf("second pass staged %d, want 0", res.Staged)
	}

	// The same course's comment for another term is its own item.
	if res := ProcessTeacherComments(ctx, env, testKid, commentsFixture("111111", "S1", true, "A strong semester."), cfg); res.Staged != 1 {
		t.Fatalf("another term staged %d, want 1", res.Staged)
	}
	// An edited comment is staged again.
	page.Comments[1].Comment = "Careful lab work this term."
	if res := ProcessTeacherComments(ctx, env, testKid, page, cfg); res.Staged != 1 {
		t.Fatalf("edited comment staged %d, want 1", res.Staged)
	}
	// A comment that is removed stages nothing.
	page.Comments[1].Comment = "   "
	if res := ProcessTeacherComments(ctx, env, testKid, page, cfg); res.Staged != 0 {
		t.Fatalf("emptied comment staged %d, want 0", res.Staged)
	}
	changes := map[string]int{}
	for _, it := range readItems(t, dir) {
		changes[it.Meta.Tags["term"]+"/"+it.Meta.Tags["change"]]++
	}
	if changes["Q1/new"] != 1 || changes["Q1/updated"] != 1 || changes["S1/new"] != 1 || len(changes) != 3 {
		t.Errorf("items by term/change = %v", changes)
	}
}

// TestProcessTeacherComments_UnconfirmedTerm: the term is half of a
// comment's identity. A page that does not itself say which term it shows
// is not staged -- unless the operator has switched that requirement off.
func TestProcessTeacherComments_UnconfirmedTerm(t *testing.T) {
	ctx := context.Background()
	t.Run("not staged by default", func(t *testing.T) {
		env, dir := newTestEnv(t, &fakeClient{})
		for _, page := range []*pslib.TeacherCommentsPage{
			commentsFixture("111111", "Q1", false, "A comment."),
			commentsFixture("111111", "", false, "A comment."),
			commentsFixture("111111", "", true, "A comment."),
		} {
			if res := ProcessTeacherComments(ctx, env, testKid, page, TeacherCommentsConfig{}); res.Staged != 0 {
				t.Errorf("term %q confirmed=%v staged %d, want 0", page.Term, page.TermConfirmed, res.Staged)
			}
		}
		if got := len(readItems(t, dir)); got != 0 {
			t.Errorf("%d items staged", got)
		}
	})
	t.Run("staged when the requirement is off and a term is known", func(t *testing.T) {
		env, _ := newTestEnv(t, &fakeClient{})
		cfg := TeacherCommentsConfig{RequireTermConfirmation: boolPtr(false)}
		if res := ProcessTeacherComments(ctx, env, testKid, commentsFixture("111111", "Q1", false, "A comment."), cfg); res.Staged != 1 {
			t.Errorf("staged %d, want 1", res.Staged)
		}
		if res := ProcessTeacherComments(ctx, env, testKid, commentsFixture("111111", "", false, "A comment."), cfg); res.Staged != 0 {
			t.Errorf("a page with no term at all staged %d", res.Staged)
		}
	})
}

// --- inside the kid's turn ----------------------------------------------

// extraFixture is pollFixture plus the three extra pages, each rendered --
// like the portal does -- for whichever student is selected at that moment,
// and each marking that student in its own switcher.
func extraFixture(selected string) *fakeClient {
	f := pollFixture(selected)
	f.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
		return attendanceFixture(f.Selected), nil
	}
	f.HistoryFunc = func(ctx context.Context) (*pslib.GradeHistoryPage, error) {
		return historyFixture(f.Selected, map[string]string{"111111": "A-", "111112": "B"}[f.Selected]), nil
	}
	f.CommentsFunc = func(ctx context.Context, term string) (*pslib.TeacherCommentsPage, error) {
		// The bare page shows (and marks) the current term, S1; a
		// requested term is the one the page marks.
		if term == "" {
			term = "S1"
		}
		return commentsFixture(f.Selected, term, true, "Comment for "+kidOf[f.Selected]+" "+term), nil
	}
	return f
}

func pollAllSurfaces(t *testing.T, client *fakeClient, cfg Config) ([]stagedItem, connector.Checkpoint, *metricRecorder, *PowerSchoolConnector) {
	t.Helper()
	c, cp, dir := newWiredConnector(t, client, cfg)
	rec := recordMetrics(t, c)
	captureLogs(t)
	if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
		t.Fatalf("pollNow: %v", err)
	}
	return readItems(t, dir), cp, rec, c
}

// recentConfig is allSurfacesConfig with lookbacks wide enough that the
// fixtures' fixed dates are always inside them, whatever today is.
func recentConfig() Config {
	cfg := allSurfacesConfig()
	cfg.AttendanceDays.LookbackDays = 36500
	cfg.TermGrades.LookbackDays = 36500
	return cfg
}

// TestTurn_ExtraSurfacesForEveryKid: with every surface on, each kid gets
// its own attendance days, stored grades and comments -- and the comments
// go to the guardians only.
func TestTurn_ExtraSurfacesForEveryKid(t *testing.T) {
	client := extraFixture("111111")
	items, _, rec, _ := pollAllSurfaces(t, client, recentConfig())

	for kid, subject := range subjectOf {
		for surface, want := range map[string]int{
			gradeSurface: 1, attendanceSurface: 1, assignmentSurface: 1,
			attendanceDaySurface: 2, termGradeSurface: 1, teacherCommentSurface: 2, // Q1 and S1
		} {
			n := 0
			for _, it := range itemsOf(items, surface) {
				if it.Meta.Tags["kid"] != kid {
					continue
				}
				n++
				if it.Meta.DataSubject != subject {
					t.Errorf("%s %s item has data_subject %q, want %q", kid, surface, it.Meta.DataSubject, subject)
				}
				wantAudience := "subject,guardians"
				if surface == teacherCommentSurface {
					wantAudience = "guardians"
				}
				if got := strings.Join(it.Meta.Audience, ","); got != wantAudience {
					t.Errorf("%s %s item audience = %s, want %s", kid, surface, got, wantAudience)
				}
				if surface == teacherCommentSurface && !strings.Contains(it.Body, "Comment for "+kid+" ") {
					t.Errorf("%s got another kid's comment: %q", kid, it.Body)
				}
			}
			if n != want {
				t.Errorf("%s: %d %s items, want %d", kid, n, surface, want)
			}
		}
	}
	// The other child's stored grade never appears under a kid's label.
	for _, it := range itemsOf(items, termGradeSurface) {
		want := map[string]string{"k1": "A-", "k2": "B"}[it.Meta.Tags["kid"]]
		if it.Meta.Tags["grade"] != want {
			t.Errorf("%s term grade = %q, want its own %q", it.Meta.Tags["kid"], it.Meta.Tags["grade"], want)
		}
	}
	for k := range rec.counters() {
		if strings.HasPrefix(k, "powerschool_kids_skipped_total") || strings.HasPrefix(k, "powerschool_surface_errors_total") {
			t.Errorf("failure metric on a clean poll: %s", k)
		}
	}
}

// TestTurn_Order: where the extra pages sit in a kid's turn. Each carries
// its own selected-student id, so none needs a home-page read around it;
// the turn ends with one selection check as before.
func TestTurn_Order(t *testing.T) {
	client := extraFixture("111111")
	pollAllSurfaces(t, client, func() Config { c := recentConfig(); c.Kids = c.Kids[:1]; return c }())

	want := []string{
		"home", // select: k1 already selected, marker verified
		"history",
		"attendance",
		"comments:", // the bare page: the current term, and which terms exist
		"comments:Q1",
		"scores", "lookup",
		"home", // end-of-turn check
	}
	if got := strings.Join(client.Log, " "); got != strings.Join(want, " ") {
		t.Errorf("calls = %v\n   want %v", client.Log, want)
	}
}

// With the assignments surface off the turn still ends with a selection
// check when an extra page was read -- and does not when none was.
func TestTurn_EndCheckFollowsExtraPages(t *testing.T) {
	cfg := recentConfig()
	cfg.Kids = cfg.Kids[:1]
	cfg.Surfaces.Assignments = boolPtr(false)
	cfg.Surfaces.TeacherComments = boolPtr(false)
	cfg.Surfaces.TermGrades = boolPtr(false)

	client := extraFixture("111111")
	pollAllSurfaces(t, client, cfg)
	if got, want := strings.Join(client.Log, " "), "home attendance home"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}

	cfg.Surfaces.AttendanceDays = boolPtr(false)
	client = extraFixture("111111")
	pollAllSurfaces(t, client, cfg)
	if got, want := strings.Join(client.Log, " "), "home"; got != want {
		t.Errorf("calls with no extra surface = %q, want %q", got, want)
	}
}

// TestTurn_ExtraPageOfAnotherStudentDiscardsTheKid: every extra page says,
// in its own switcher, whose it is. If any of them marks another student --
// or marks nobody -- nothing at all is staged for the kid whose turn it
// was, not its grades either, and the other kid's own turn is unaffected.
// The comparison is on the id: a page headed with the right name but
// marking the other student's id is still refused.
func TestTurn_ExtraPageOfAnotherStudentDiscardsTheKid(t *testing.T) {
	// during reports whether the page is being served in k1's turn.
	cases := []struct {
		name      string
		breakIt   func(f *fakeClient)
		wantClass string
	}{
		{
			name: "attendance page marks the other child",
			breakIt: func(f *fakeClient) {
				f.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
					if f.Selected == "111111" {
						return attendanceFixture("111112"), nil
					}
					return attendanceFixture(f.Selected), nil
				}
			},
			wantClass: "selection_changed",
		},
		{
			name: "grade history page marks the other child",
			breakIt: func(f *fakeClient) {
				inner := f.HistoryFunc
				f.HistoryFunc = func(ctx context.Context) (*pslib.GradeHistoryPage, error) {
					p, err := inner(ctx)
					if f.Selected == "111111" {
						p.Student.SelectedID = "111112"
					}
					return p, err
				}
			},
			wantClass: "selection_changed",
		},
		{
			name: "one term's comments page marks the other child",
			breakIt: func(f *fakeClient) {
				inner := f.CommentsFunc
				f.CommentsFunc = func(ctx context.Context, term string) (*pslib.TeacherCommentsPage, error) {
					p, err := inner(ctx, term)
					if f.Selected == "111111" && term == "Q1" {
						p.Student.SelectedID = "111112"
					}
					return p, err
				}
			},
			wantClass: "selection_changed",
		},
		{
			name: "right name in the heading, other child's id in the switcher",
			breakIt: func(f *fakeClient) {
				f.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
					p := attendanceFixture(f.Selected)
					if f.Selected == "111111" {
						p.Student = pslib.PageStudent{SelectedID: "111112", HeadingName: studentHeadings["111111"]}
					}
					return p, nil
				}
			},
			wantClass: "selection_changed",
		},
		{
			name: "attendance page marks nobody",
			breakIt: func(f *fakeClient) {
				f.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
					p := attendanceFixture(f.Selected)
					if f.Selected == "111111" {
						p.Student.SelectedID = "" // the name alone is not evidence
					}
					return p, nil
				}
			},
			wantClass: "selection_unknown",
		},
		{
			name: "the library reports the page marks nobody",
			breakIt: func(f *fakeClient) {
				inner := f.HistoryFunc
				f.HistoryFunc = func(ctx context.Context) (*pslib.GradeHistoryPage, error) {
					if f.Selected == "111111" {
						return nil, pslib.ErrSelectionUnknown
					}
					return inner(ctx)
				}
			},
			wantClass: "selection_unknown",
		},
		{
			name: "selection moves during the extra pages and stays moved",
			breakIt: func(f *fakeClient) {
				inner := f.HistoryFunc
				moved := false
				f.HistoryFunc = func(ctx context.Context) (*pslib.GradeHistoryPage, error) {
					p, err := inner(ctx) // served for k1 ...
					if !moved {
						moved = true
						f.Selected = "111112" // ... then the session moves
					}
					return p, err
				}
			},
			wantClass: "selection_changed", // the very next page marks the other child
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := extraFixture("111111")
			tc.breakIt(client)
			items, cp, rec, _ := pollAllSurfaces(t, client, recentConfig())

			for _, it := range items {
				if it.Meta.Tags["kid"] == "k1" {
					t.Errorf("k1 item staged from a discarded turn: %s %q", it.Meta.Tags["surface"], it.Meta.Subject)
				}
			}
			assertNoFingerprints(t, cp, "k1")
			for _, surface := range []string{termGradeSurface, teacherCommentSurface} {
				if _, ok := cp.Load(lastReadKey(surface, "k1")); ok {
					t.Errorf("a discarded turn advanced k1's %s last-read time", surface)
				}
			}
			if rec.counters()["powerschool_kids_skipped_total{kid=k1,reason="+tc.wantClass+"}"] != 1 {
				t.Errorf("k1 not recorded as skipped with %s: %v", tc.wantClass, rec.counters())
			}
			// k2's turn is clean and complete.
			for surface, want := range map[string]int{gradeSurface: 1, attendanceDaySurface: 2, termGradeSurface: 1, teacherCommentSurface: 2} {
				n := 0
				for _, it := range itemsOf(items, surface) {
					if it.Meta.Tags["kid"] == "k2" {
						n++
					}
				}
				if n != want {
					t.Errorf("k2: %d %s items, want %d", n, surface, want)
				}
			}
		})
	}
}

// One surface's page failing to parse does not cost the kid its poll.
func TestTurn_ExtraPageErrorSkipsThatSurfaceOnly(t *testing.T) {
	client := extraFixture("111111")
	client.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
		return nil, pslib.WrapParseError(errors.New("no attendance grid"), "attendance page")
	}
	inner := client.CommentsFunc
	client.CommentsFunc = func(ctx context.Context, term string) (*pslib.TeacherCommentsPage, error) {
		if term == "Q1" {
			// The portal answered ?fg=Q1 with another term's page.
			return nil, pslib.ErrTermMismatch
		}
		return inner(ctx, term)
	}
	cfg := recentConfig()
	cfg.Kids = cfg.Kids[:1]
	items, _, rec, _ := pollAllSurfaces(t, client, cfg)

	if n := len(itemsOf(items, attendanceDaySurface)); n != 0 {
		t.Errorf("%d attendance_day items from a page that did not parse", n)
	}
	comments := itemsOf(items, teacherCommentSurface)
	if len(comments) != 1 || comments[0].Meta.Tags["term"] != "S1" {
		t.Errorf("comments = %d items, want S1 only (the Q1 request came back as another term)", len(comments))
	}
	if len(itemsOf(items, gradeSurface)) != 1 || len(itemsOf(items, termGradeSurface)) != 1 {
		t.Errorf("the kid's other surfaces were not staged")
	}
	counters := rec.counters()
	if counters["powerschool_surface_errors_total{error_class=parse,kid=k1,surface=attendance_day}"] != 1 ||
		counters["powerschool_surface_errors_total{error_class=term_mismatch,kid=k1,surface=teacher_comment}"] != 1 {
		t.Errorf("surface errors = %v", counters)
	}
}

func TestTurn_ExtraPageAuthErrorIsPermanent(t *testing.T) {
	client := extraFixture("111111")
	client.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) { return nil, pslib.ErrSessionExpired }
	c, cp, dir := newWiredConnector(t, client, recentConfig())
	captureLogs(t)
	rejected := 0
	c.OnSessionRejected = func(time.Duration) { rejected++ }

	err := c.pollNow(context.Background(), cp, "scheduled")
	if !connector.IsPermanent(err) || rejected != 1 {
		t.Fatalf("pollNow = %v (rejected %d), want a PermanentError and one rejection", err, rejected)
	}
	if got := len(readItems(t, dir)); got != 0 {
		t.Errorf("staged %d items from a turn that ended in an auth failure", got)
	}
}

// TestTurn_ThrottledSurfaces: the grade history and comment pages are read
// at most once per min_interval_hours per kid; attendance every poll.
func TestTurn_ThrottledSurfaces(t *testing.T) {
	client := extraFixture("111111")
	cfg := recentConfig()
	cfg.Kids = cfg.Kids[:1]
	cfg.Surfaces.Assignments = boolPtr(false)
	c, cp, _ := newWiredConnector(t, client, cfg)
	captureLogs(t)
	now := testNow
	// pollNow builds its own Env, whose clock is the wall clock; the
	// throttle is exercised through the phase functions with a pinned one.
	turn := func() string {
		client.Log = nil
		w, _ := connector.NewStagingWriter(t.TempDir(), "powerschool")
		env := Env{Client: client, Writer: w, Matcher: connector.NewRuleMatcher(cfg.Rules), CP: cp, Now: func() time.Time { return now }}
		pages, _, err := c.readExtraPages(context.Background(), env, cfg.Kids[0])
		if err != nil {
			t.Fatalf("readExtraPages: %v", err)
		}
		c.stageExtraPages(context.Background(), env, cfg.Kids[0], pages)
		return strings.Join(client.Log, " ")
	}

	if got, want := turn(), "history attendance comments: comments:Q1"; got != want {
		t.Fatalf("first turn = %q, want %q", got, want)
	}
	now = now.Add(8 * time.Hour)
	if got, want := turn(), "attendance"; got != want {
		t.Errorf("8 hours later = %q, want %q (history and comments not due)", got, want)
	}
	now = now.Add(17 * time.Hour)
	if got, want := turn(), "history attendance comments: comments:Q1"; got != want {
		t.Errorf("25 hours later = %q, want %q", got, want)
	}
}

// A throttled surface whose read failed is tried again at the next poll.
func TestTurn_FailedThrottledReadIsRetried(t *testing.T) {
	client := extraFixture("111111")
	failing := true
	inner := client.HistoryFunc
	client.HistoryFunc = func(ctx context.Context) (*pslib.GradeHistoryPage, error) {
		if failing {
			return nil, pslib.WrapNetworkError(errors.New("reset"))
		}
		return inner(ctx)
	}
	cfg := recentConfig()
	cfg.Kids = cfg.Kids[:1]
	c, cp, dir := newWiredConnector(t, client, cfg)
	captureLogs(t)
	ctx := context.Background()

	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cp.Load(lastReadKey(termGradeSurface, "k1")); ok {
		t.Fatal("a failed read advanced the surface's last-read time")
	}
	failing = false
	if err := c.pollNow(ctx, cp, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if n := len(itemsOf(readItems(t, dir), termGradeSurface)); n != 1 {
		t.Errorf("%d term_grade items after the retry, want 1", n)
	}
	if _, ok := cp.Load(lastReadKey(termGradeSurface, "k1")); !ok {
		t.Error("a successful read did not record the last-read time")
	}
}

func TestCheckPageStudent(t *testing.T) {
	kid := Kid{Name: "k1", StudentID: "111111"}
	cases := []struct {
		name    string
		ps      pslib.PageStudent
		wantErr error
	}{
		{"this kid's id", pslib.PageStudent{SelectedID: "111111"}, nil},
		{"this kid's id, whatever the heading says", pslib.PageStudent{SelectedID: "111111", HeadingName: "Rivera, Ben"}, nil},
		{"another id", pslib.PageStudent{SelectedID: "111112"}, ErrSelectionChanged},
		{"another id under this kid's name", pslib.PageStudent{SelectedID: "111112", HeadingName: "Rivera, Ana"}, ErrSelectionChanged},
		{"no id", pslib.PageStudent{}, ErrSelectionUnknown},
		{"no id, a name is not evidence", pslib.PageStudent{HeadingName: "Rivera, Ana"}, ErrSelectionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkPageStudent(tc.ps, kid); !errors.Is(err, tc.wantErr) {
				t.Errorf("checkPageStudent = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
