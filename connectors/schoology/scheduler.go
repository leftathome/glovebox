package schoology

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// computeNextPollTime returns the next scheduled poll time given the
// current moment, the timezone, and a random source for splay.
// The second return value is true when the current day was skipped
// because of weekdays_only=true falling on a weekend.
//
// The rng is mutated on each call and is not safe to share across
// goroutines; the Watcher owns its own *rand.Rand.
//
// Panics if cfg.PollSchedule.Windows is empty (config validation
// should have rejected this; see ValidateConfig).
//
// TODO: candidate for extraction to connector primitive base type
// (the windowed-with-splay pattern is generally useful).
func computeNextPollTime(cfg Config, now time.Time, tz *time.Location, rng *rand.Rand) (time.Time, bool) {
	if len(cfg.PollSchedule.Windows) == 0 {
		panic("schoology: computeNextPollTime called with zero windows; config validation should have rejected this")
	}
	now = now.In(tz)
	day := now
	skipped := false

	// Skip ahead through weekend days if weekdays_only.
	if cfg.PollSchedule.WeekdaysOnly && isWeekend(day) {
		skipped = true
		for isWeekend(day) {
			day = day.AddDate(0, 0, 1)
		}
		return splayedTimeIn(cfg.PollSchedule.Windows[0], day, tz, rng), true
	}

	// Find the first window today whose splayed time is strictly after now.
	for _, w := range cfg.PollSchedule.Windows {
		t := splayedTimeIn(w, day, tz, rng)
		if t.After(now) {
			return t, skipped
		}
	}

	// All today's windows are past -- roll to next day (or next weekday).
	day = day.AddDate(0, 0, 1)
	for cfg.PollSchedule.WeekdaysOnly && isWeekend(day) {
		day = day.AddDate(0, 0, 1)
	}
	return splayedTimeIn(cfg.PollSchedule.Windows[0], day, tz, rng), skipped
}

func isWeekend(t time.Time) bool {
	wd := t.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

// splayedTimeIn returns a random time strictly within window w on the
// given day. Both endpoints in the window are inclusive at start and
// exclusive at end (the math reflects that).
func splayedTimeIn(w PollWindow, day time.Time, tz *time.Location, rng *rand.Rand) time.Time {
	startH, startM := parseHHMM(w.Start)
	endH, endM := parseHHMM(w.End)
	startSec := startH*3600 + startM*60
	endSec := endH*3600 + endM*60
	if endSec <= startSec {
		panic(fmt.Sprintf("schoology: invalid window %q-%q reached scheduler; config validation should have rejected it", w.Start, w.End))
	}
	splay := rng.Intn(endSec - startSec)
	totalSec := startSec + splay
	return time.Date(day.Year(), day.Month(), day.Day(),
		totalSec/3600, (totalSec%3600)/60, totalSec%60, 0, tz)
}

func parseHHMM(s string) (int, int) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	hh, _ := strconv.Atoi(parts[0])
	mm, _ := strconv.Atoi(parts[1])
	return hh, mm
}

// loadTimezone returns the timezone for the scheduler. Empty input
// defaults to America/Los_Angeles per spec 12 §6.1.
func loadTimezone(name string) (*time.Location, error) {
	if name == "" {
		name = "America/Los_Angeles"
	}
	tz, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", name, err)
	}
	return tz, nil
}
