package powerschool

import (
	"fmt"
	"testing"
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// gridCell is one cell: "-" no meeting, "." unmarked, "" present, anything
// else an attendance code.
func gridCell(date time.Time, c string) pslib.AttendanceDay {
	switch c {
	case "-":
		return pslib.AttendanceDay{Date: date, Kind: pslib.CellNoMeeting, Raw: c}
	case ".":
		return pslib.AttendanceDay{Date: date, Kind: pslib.CellUnmarked, Raw: c}
	case "":
		return pslib.AttendanceDay{Date: date, Kind: pslib.CellPresent}
	}
	return codeDay(date, c)
}

// gridPage is an attendance page with one course row per row; row i has
// one cell per date.
func gridPage(dates []time.Time, rows ...[]string) *pslib.AttendancePage {
	page := &pslib.AttendancePage{Student: pageStudentOf("111111"), Legend: attendanceLegend}
	for i, row := range rows {
		course := &pslib.AttendanceCourse{CourseName: fmt.Sprintf("EXAMPLE COURSE %d", i+1), Expression: fmt.Sprintf("%d(A)", i+1)}
		for j, c := range row {
			course.Days = append(course.Days, gridCell(dates[j], c))
		}
		page.Courses = append(page.Courses, course)
	}
	return page
}

// column is one date's cells as rows of a one-date grid.
func column(cells ...string) [][]string {
	rows := make([][]string, len(cells))
	for i, c := range cells {
		rows[i] = []string{c}
	}
	return rows
}

func TestAttendanceDayTallyAndClassification(t *testing.T) {
	cases := []struct {
		name  string
		cells []string
		want  *dayTally // nil: not a candidate
		class string
	}{
		{"full", []string{"UA", "EA", "UA"}, &dayTally{met: 3, absUnex: 2, absEx: 1, anyUnex: true}, "full_day"},
		{"partial", []string{"UA", "", "", ""}, &dayTally{met: 4, absUnex: 1, anyUnex: true}, "partial_day"},
		{"late or left early", []string{"L", "LE", ""}, &dayTally{met: 3, late: 1, leftEarly: 1, anyUnex: true}, "late_or_left_early"},
		{"excused late only", []string{"EL", ""}, &dayTally{met: 2, late: 1}, "late_or_left_early"},
		{"no meeting does not count", []string{"UA", "-", "UA", "-"}, &dayTally{met: 2, absUnex: 2, anyUnex: true}, "full_day"},
		{"unmarked is not marked", []string{"UA", ".", "UA"}, &dayTally{met: 3, unmarked: 1, absUnex: 2, anyUnex: true}, "full_day"},
		{"authorized only", []string{"FT", ""}, &dayTally{met: 2, auth: 1}, "other"},
		{"unknown code", []string{"ZZ", ""}, &dayTally{met: 2, unknown: 1}, "other"},
		{"mixed excused and unexcused", []string{"EA", "VU"}, &dayTally{met: 2, absUnex: 1, absEx: 1, anyUnex: true}, "full_day"},
		{"late unexcused and excused absence", []string{"UL", "ME", ""}, &dayTally{met: 3, absEx: 1, late: 1, anyUnex: true}, "partial_day"},
		{"left early unexcused", []string{"LU", ""}, &dayTally{met: 2, leftEarly: 1, anyUnex: true}, "late_or_left_early"},
		{"no codes", []string{"", ".", "-"}, nil, ""},
	}
	date := day(2026, 10, 6)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := gridPage([]time.Time{date}, column(tc.cells...)...)
			got := tallyDays(page, defaultDaysConfig())["2026-10-06"]
			if tc.want == nil {
				if got != nil && got.coded {
					t.Fatalf("a day with no codes is a candidate: %+v", got)
				}
				return
			}
			want := *tc.want
			want.date, want.coded = date, true
			if got == nil || *got != want {
				t.Fatalf("tally %+v, want %+v", got, want)
			}
			if c := got.classification(); c != tc.class {
				t.Fatalf("classification %q, want %q", c, tc.class)
			}
		})
	}
}

// A code cell with an empty code counts as met and present.
func TestAttendanceDayEmptyCodeIsPresent(t *testing.T) {
	page := gridPage([]time.Time{day(2026, 10, 6)}, []string{"UA"})
	page.Courses = append(page.Courses, &pslib.AttendanceCourse{CourseName: "X", Days: []pslib.AttendanceDay{
		{Date: day(2026, 10, 6), Kind: pslib.CellCode}}}, nil)
	got := tallyDays(page, defaultDaysConfig())["2026-10-06"]
	if got.met != 2 || got.absUnex != 1 || got.classification() != "partial_day" {
		t.Fatalf("tally %+v", got)
	}
}
