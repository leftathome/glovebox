package powerschool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

func defaultDaysConfig() AttendanceDaysConfig {
	return AttendanceDaysConfig{LookbackDays: 36500, Kinds: defaultAttendanceKinds()}
}

func cell(date time.Time, kind pslib.AttendanceCellKind) pslib.AttendanceDay {
	return pslib.AttendanceDay{Date: date, Kind: kind}
}

func wantRange(t *testing.T, s journalapi.Snapshot, from, to string) {
	t.Helper()
	if !s.Complete || s.CompleteRange == nil || s.CompleteRange.Field != "date" || s.CompleteRange.From != from || s.CompleteRange.To != to {
		t.Fatalf("complete %v range %+v, want date %s..%s", s.Complete, s.CompleteRange, from, to)
	}
}

func TestAttendanceMarksPerSchoolYearWithRange(t *testing.T) {
	page := attendanceFixture("111111") // 10-02 L, 10-05 blank, 10-06 VU, 10-07 "-", 10-08 "."
	snaps := BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	s := snaps[0]
	wantScope(t, s, "powerschool", "attendance_marks", "e_child1", "school_year", "2026-27")
	wantRange(t, s, "2026-10-02", "2026-10-08")
	if got := strings.Join(keysOfSnap(s), ","); got != "example science 7a|1(A):2026-10-06,example science 7a|1(A):2026-10-02" {
		t.Fatalf("keys %s", got)
	}
	wantJSON(t, fieldsOf(t, s, "example science 7a|1(A):2026-10-06"), map[string]string{"date": `"2026-10-06"`,
		"course": `"EXAMPLE SCIENCE 7A"`, "period": `"1(A)"`, "code": `"VU"`, "code_description": `"Unexcused Travel"`,
		"kind": `"absence_unexcused"`, "excused": "false"})
	wantJSON(t, fieldsOf(t, s, "example science 7a|1(A):2026-10-02"), map[string]string{"kind": `"late_unexcused"`, "excused": "false"})
}

func TestAttendanceMarksSplitAtTheSchoolYear(t *testing.T) {
	page := attendanceFixture("111111", codeDay(day(2026, 7, 30), "EA"), cell(day(2026, 7, 31), pslib.CellPresent),
		cell(day(2026, 8, 3), pslib.CellPresent), codeDay(day(2026, 8, 4), "UA"), cell(day(2026, 8, 5), pslib.CellNoMeeting))
	snaps := BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	wantScope(t, snaps[0], "powerschool", "attendance_marks", "e_child1", "school_year", "2025-26")
	wantRange(t, snaps[0], "2026-07-30", "2026-07-31")
	wantScope(t, snaps[1], "powerschool", "attendance_marks", "e_child1", "school_year", "2026-27")
	wantRange(t, snaps[1], "2026-08-03", "2026-08-05")
	if len(snaps[0].Records) != 1 || len(snaps[1].Records) != 1 {
		t.Fatalf("records %d / %d", len(snaps[0].Records), len(snaps[1].Records))
	}
}

// Every code corrected away: an empty snapshot over the range removes them.
func TestAttendanceMarksEmptyButDatedIsAnEmptyCompleteSnapshot(t *testing.T) {
	page := attendanceFixture("111111", cell(day(2026, 10, 5), pslib.CellPresent), cell(day(2026, 10, 6), pslib.CellUnmarked))
	snaps := BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	if len(snaps) != 1 || len(snaps[0].Records) != 0 {
		t.Fatalf("snapshots %+v", snaps)
	}
	wantRange(t, snaps[0], "2026-10-05", "2026-10-06")
}

func TestAttendanceMarksNoDatesNoSnapshot(t *testing.T) {
	page := attendanceFixture("111111")
	page.Courses[0].Days = []pslib.AttendanceDay{}
	if n := len(BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)); n != 0 {
		t.Fatalf("%d snapshots", n)
	}
	if n := len(BuildAttendanceMarks(bind1, nil, defaultDaysConfig(), obsAt)); n != 0 {
		t.Fatalf("%d snapshots from no page", n)
	}
}

func TestAttendanceMarksKeysEqualTheCheckpointIDs(t *testing.T) {
	env, _ := newTestEnv(t, &fakeClient{})
	page := attendanceFixture("111111")
	page.Courses = append(page.Courses, &pslib.AttendanceCourse{CourseName: "EXAMPLE ART", Expression: "2(A)",
		EnrollDate: day(2026, 9, 1), Days: []pslib.AttendanceDay{codeDay(day(2026, 10, 6), "EA"), codeDay(day(2026, 10, 7), "ZZ")}})
	ProcessAttendanceDays(context.Background(), env, testKid, page, defaultDaysConfig())
	snaps := BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	n := 0
	for _, r := range snaps[0].Records {
		n++
		if _, ok := env.CP.Load(CheckpointKey(attendanceDaySurface, "k1", r.Key)); !ok {
			t.Errorf("record %q has no checkpoint id", r.Key)
		}
	}
	if n != 4 {
		t.Fatalf("%d records", n)
	}
	if AttendanceMarkRecordKey(page.Courses[1], day(2026, 10, 6)) != "example art|2(A)|2026-09-01:2026-10-06" {
		t.Fatal("AttendanceMarkRecordKey")
	}
}

func TestAttendanceMarksExcused(t *testing.T) {
	page := attendanceFixture("111111", codeDay(day(2026, 10, 1), "EA"), codeDay(day(2026, 10, 2), "UA"),
		codeDay(day(2026, 10, 5), "FT"), codeDay(day(2026, 10, 6), "ZZ"), codeDay(day(2026, 10, 7), "LE"))
	snaps := BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	k := func(d string) string { return "example science 7a|1(A):" + d }
	wantJSON(t, fieldsOf(t, snaps[0], k("2026-10-01")), map[string]string{"kind": `"absence_excused"`, "excused": "true"})
	wantJSON(t, fieldsOf(t, snaps[0], k("2026-10-02")), map[string]string{"kind": `"absence_unexcused"`, "excused": "false"})
	wantJSON(t, fieldsOf(t, snaps[0], k("2026-10-05")), map[string]string{"kind": `"authorized"`, "excused": "null"})
	wantJSON(t, fieldsOf(t, snaps[0], k("2026-10-06")), map[string]string{"kind": `"unknown"`, "excused": "null",
		"code": `"ZZ"`, "code_description": "null"})
	wantJSON(t, fieldsOf(t, snaps[0], k("2026-10-07")), map[string]string{"kind": `"left_early_excused"`, "excused": "true"})
}

func TestAttendanceMarksReEnrolmentIsADifferentRecord(t *testing.T) {
	page := attendanceFixture("111111", codeDay(day(2026, 10, 6), "UA"))
	again := *page.Courses[0]
	again.EnrollDate = day(2026, 9, 15)
	page.Courses = append(page.Courses, &again)
	snaps := BuildAttendanceMarks(bind1, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	if len(snaps[0].Records) != 2 || snaps[0].Records[0].Key == snaps[0].Records[1].Key {
		t.Fatalf("keys %v", keysOfSnap(snaps[0]))
	}
}

// Two rows the page cannot tell apart: the second's cells would repeat a
// key (422 duplicate_key, permanent); they are counted and left out.
func TestAttendanceMarksIndistinguishableRowsKeepTheFirst(t *testing.T) {
	page := attendanceFixture("111111", codeDay(day(2026, 10, 6), "UA"))
	page.Courses = append(page.Courses, page.Courses[0], nil)
	var skipped []string
	b := bind1
	b.OnSkip = func(r string) { skipped = append(skipped, r) }
	snaps := BuildAttendanceMarks(b, page, defaultDaysConfig(), obsAt)
	validateAll(t, snaps)
	if len(snaps[0].Records) != 1 || len(skipped) != 1 || skipped[0] != "duplicate_key" {
		t.Fatalf("records %d skipped %v", len(snaps[0].Records), skipped)
	}
}

// Spec 8, term rollover: this term's snapshot range starts after every
// date of last term's, so the journal's removal step cannot touch last
// term's records. (The end-to-end version against the real journal is in
// Task 23.)
func TestAttendanceMarksTermChangeDoesNotRemoveLastTerm(t *testing.T) {
	last := attendanceFixture("111111", codeDay(day(2026, 9, 8), "UA"), cell(day(2026, 9, 9), pslib.CellPresent))
	this := attendanceFixture("111111", cell(day(2026, 10, 5), pslib.CellPresent), codeDay(day(2026, 10, 6), "EA"))
	a := BuildAttendanceMarks(bind1, last, defaultDaysConfig(), obsAt)
	b := BuildAttendanceMarks(bind1, this, defaultDaysConfig(), obsAt)
	validateAll(t, append(a, b...))
	if a[0].CompleteRange.To >= b[0].CompleteRange.From {
		t.Fatalf("ranges overlap: %+v then %+v", a[0].CompleteRange, b[0].CompleteRange)
	}
	for _, r := range a[0].Records {
		d := strings.TrimPrefix(string(r.Fields["date"]), `"`)
		if d[:10] >= b[0].CompleteRange.From {
			t.Fatalf("last term's %s is inside this term's range", d)
		}
	}
}
