package schoology

import (
	"math/rand"
	"testing"
	"time"
)

// zeroSource is a rand.Source that always returns 0, forcing every splay to
// the first second of its window.
type zeroSource struct{}

func (zeroSource) Int63() int64 { return 0 }
func (zeroSource) Seed(int64)   {}

// TestScheduler_AdjacentWindowsZeroSplay: two windows that touch, and a
// splay of zero. After the first window's poll the next one is scheduled
// from that window's end, which IS the second window's start and, with a
// zero splay, its scheduled time. A strict "after now" comparison skipped
// the second window every day.
func TestScheduler_AdjacentWindowsZeroSplay(t *testing.T) {
	tz, err := loadTimezone("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	cfg.PollSchedule.WeekdaysOnly = true
	cfg.PollSchedule.Windows = []PollWindow{
		{Start: "07:00", End: "07:30"},
		{Start: "07:30", End: "08:00"},
	}
	rng := rand.New(zeroSource{})
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, tz) // a Monday
	end := start.AddDate(0, 0, 7)

	perDay := map[string]int{}
	perWindow := map[string]int{}
	now := start
	for {
		next, _ := computeNextPollTime(cfg, now, tz, rng)
		if !next.Before(end) {
			break
		}
		if next.Before(now) {
			t.Fatalf("next %s is before now %s", next, now)
		}
		we := windowEnd(cfg, next, tz)
		if we.Equal(next) {
			t.Fatalf("scheduled poll %s is outside every window", next)
		}
		perDay[next.Format("2006-01-02")]++
		perWindow[we.Format(time.RFC3339)]++
		now = we // what Watch does via notBefore
	}
	if len(perDay) != 5 {
		t.Fatalf("polled on %d days, want 5 weekdays: %v", len(perDay), perDay)
	}
	for day, n := range perDay {
		if n != 2 {
			t.Errorf("%s had %d polls, want 2 (one per adjacent window)", day, n)
		}
	}
	for w, n := range perWindow {
		if n != 1 {
			t.Errorf("window ending %s polled %d times, want 1", w, n)
		}
	}
}
