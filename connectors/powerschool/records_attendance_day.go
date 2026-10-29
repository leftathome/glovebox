package powerschool

import (
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// attendance_day: one record per school day with at least one attendance
// code, aggregated over every course row of the attendance page (spec 5.1).

// dayTally is one grid date across all course rows.
type dayTally struct {
	date time.Time
	// met: cells that are not "-" (no meeting); unmarked: "." cells.
	met, unmarked int
	// Code cells by kind.
	absUnex, absEx, late, leftEarly, auth, unknown int
	// anyUnex: an *_unexcused kind on the day; coded: any code at all.
	anyUnex, coded bool
}

// tallyDays counts every grid date of the page. A code cell with an empty
// code counts as met and present.
func tallyDays(page *pslib.AttendancePage, cfg AttendanceDaysConfig) map[string]*dayTally {
	if cfg.Kinds == nil {
		cfg.Kinds = defaultAttendanceKinds()
	}
	out := map[string]*dayTally{}
	if page == nil {
		return out
	}
	for _, course := range page.Courses {
		if course == nil {
			continue
		}
		for _, cell := range course.Days {
			k := gridDate(cell.Date)
			t := out[k]
			if t == nil {
				t = &dayTally{date: cell.Date.UTC()}
				out[k] = t
			}
			switch cell.Kind {
			case pslib.CellNoMeeting:
				continue
			case pslib.CellUnmarked:
				t.met++
				t.unmarked++
				continue
			}
			t.met++
			if cell.Kind != pslib.CellCode || cell.Code == "" {
				continue
			}
			entry, _ := page.Describe(cell.Code)
			t.coded = true
			switch kind := cfg.kindOf(entry.Category); kind {
			case "absence_unexcused":
				t.absUnex++
				t.anyUnex = true
			case "absence_excused":
				t.absEx++
			case "late_unexcused", "late_excused":
				t.late++
				t.anyUnex = t.anyUnex || kind == "late_unexcused"
			case "left_early_unexcused", "left_early_excused":
				t.leftEarly++
				t.anyUnex = t.anyUnex || kind == "left_early_unexcused"
			case "authorized":
				t.auth++
			default:
				t.unknown++
			}
		}
	}
	return out
}

// classification is over the marked periods (met minus unmarked): every
// marked period an absence is a full day, some a partial day; no absence
// but a late or left-early is late_or_left_early; only authorized or
// unknown codes is other. (More absences than marked periods cannot
// happen: every absence cell is a met, marked cell.)
func (t *dayTally) classification() string {
	marked, abs := t.met-t.unmarked, t.absUnex+t.absEx
	switch {
	case abs >= 1 && abs == marked:
		return "full_day"
	case abs >= 1:
		return "partial_day"
	case t.late+t.leftEarly >= 1:
		return "late_or_left_early"
	}
	return "other"
}
