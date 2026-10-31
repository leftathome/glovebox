package powerschool

import (
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/leftathome/glovebox/connector"
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
//
// It carries nothing across polls; the connector uses
// buildAttendanceDaysCarry (decision S16).
func BuildAttendanceDays(b Binding, page *pslib.AttendancePage, cfg AttendanceDaysConfig, settle int,
	now time.Time, tz *time.Location, observedAt time.Time) []journalapi.Snapshot {
	snaps, _ := buildAttendanceDaysCarry(b, page, cfg, settle, now, tz, observedAt, nil)
	return snaps
}

// carryHorizonDays bounds how long a carried day waits to settle: one
// that has not settled this many calendar days after its date is dropped
// (decision S16).
const carryHorizonDays = 30

// pendingDay is a coded day the grid showed but could not yet settle: its
// tally and how many school days have followed it so far. The connector
// keeps a kid's pending days in its checkpoint (decision S16) so that the
// last days of a term still settle after the grid has rolled to the next
// term. Counts only: no course, code or name.
type pendingDay struct {
	Date      string `json:"date"`
	Met       int    `json:"met"`
	Unmarked  int    `json:"unmarked"`
	AbsUnex   int    `json:"absent_unexcused"`
	AbsEx     int    `json:"absent_excused"`
	Late      int    `json:"late"`
	LeftEarly int    `json:"left_early"`
	Auth      int    `json:"authorized"`
	Unknown   int    `json:"unknown_codes"`
	AnyUnex   bool   `json:"any_unexcused"`
	// Followed is the number of school days counted after Date, through
	// Through (inclusive).
	Followed int    `json:"followed"`
	Through  string `json:"through"`
}

func pendingOf(t *dayTally, followed int, through string) pendingDay {
	return pendingDay{Date: gridDate(t.date), Met: t.met, Unmarked: t.unmarked, AbsUnex: t.absUnex, AbsEx: t.absEx,
		Late: t.late, LeftEarly: t.leftEarly, Auth: t.auth, Unknown: t.unknown, AnyUnex: t.anyUnex,
		Followed: followed, Through: through}
}

func (p pendingDay) tally() (*dayTally, bool) {
	d, err := time.Parse("2006-01-02", p.Date)
	if err != nil {
		return nil, false
	}
	return &dayTally{date: d, met: p.Met, unmarked: p.Unmarked, absUnex: p.AbsUnex, absEx: p.AbsEx, late: p.Late,
		leftEarly: p.LeftEarly, auth: p.Auth, unknown: p.Unknown, anyUnex: p.AnyUnex, coded: true}, true
}

// dayRecord is an attendance_day record for a settled day.
func dayRecord(b Binding, t *dayTally) journalapi.Record {
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
	return b.record(gridDate(t.date), f)
}

// buildAttendanceDaysCarry is BuildAttendanceDays with the days carried
// from earlier polls (decision S16). It returns the snapshots and the days
// to carry on.
//
// Carried out of this poll: every coded grid day on or before today that
// lies after the settled range -- today itself, a day still waiting for
// school days after an unmarked cell, and any day after such a one.
//
// A carried day that the grid still shows is ignored: the grid wins, so a
// correction is picked up. Any other carried day counts the grid's school
// days after it (and after the last day already counted) toward settling,
// by the same rule as a grid day. Once settled it is sent in a
// complete=false snapshot of its school year's scope: it adds or updates
// that day and removes nothing, so the current term's complete_range is
// not widened over the break between terms. A carried day not settled
// carryHorizonDays after its date is dropped and counted.
func buildAttendanceDaysCarry(b Binding, page *pslib.AttendancePage, cfg AttendanceDaysConfig, settle int,
	now time.Time, tz *time.Location, observedAt time.Time, carried []pendingDay) ([]journalapi.Snapshot, []pendingDay) {
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
	// schoolDaysAfter counts the grid's school days after d (all are on or
	// before today).
	schoolDaysAfter := func(d string) int {
		after := sort.Search(len(schoolDays), func(i int) bool { return schoolDays[i] > d })
		return len(schoolDays) - after
	}
	isSettled := func(date string, unmarked, followed int) bool {
		return date < today && (unmarked == 0 || followed >= settle)
	}
	settled := func(d string) bool { return isSettled(d, tallies[d].unmarked, schoolDaysAfter(d)) }

	var out []journalapi.Snapshot
	var pending []pendingDay
	for _, y := range gridYears(page) {
		to := ""
		for _, d := range y.dates {
			if !settled(d) {
				break
			}
			to = d
		}
		recs := []journalapi.Record{}
		for _, d := range y.dates {
			t := tallies[d]
			if !t.coded {
				continue
			}
			if d > to {
				if d <= today {
					pending = append(pending, pendingOf(t, schoolDaysAfter(d), today))
				}
				continue
			}
			recs = append(recs, dayRecord(b, t))
		}
		if to == "" {
			continue
		}
		rng := &journalapi.CompleteRange{Field: "date", From: y.from(), To: to}
		s := records.NewSnapshot(TypeAttendanceDay, recordSchemaVersion, b.scope("attendance_days", records.Part("school_year", y.year)),
			true, rng, observedAt, recs)
		if b.fits(s) {
			out = append(out, s)
		}
	}

	// Days carried from earlier polls that the grid no longer shows.
	settledCarried := map[string][]journalapi.Record{}
	for _, c := range carried {
		if _, inGrid := tallies[c.Date]; inGrid {
			continue
		}
		t, ok := c.tally()
		if !ok {
			continue
		}
		if expires := t.date.AddDate(0, 0, carryHorizonDays).Format("2006-01-02"); expires < today {
			b.skip("attendance_day_carry_expired")
			slog.Warn("powerschool attendance day carried past the horizon without settling; dropped",
				"record_type", TypeAttendanceDay)
			continue
		}
		followed := c.Followed + schoolDaysAfter(max(c.Date, c.Through))
		if isSettled(c.Date, c.Unmarked, followed) {
			y := records.SchoolYear(t.date)
			settledCarried[y] = append(settledCarried[y], dayRecord(b, t))
			continue
		}
		c.Followed, c.Through = followed, max(c.Through, today)
		pending = append(pending, c)
	}
	years := make([]string, 0, len(settledCarried))
	for y := range settledCarried {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		recs := settledCarried[y]
		sort.Slice(recs, func(i, j int) bool { return recs[i].Key < recs[j].Key })
		s := records.NewSnapshot(TypeAttendanceDay, recordSchemaVersion, b.scope("attendance_days", records.Part("school_year", y)),
			false, nil, observedAt, recs)
		if b.fits(s) {
			out = append(out, s)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Date < pending[j].Date })
	return out, pending
}

// pendingDaysKey is the checkpoint key of a kid's carried attendance days.
// Written only with records enabled.
func pendingDaysKey(kid string) string { return "meta:records_pending_attendance_days:" + kid }

// loadPendingDays reads a kid's carried days. No key (state from before
// decision S16, or nothing carried) is none; an unreadable value is logged
// and taken as none.
func loadPendingDays(cp connector.Checkpoint, kid string) []pendingDay {
	raw, ok := cp.Load(pendingDaysKey(kid))
	if !ok || raw == "" {
		return nil
	}
	var out []pendingDay
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		slog.Warn("powerschool carried attendance days unreadable; starting without them",
			"record_type", TypeAttendanceDay, "kid", kid)
		return nil
	}
	return out
}

// savePendingDays writes a kid's carried days through the checkpoint (an
// atomic file write); an empty set removes the key.
func savePendingDays(cp connector.Checkpoint, kid string, days []pendingDay) error {
	if len(days) == 0 {
		if _, ok := cp.Load(pendingDaysKey(kid)); !ok {
			return nil
		}
		return cp.Delete(pendingDaysKey(kid))
	}
	raw, err := json.Marshal(days)
	if err != nil {
		return err
	}
	return cp.Save(pendingDaysKey(kid), string(raw))
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
