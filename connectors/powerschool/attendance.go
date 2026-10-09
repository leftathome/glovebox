package powerschool

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	pslib "github.com/leftathome/powerschool-go"
)

// attendanceSurface is the per-kid surface for absences and tardies.
const attendanceSurface = "attendance"

// attendanceFingerprint is the checkpoint value for a course's attendance
// counts: "absences=2|tardies=1".
func attendanceFingerprint(absences, tardies int) string {
	return fmt.Sprintf("absences=%d|tardies=%d", absences, tardies)
}

// parseAttendanceFingerprint is the inverse of attendanceFingerprint. A
// missing or malformed value reads as zero.
func parseAttendanceFingerprint(fp string) (absences, tardies int) {
	for _, part := range strings.Split(fp, "|") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		switch k {
		case "absences":
			absences = n
		case "tardies":
			tardies = n
		}
	}
	return absences, tardies
}

// ProcessAttendance stages one item per course whose year-to-date absence
// or tardy count has changed.
//
// The counts come from the same home-page table as the grades; the library
// exposes no dated attendance records, so this surface reports "the count
// for this course moved from N to M", not "absent on the 3rd". A course
// never seen before is compared against zero, so a clean record stages
// nothing and the first absence stages one item.
func ProcessAttendance(ctx context.Context, e Env, kid Kid, grades []*pslib.Grade) Result {
	var res Result
	rule, ok := matchRule(e, kid, attendanceSurface)
	if !ok {
		return res
	}

	for _, g := range grades {
		if g == nil {
			continue
		}
		key := courseKey(g)
		fp := attendanceFingerprint(g.Absences, g.Tardies)
		change, prev := Compare(e.CP, attendanceSurface, kid.Name, key, fp)
		if change == ChangeNone {
			continue
		}
		prevAbs, prevTardy := parseAttendanceFingerprint(prev)
		if change == ChangeNew && g.Absences == 0 && g.Tardies == 0 {
			continue
		}

		tags := map[string]string{
			"absences":       strconv.Itoa(g.Absences),
			"tardies":        strconv.Itoa(g.Tardies),
			"absences_delta": strconv.Itoa(g.Absences - prevAbs),
			"tardies_delta":  strconv.Itoa(g.Tardies - prevTardy),
		}
		setTag(tags, "course", g.CourseName)
		setTag(tags, "period", g.Period)

		subject := fmt.Sprintf("Attendance: %s (%d absences, %d tardies)", g.CourseName, g.Absences, g.Tardies)
		opts := itemOptions(e, rule, kid, attendanceSurface, change, g.CourseName, subject, tags)

		var b strings.Builder
		fmt.Fprintf(&b, "Course: %s\n", g.CourseName)
		if g.Period != "" {
			fmt.Fprintf(&b, "Period: %s\n", g.Period)
		}
		fmt.Fprintf(&b, "Absences this year: %d (was %d)\n", g.Absences, prevAbs)
		fmt.Fprintf(&b, "Tardies this year: %d (was %d)\n", g.Tardies, prevTardy)

		if err := stage(ctx, e, attendanceSurface, opts, b.String()); err != nil {
			slog.Warn("powerschool attendance stage failed", "kid", kid.Name, "error", err)
			res.Failed++
			continue
		}
		if err := SaveFingerprint(e.CP, attendanceSurface, kid.Name, key, fp); err != nil {
			slog.Error("powerschool attendance checkpoint save failed",
				"kid", kid.Name, "duplicate_risk", true, "error", err)
		}
		e.Tel.RecordItem(attendanceSurface, kid.Name, change)
		res.Staged++
	}
	return res
}
