package powerschool

import (
	"fmt"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
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

// The week of 2026-10-05 (Monday) and the Monday after.
var (
	mon, tue, wed, thu, fri = day(2026, 10, 5), day(2026, 10, 6), day(2026, 10, 7), day(2026, 10, 8), day(2026, 10, 9)
	nextMon                 = day(2026, 10, 12)
)

// at is an evening of the given school day in the school's time zone.
func at(t *testing.T, d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 20, 0, 0, 0, testTZ(t))
}

func buildDays(t *testing.T, page *pslib.AttendancePage, settle int, today time.Time) []journalapi.Snapshot {
	t.Helper()
	snaps := BuildAttendanceDays(bind1, page, defaultDaysConfig(), settle, at(t, today), testTZ(t), obsAt)
	validateAll(t, snaps)
	return snaps
}

func TestDayWithUnmarkedCellIsNotSettledUntilThreeSchoolDays(t *testing.T) {
	// Mon has UA in period 1 and "." in period 2.
	page := gridPage([]time.Time{mon, tue, wed, thu}, []string{"UA", "", "", ""}, []string{".", "", "", ""})
	// Today Tue: one school day has followed Mon. Nothing is settled.
	if snaps := buildDays(t, page, 3, tue); len(snaps) != 0 {
		t.Fatalf("Tue: %d snapshots, range %+v", len(snaps), snaps[0].CompleteRange)
	}
	// Today Wed: two. Still nothing.
	if snaps := buildDays(t, page, 3, wed); len(snaps) != 0 {
		t.Fatalf("Wed: %d snapshots", len(snaps))
	}
	// Today Thu: Tue, Wed and Thu followed: Mon settled, over its marked
	// period only -> full_day. Range Mon..Wed (Thu is today).
	snaps := buildDays(t, page, 3, thu)
	if len(snaps) != 1 {
		t.Fatalf("Thu: %d snapshots", len(snaps))
	}
	wantScope(t, snaps[0], "powerschool", "attendance_days", "e_child1", "school_year", "2026-27")
	wantRange(t, snaps[0], "2026-10-05", "2026-10-07")
	wantJSON(t, fieldsOf(t, snaps[0], "2026-10-05"), map[string]string{"date": `"2026-10-05"`, "periods_met": "2",
		"periods_unmarked": "1", "absent_unexcused": "1", "absent_excused": "0", "late": "0", "left_early": "0",
		"authorized": "0", "unknown_codes": "0", "any_unexcused": "true", "classification": `"full_day"`})
}

func TestDayWithoutUnmarkedCellsSettlesTheNextDay(t *testing.T) {
	page := gridPage([]time.Time{mon, tue}, []string{"UA", "EA"}, []string{"", "EA"})
	snaps := buildDays(t, page, 3, tue)
	if len(snaps) != 1 || len(snaps[0].Records) != 1 {
		t.Fatalf("snapshots %+v", snaps)
	}
	// Today (Tue) is never settled, whatever it holds.
	wantRange(t, snaps[0], "2026-10-05", "2026-10-05")
	wantJSON(t, fieldsOf(t, snaps[0], "2026-10-05"), map[string]string{"classification": `"partial_day"`})
}

func TestNoMeetingDaysAreNotSchoolDays(t *testing.T) {
	// Tue is a holiday column of "-": Mon is followed by Wed and Thu only.
	page := gridPage([]time.Time{mon, tue, wed, thu, fri}, []string{"UA", "-", "", "", ""}, []string{".", "-", "", "", ""})
	if snaps := buildDays(t, page, 3, thu); len(snaps) != 0 {
		t.Fatalf("Mon settled after two school days: %+v", snaps[0].CompleteRange)
	}
	snaps := buildDays(t, page, 3, fri)
	if len(snaps) != 1 {
		t.Fatalf("Fri: %d snapshots", len(snaps))
	}
}

func TestSettleIsConfigurable(t *testing.T) {
	page := gridPage([]time.Time{mon, tue, wed}, []string{"UA", "", ""}, []string{".", "", ""})
	if snaps := buildDays(t, page, 1, tue); len(snaps) != 1 {
		t.Fatalf("settle 1: Mon not settled on Tue (%d snapshots)", len(snaps))
	}
	if snaps := buildDays(t, page, 2, tue); len(snaps) != 0 {
		t.Fatal("settle 2: Mon settled after one school day")
	}
}

func TestRangeStopsBeforeTheFirstUnsettledDay(t *testing.T) {
	// Thu has a "." and only Fri and Mon follow (2 school days): Thu is
	// unsettled; Fri is settled. to = Wed; neither Thu nor Fri is emitted.
	page := gridPage([]time.Time{mon, tue, wed, thu, fri, nextMon},
		[]string{"", "UA", "", "UA", "UA", ""}, []string{"", "", "", ".", "", ""})
	snaps := buildDays(t, page, 3, nextMon)
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	wantRange(t, snaps[0], "2026-10-05", "2026-10-07")
	if keys := keysOfSnap(snaps[0]); len(keys) != 1 || keys[0] != "2026-10-06" {
		t.Fatalf("keys %v", keys)
	}
}

func TestCorrectedDayDropsOut(t *testing.T) {
	first := buildDays(t, gridPage([]time.Time{mon, tue, wed}, []string{"", "UA", ""}), 3, wed)
	if len(first) != 1 || len(first[0].Records) != 1 {
		t.Fatalf("first %+v", first)
	}
	// The code is removed: Tue is inside the range and has no record, so
	// the journal records it removed.
	second := buildDays(t, gridPage([]time.Time{mon, tue, wed}, []string{"", "", ""}), 3, wed)
	if len(second) != 1 || len(second[0].Records) != 0 {
		t.Fatalf("second %+v", second)
	}
	wantRange(t, second[0], "2026-10-05", "2026-10-06")
}

func TestFullyPresentDayIsNotARecord(t *testing.T) {
	snaps := buildDays(t, gridPage([]time.Time{mon, tue}, []string{"", "UA"}, []string{"", "-"}), 3, wed)
	// Mon is fully present: settled, inside the range, no record.
	wantRange(t, snaps[0], "2026-10-05", "2026-10-06")
	for _, k := range keysOfSnap(snaps[0]) {
		if k == "2026-10-05" {
			t.Fatal("a day without a code is a record")
		}
	}
}

func TestAttendanceDaysSplitAtTheSchoolYear(t *testing.T) {
	jul30, jul31, aug3 := day(2026, 7, 30), day(2026, 7, 31), day(2026, 8, 3)
	page := gridPage([]time.Time{jul30, jul31, aug3, day(2026, 8, 4)}, []string{"UA", "", "EA", ""})
	snaps := buildDays(t, page, 3, day(2026, 8, 4))
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	wantScope(t, snaps[0], "powerschool", "attendance_days", "e_child1", "school_year", "2025-26")
	wantRange(t, snaps[0], "2026-07-30", "2026-07-31")
	wantScope(t, snaps[1], "powerschool", "attendance_days", "e_child1", "school_year", "2026-27")
	wantRange(t, snaps[1], "2026-08-03", "2026-08-03")
	if len(snaps[0].Records) != 1 || len(snaps[1].Records) != 1 {
		t.Fatal("one coded day per year")
	}
}

func TestAttendanceDayFieldsAreTheSpecSet(t *testing.T) {
	page := gridPage([]time.Time{mon, tue}, []string{"FT", ""}, []string{"ZZ", ""}, []string{"LE", ""}, []string{"-", ""})
	snaps := buildDays(t, page, 3, tue)
	wantJSON(t, fieldsOf(t, snaps[0], "2026-10-05"), map[string]string{"periods_met": "3", "periods_unmarked": "0",
		"authorized": "1", "unknown_codes": "1", "left_early": "1", "any_unexcused": "false", "classification": `"late_or_left_early"`})
	if n := len(BuildAttendanceDays(bind1, nil, defaultDaysConfig(), 3, at(t, tue), testTZ(t), obsAt)); n != 0 {
		t.Fatalf("%d snapshots from no page", n)
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
