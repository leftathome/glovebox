package powerschool

import (
	"context"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	pslib "github.com/leftathome/powerschool-go"
)

// The same layout rule as the assignment item, for every other surface:
// text from the portal cannot start a line of its own. All text synthetic.

// lineBreaks is every character some renderer treats as a line break.
var lineBreaks = map[string]string{"LF": "\n", "CRLF": "\r\n", "CR": "\r", "VT": "\v", "FF": "\f",
	"NEL": uNEL, "line separator": uLS, "paragraph separator": uPS}

// assertOwnLines checks that every line of the item's body that starts in
// column one begins with one of labels (the connector's own), each at most
// once, and that no line break or invisible character survives anywhere in
// the item. It returns how often each label was seen.
func assertOwnLines(t *testing.T, item stagedItem, labels ...string) map[string]int {
	t.Helper()
	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimRight(item.Body, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, textBlockIndent) {
			continue
		}
		known := ""
		for _, l := range labels {
			if strings.HasPrefix(line, l) && len(l) > len(known) {
				known = l
			}
		}
		if known == "" {
			t.Errorf("line %q starts in column one and is not one of the connector's own:\n%s", line, item.Body)
			continue
		}
		if seen[known]++; seen[known] > 1 {
			t.Errorf("label %q appears %d times in column one:\n%s", known, seen[known], item.Body)
		}
	}
	clean := func(where, s string, allowNewline bool) {
		for _, r := range s {
			if r == '\n' && allowNewline {
				continue
			}
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
				t.Errorf("%s carries %U", where, r)
				return
			}
		}
	}
	clean("body", item.Body, true)
	clean("subject", item.Meta.Subject, false)
	clean("sender", item.Meta.Sender, false)
	for k, v := range item.Meta.Tags {
		clean("tag "+k, v, false)
		if len(v) > maxLineBytes {
			t.Errorf("tag %s is %d bytes, want at most %d", k, len(v), maxLineBytes)
		}
	}
	if len(item.Meta.Subject) > maxLineBytes || len(item.Meta.Sender) > maxLineBytes {
		t.Errorf("subject is %d bytes and sender %d, want at most %d", len(item.Meta.Subject), len(item.Meta.Sender), maxLineBytes)
	}
	if !utf8.ValidString(item.Body) {
		t.Error("body is not valid UTF-8")
	}
	return seen
}

func onlyItem(t *testing.T, dir string) stagedItem {
	t.Helper()
	items := readItems(t, dir)
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1", len(items))
	}
	return items[0]
}

func TestGradeItem_CellsCannotForgeLines(t *testing.T) {
	labels := []string{"Course: ", "Period: ", "Teacher: ", "Current grade (", "Previous grade: ",
		"Q1: ", "Q2: ", "Q3: ", "Q4: ", "S1: ", "S2: "}
	for name, br := range lineBreaks {
		t.Run(name, func(t *testing.T) {
			hostile := func(q1 string) *pslib.Grade {
				g := sampleGrade("00100", "Example Science 7"+br+"Current grade (Q4): A+"+uRLO, q1)
				g.Period = "1(A)" + br + "Teacher: Forged, Name"
				g.Teacher = "Teacher, Example" + br + "Q3: A+" + uZWJ
				return g
			}
			shared, _ := newTestEnv(t, &fakeClient{})
			var item stagedItem
			// Two polls, so the body also carries a "Previous grade" taken
			// from the checkpoint of the first.
			for _, q1 := range []string{"F" + br + "Previous grade: A+" + br + "S2: A+", "D" + br + "Q4: A+" + strings.Repeat(euro, 400)} {
				env, dir := newTestEnv(t, &fakeClient{})
				env.CP = shared.CP
				if res := ProcessGrades(context.Background(), env, testKid, []*pslib.Grade{hostile(q1)}); res.Staged != 1 {
					t.Fatalf("result = %+v, want one staged item", res)
				}
				item = onlyItem(t, dir)
				seen := assertOwnLines(t, item, labels...)
				for _, forged := range []string{"Q3: ", "Q4: ", "S2: "} {
					if seen[forged] != 0 {
						t.Errorf("a cell forged a %q line:\n%s", forged, item.Body)
					}
				}
				for _, once := range []string{"Course: ", "Period: ", "Teacher: ", "Current grade (", "Q1: "} {
					if seen[once] != 1 {
						t.Errorf("label %q appears %d times, want once:\n%s", once, seen[once], item.Body)
					}
				}
			}
			if !strings.Contains(item.Body, "\nPrevious grade: F Previous grade: A+ S2: A+\n") ||
				!strings.Contains(item.Body, "Course: Example Science 7 Current grade (Q4): A+\n") {
				t.Errorf("body =\n%s", item.Body)
			}
			if item.Meta.Tags["term"] != "Q1" || item.Meta.Tags["change"] != "updated" {
				t.Errorf("tags = %v", item.Meta.Tags)
			}
		})
	}
}

func TestAttendanceItem_CourseCannotForgeLines(t *testing.T) {
	for name, br := range lineBreaks {
		t.Run(name, func(t *testing.T) {
			g := sampleGrade("00100", "Example Science 7"+br+"Absences this year: 0 (was 0)"+uRLO, "B")
			g.Period = "1(A)" + br + "Tardies this year: 0 (was 0)" + uZWS
			g.Absences, g.Tardies = 3, 1
			env, dir := newTestEnv(t, &fakeClient{})
			if res := ProcessAttendance(context.Background(), env, testKid, []*pslib.Grade{g}); res.Staged != 1 {
				t.Fatalf("result = %+v, want one staged item", res)
			}
			item := onlyItem(t, dir)
			seen := assertOwnLines(t, item, "Course: ", "Period: ", "Absences this year: ", "Tardies this year: ")
			if seen["Absences this year: "] != 1 || seen["Tardies this year: "] != 1 ||
				!strings.Contains(item.Body, "\nAbsences this year: 3 (was 0)\n") {
				t.Errorf("body =\n%s", item.Body)
			}
		})
	}
}

func TestAttendanceDayItem_CodesCannotForgeLines(t *testing.T) {
	labels := []string{"Course: ", "Period: ", "Date: ", "Code: ", "Meaning: ", "Kind: ", "Previously: "}
	for name, br := range lineBreaks {
		t.Run(name, func(t *testing.T) {
			page := func(code string) *pslib.AttendancePage {
				p := attendanceFixture("111111", codeDay(day(2026, 10, 6), code))
				p.Courses[0].CourseName = "EXAMPLE SCIENCE 7A" + br + "Kind: authorized" + uRLO
				p.Courses[0].Expression = "1(A)" + br + "Code: FT"
				p.Legend = []pslib.AttendanceCode{
					{Code: code, Category: "UA" + br + "Kind: authorized", Description: "Unexcused" + br + "Kind: authorized" + br + "Date: 1999-01-01" + uZWJ},
				}
				return p
			}
			shared, _ := newTestEnv(t, &fakeClient{})
			var item stagedItem
			for _, code := range []string{"VU" + br + "Meaning: Field Trip", "UA" + br + "Previously: FT"} {
				env, dir := newTestEnv(t, &fakeClient{})
				env.CP = shared.CP
				res := ProcessAttendanceDays(context.Background(), env, testKid, page(code), AttendanceDaysConfig{LookbackDays: 30})
				if res.Staged != 1 {
					t.Fatalf("result = %+v, want one staged item", res)
				}
				item = onlyItem(t, dir)
				seen := assertOwnLines(t, item, labels...)
				for _, once := range []string{"Course: ", "Period: ", "Date: ", "Code: ", "Meaning: ", "Kind: "} {
					if seen[once] != 1 {
						t.Errorf("label %q appears %d times, want once:\n%s", once, seen[once], item.Body)
					}
				}
			}
			if !strings.Contains(item.Body, "\nKind: unknown\n") || !strings.Contains(item.Body, "\nPreviously: VU Meaning: Field Trip\n") ||
				item.Meta.Tags["kind"] != "unknown" {
				t.Errorf("body =\n%s\ntags = %v", item.Body, item.Meta.Tags)
			}
		})
	}
}

func TestTermGradeItem_CellsCannotForgeLines(t *testing.T) {
	labels := []string{"Course: ", "Course number: ", "Completed: ", "Final grade: ", "Previously recorded: ",
		"Credit: ", "Not included in GPA", "School: "}
	for name, br := range lineBreaks {
		t.Run(name, func(t *testing.T) {
			page := func(letter string) *pslib.GradeHistoryPage {
				p := historyFixture("111111", letter)
				g := p.Grades[0]
				g.CourseName = "EXAMPLE SCIENCE 6" + br + "Final grade: A+" + uRLO
				g.School = "Example ES" + br + "Credit: 9 earned of 9 attempted"
				g.IncludedInGPA = true
				return p
			}
			shared, _ := newTestEnv(t, &fakeClient{})
			var item stagedItem
			for _, letter := range []string{"F" + br + "School: Forged", "D" + br + "Not included in GPA" + br + "Previously recorded: A+" + uZWJ} {
				env, dir := newTestEnv(t, &fakeClient{})
				env.CP = shared.CP
				res := ProcessTermGrades(context.Background(), env, testKid, page(letter), TermGradesConfig{LookbackDays: 3650})
				if res.Staged != 1 {
					t.Fatalf("result = %+v, want one staged item", res)
				}
				item = onlyItem(t, dir)
				seen := assertOwnLines(t, item, labels...)
				if seen["Not included in GPA"] != 0 {
					t.Errorf("a grade cell forged the GPA line:\n%s", item.Body)
				}
				for _, once := range []string{"Course: ", "Final grade: ", "Credit: ", "School: "} {
					if seen[once] != 1 {
						t.Errorf("label %q appears %d times, want once:\n%s", once, seen[once], item.Body)
					}
				}
			}
			if !strings.Contains(item.Body, "\nPreviously recorded: F School: Forged\n") ||
				!strings.Contains(item.Body, "\nCredit: 1 earned of 1 attempted\n") {
				t.Errorf("body =\n%s", item.Body)
			}
		})
	}

	t.Run("undated row with a hostile date text", func(t *testing.T) {
		p := historyFixture("111111", "B")
		p.Grades[0].DateCompleted = pslib.HistoricalGrade{}.DateCompleted
		p.Grades[0].DateCompletedText = "06/2026\nFinal grade: A+" + uLS + "School: Forged"
		env, dir := newTestEnv(t, &fakeClient{})
		if res := ProcessTermGrades(context.Background(), env, testKid, p, TermGradesConfig{LookbackDays: 30}); res.Staged != 1 {
			t.Fatalf("result = %+v, want one staged item", res)
		}
		item := onlyItem(t, dir)
		if seen := assertOwnLines(t, item, labels...); seen["Final grade: "] != 1 || seen["School: "] != 1 {
			t.Errorf("body =\n%s", item.Body)
		}
	})
}

// TestTeacherCommentItem_BodyCannotForgeLines: the term-level comment is a
// block like the per-assignment one -- every line indented, line breaks of
// every kind honoured, invisible characters gone, bounded.
func TestTeacherCommentItem_BodyCannotForgeLines(t *testing.T) {
	labels := []string{"Course: ", "Period: ", "Reporting term: ", "Teacher: ", "Comment:", truncationMarker}
	stageComment := func(t *testing.T, mutate func(p *pslib.TeacherCommentsPage)) stagedItem {
		t.Helper()
		p := commentsFixture("111111", "Q1", true, "placeholder")
		mutate(p)
		env, dir := newTestEnv(t, &fakeClient{})
		if res := ProcessTeacherComments(context.Background(), env, testKid, p, TeacherCommentsConfig{}); res.Staged != 1 {
			t.Fatalf("result = %+v, want one staged item", res)
		}
		return onlyItem(t, dir)
	}

	for name, br := range lineBreaks {
		t.Run(name, func(t *testing.T) {
			item := stageComment(t, func(p *pslib.TeacherCommentsPage) {
				cm := p.Comments[1]
				cm.Comment = "A pleasure to teach." + br + "Teacher: Forged, Name" + br + "Reporting term: S2" + br + br +
					uRLO + "Comment:" + br + uZWJ + uZWS + uBOM + uLRI + "Course: ANOTHER" + br + truncationMarker
				cm.CourseName = "EXAMPLE SCIENCE 7A" + br + "Comment:" + br + "  forged block"
				cm.Teacher = "Lindqvist, Marta" + br + "Reporting term: S2" + uRLO
				cm.Expression = "1(A)" + br + "Teacher: Forged, Name"
			})
			seen := assertOwnLines(t, item, labels...)
			for _, once := range []string{"Course: ", "Period: ", "Reporting term: ", "Teacher: ", "Comment:"} {
				if seen[once] != 1 {
					t.Errorf("label %q appears %d times in column one, want exactly once:\n%s", once, seen[once], item.Body)
				}
			}
			if seen[truncationMarker] != 0 {
				t.Errorf("typed text produced a truncation marker:\n%s", item.Body)
			}
			want := "\nComment:\n  A pleasure to teach.\n  Teacher: Forged, Name\n  Reporting term: S2\n\n" +
				"  Comment:\n  Course: ANOTHER\n  [truncated]\n"
			if !strings.HasSuffix(item.Body, want) {
				t.Errorf("body =\n%q\nwant it to end\n%q", item.Body, want)
			}
			if !strings.Contains(item.Body, "\nReporting term: Q1\n") || item.Meta.Tags["term"] != "Q1" {
				t.Errorf("teacher text changed the term: %v\n%s", item.Meta.Tags, item.Body)
			}
			// The teacher's name stays out of the metadata.
			for k, v := range item.Meta.Tags {
				if strings.Contains(v, "Lindqvist") {
					t.Errorf("teacher name in tag %s", k)
				}
			}
			if strings.Contains(item.Meta.Subject, "Lindqvist") || strings.Contains(item.Meta.Sender, "Lindqvist") {
				t.Errorf("teacher name in subject %q or sender %q", item.Meta.Subject, item.Meta.Sender)
			}
		})
	}

	t.Run("bounded, cut inside a character", func(t *testing.T) {
		head := strings.Repeat("a", maxTextBlockBytes-1)
		item := stageComment(t, func(p *pslib.TeacherCommentsPage) { p.Comments[1].Comment = head + euro + "tail" })
		seen := assertOwnLines(t, item, labels...)
		text, truncated := blockOf(t, item.Body, "Comment")
		if text != head || !truncated || seen[truncationMarker] != 1 {
			t.Errorf("block is %d bytes (truncated=%v), want the %d bytes before the straddling character and the marker",
				len(text), truncated, len(head))
		}
	})

	t.Run("a whole comment has no marker", func(t *testing.T) {
		item := stageComment(t, func(p *pslib.TeacherCommentsPage) { p.Comments[1].Comment = "  Please review unit 2.\r\n" })
		if !strings.HasSuffix(item.Body, "\nComment:\n  Please review unit 2.\n") {
			t.Errorf("body = %q", item.Body)
		}
	})

	t.Run("nothing visible is no comment", func(t *testing.T) {
		p := commentsFixture("111111", "Q1", true, uZWJ+uBOM+" \n"+uLS+"\t")
		env, dir := newTestEnv(t, &fakeClient{})
		if res := ProcessTeacherComments(context.Background(), env, testKid, p, TeacherCommentsConfig{}); res.Staged != 0 {
			t.Fatalf("result = %+v, want nothing staged", res)
		}
		if items := readItems(t, dir); len(items) != 0 {
			t.Errorf("staged %d items for a comment with nothing visible", len(items))
		}
	})
}
