package powerschool

import (
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// course_mark and attendance_total come from the home page's grade rows:
// one record per course and one complete scope per course. Unlike items,
// every row is a record -- a course with no grade posted, a course with no
// absence -- because records describe state, not change. A scope holds one
// record, so a complete snapshot of it can never remove another course.

// CourseRecordKey is a course row's record key and the value of its
// scope's course part: courseKey made safe for the journal (decision S7).
// Exported for the seed (S15).
func CourseRecordKey(g *pslib.Grade) string { return records.SafeKey(courseKey(g)) }

// gradeRows yields the non-nil rows with a course key not seen before on
// the page; a repeated key is counted and skipped (frn is per enrolment, so
// this should not happen).
func gradeRows(b Binding, grades []*pslib.Grade, each func(g *pslib.Grade, key string)) {
	seen := map[string]bool{}
	for _, g := range grades {
		if g == nil {
			continue
		}
		key := CourseRecordKey(g)
		if seen[key] {
			b.skip("duplicate_key")
			continue
		}
		seen[key] = true
		each(g, key)
	}
}

// BuildCourseMarks builds one course_mark snapshot per course row.
func BuildCourseMarks(b Binding, grades []*pslib.Grade, observedAt time.Time) []journalapi.Snapshot {
	var out []journalapi.Snapshot
	gradeRows(b, grades, func(g *pslib.Grade, key string) {
		f := records.NewFields(nil)
		f.String("course", g.CourseName)
		f.String("period", g.Period)
		f.String("teacher", g.Teacher)
		terms := termGrades(g)
		for _, term := range termOrder {
			letter, percent := splitGrade(terms[term])
			name := strings.ToLower(term)
			f.String(name+"_letter", letter)
			f.Number(name+"_percent", parsePercent(percent))
		}
		s := records.NewSnapshot(TypeCourseMark, recordSchemaVersion, b.scope("grades", records.Part("course", courseKey(g))),
			true, nil, observedAt, []journalapi.Record{b.record(key, f)})
		if b.fits(s) {
			out = append(out, s)
		}
	})
	return out
}

// BuildAttendanceTotals builds one attendance_total snapshot per course
// row, zero counts included.
func BuildAttendanceTotals(b Binding, grades []*pslib.Grade, observedAt time.Time) []journalapi.Snapshot {
	var out []journalapi.Snapshot
	gradeRows(b, grades, func(g *pslib.Grade, key string) {
		f := records.NewFields(nil)
		f.String("course", g.CourseName)
		f.Int("absences", g.Absences)
		f.Int("tardies", g.Tardies)
		s := records.NewSnapshot(TypeAttendanceTotal, recordSchemaVersion,
			b.scope("attendance_totals", records.Part("course", courseKey(g))),
			true, nil, observedAt, []journalapi.Record{b.record(key, f)})
		if b.fits(s) {
			out = append(out, s)
		}
	})
	return out
}

// parsePercent is splitGrade's percent as a number; empty or unparsable is
// nil (null).
func parsePercent(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}
