package powerschool

import (
	"sort"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
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

// BuildAttendanceDays builds one attendance_day snapshot per school year
// shown on the page, over its settled days only (decisions S1-S3).
//
// "Today" is now's calendar date in the school's time zone tz; grid dates
// are compared with it as YYYY-MM-DD. A school day is a grid date on or
// before today with at least one met period. A date is settled when it is
// before today and either has no unmarked cell or has been followed by at
// least settle school days (those after it, today included).
//
// The range runs from the year's first grid date to the last date of the
// unbroken run of settled dates that starts there: every date up to `to`
// is settled, so the snapshot never removes a day it has not judged. A
// year with no such run gets no snapshot. Records are the coded days
// inside the range, keyed by date.
func BuildAttendanceDays(b Binding, page *pslib.AttendancePage, cfg AttendanceDaysConfig, settle int,
	now time.Time, tz *time.Location, observedAt time.Time) []journalapi.Snapshot {
	if settle < 1 {
		settle = DefaultAttendanceDaySettleSchoolDays
	}
	if tz == nil {
		tz = time.UTC
	}
	tallies := tallyDays(page, cfg)
	today := now.In(tz).Format("2006-01-02")
	var schoolDays []string
	for d, t := range tallies {
		if d <= today && t.met > 0 {
			schoolDays = append(schoolDays, d)
		}
	}
	sort.Strings(schoolDays)
	settled := func(d string) bool {
		if d >= today {
			return false
		}
		if tallies[d].unmarked == 0 {
			return true
		}
		after := sort.Search(len(schoolDays), func(i int) bool { return schoolDays[i] > d })
		return len(schoolDays)-after >= settle
	}

	var out []journalapi.Snapshot
	for _, y := range gridYears(page) {
		to := ""
		for _, d := range y.dates {
			if !settled(d) {
				break
			}
			to = d
		}
		if to == "" {
			continue
		}
		recs := []journalapi.Record{}
		for _, d := range y.dates {
			t := tallies[d]
			if d > to {
				break
			}
			if !t.coded {
				continue
			}
			f := records.NewFields(nil)
			f.Date("date", t.date)
			f.Int("periods_met", t.met)
			f.Int("periods_unmarked", t.unmarked)
			f.Int("absent_unexcused", t.absUnex)
			f.Int("absent_excused", t.absEx)
			f.Int("late", t.late)
			f.Int("left_early", t.leftEarly)
			f.Int("authorized", t.auth)
			f.Int("unknown_codes", t.unknown)
			f.Bool("any_unexcused", t.anyUnex)
			f.String("classification", t.classification())
			recs = append(recs, b.record(d, f))
		}
		rng := &journalapi.CompleteRange{Field: "date", From: y.from(), To: to}
		s := records.NewSnapshot(TypeAttendanceDay, recordSchemaVersion, b.scope("attendance_days", records.Part("school_year", y.year)),
			true, rng, observedAt, recs)
		if b.fits(s) {
			out = append(out, s)
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
