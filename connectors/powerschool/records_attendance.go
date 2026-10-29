package powerschool

import (
	"sort"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// attendance_mark: one record per coded cell of the attendance page, the
// same (course, day) the attendance_day items are about. The page shows a
// window of dates, so a snapshot is complete only over the dates it showed:
// one snapshot per school year in the grid, with complete_range over that
// year's first and last grid date. A code removed from a day inside the
// range is a removed record; a day outside it (last term) is untouched.

// AttendanceMarkRecordKey is the record key of a course row's cell on date:
// the item path's checkpoint id "<attendanceCourseKey>:<date>" made safe
// for the journal. Exported for the seed (S15).
func AttendanceMarkRecordKey(course *pslib.AttendanceCourse, date time.Time) string {
	return records.SafeKey(attendanceCourseKey(course) + ":" + gridDate(date))
}

// gridDate is a grid cell's calendar date. The library parses it as
// midnight UTC.
func gridDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

// gridYear is one school year's part of the grid: every date shown, in
// order.
type gridYear struct {
	year  string
	dates []string
}

func (y gridYear) from() string { return y.dates[0] }
func (y gridYear) to() string   { return y.dates[len(y.dates)-1] }

// gridYears groups the dates of every cell of every row, whatever the cell
// holds, by school year, in date order.
func gridYears(page *pslib.AttendancePage) []gridYear {
	if page == nil {
		return nil
	}
	byYear := map[string]map[string]bool{}
	for _, course := range page.Courses {
		if course == nil {
			continue
		}
		for _, c := range course.Days {
			y := records.SchoolYear(c.Date.UTC())
			if byYear[y] == nil {
				byYear[y] = map[string]bool{}
			}
			byYear[y][gridDate(c.Date)] = true
		}
	}
	var out []gridYear
	for y, set := range byYear {
		g := gridYear{year: y}
		for d := range set {
			g.dates = append(g.dates, d)
		}
		sort.Strings(g.dates)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].year < out[j].year })
	return out
}

// excusedOf is decision S10: true for an excused kind, false for an
// unexcused one, unknown (null) for authorized and unknown codes.
func excusedOf(kind string) *bool {
	var v bool
	switch {
	case strings.HasSuffix(kind, "_unexcused"):
		v = false
	case strings.HasSuffix(kind, "_excused"):
		v = true
	default:
		return nil
	}
	return &v
}

// BuildAttendanceMarks builds one attendance_mark snapshot per school year
// shown on the page. A page with no dates gives none; a year with dates and
// no codes gives an empty snapshot over its range.
func BuildAttendanceMarks(b Binding, page *pslib.AttendancePage, cfg AttendanceDaysConfig, observedAt time.Time) []journalapi.Snapshot {
	if cfg.Kinds == nil {
		cfg.Kinds = defaultAttendanceKinds()
	}
	years := gridYears(page)
	if len(years) == 0 {
		return nil
	}
	recs := map[string][]journalapi.Record{}
	seen := map[string]bool{}
	for _, course := range page.Courses {
		if course == nil {
			continue
		}
		for _, c := range course.Days {
			if c.Kind != pslib.CellCode || c.Code == "" {
				continue
			}
			key := AttendanceMarkRecordKey(course, c.Date)
			if seen[key] {
				b.skip("duplicate_key")
				continue
			}
			seen[key] = true
			entry, _ := page.Describe(c.Code)
			kind := cfg.kindOf(entry.Category)
			f := records.NewFields(nil)
			f.Date("date", c.Date.UTC())
			f.String("course", course.CourseName)
			f.String("period", course.Expression)
			f.String("code", c.Code)
			f.String("code_description", entry.Description)
			f.String("kind", kind)
			f.OptBool("excused", excusedOf(kind))
			y := records.SchoolYear(c.Date.UTC())
			recs[y] = append(recs[y], b.record(key, f))
		}
	}
	var out []journalapi.Snapshot
	for _, y := range years {
		rng := &journalapi.CompleteRange{Field: "date", From: y.from(), To: y.to()}
		s := records.NewSnapshot(TypeAttendanceMark, recordSchemaVersion, b.scope("attendance_marks", records.Part("school_year", y.year)),
			true, rng, observedAt, recs[y.year])
		if b.fits(s) {
			out = append(out, s)
		}
	}
	return out
}
