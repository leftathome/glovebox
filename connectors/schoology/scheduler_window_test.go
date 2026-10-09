package schoology

import (
	"math/rand"
	"testing"
	"time"
)

func threeWindowConfig() Config {
	var cfg Config
	cfg.PollSchedule.WeekdaysOnly = true
	cfg.PollSchedule.Windows = []PollWindow{
		{Start: "07:00", End: "07:30"},
		{Start: "15:45", End: "16:15"},
		{Start: "19:00", End: "19:30"},
	}
	return cfg
}

func TestWindowEnd(t *testing.T) {
	tz, err := loadTimezone("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	cfg := threeWindowConfig()
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 12, h, m, s, 0, tz) }

	for _, tc := range []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"window start", at(7, 0, 0), at(7, 30, 0)},
		{"mid window", at(7, 12, 41), at(7, 30, 0)},
		{"last second", at(7, 29, 59), at(7, 30, 0)},
		{"window end is exclusive", at(7, 30, 0), at(7, 30, 0)},
		{"between windows", at(12, 0, 0), at(12, 0, 0)},
		{"evening window", at(19, 5, 0), at(19, 30, 0)},
	} {
		if got := windowEnd(cfg, tc.in, tz); !got.Equal(tc.want) {
			t.Errorf("%s: windowEnd(%s) = %s, want %s", tc.name, tc.in.Format("15:04:05"), got.Format("15:04:05"), tc.want.Format("15:04:05"))
		}
	}
}

// TestScheduler_OnePollPerWindow walks the Watch loop's scheduling for a full
// week under many seeds. Scheduling the next poll from the moment the last one
// fired re-rolled inside the same window and polled it again about 70% of the
// time; scheduling from the end of that window must give exactly one poll per
// window, weekdays only.
func TestScheduler_OnePollPerWindow(t *testing.T) {
	tz, err := loadTimezone("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	cfg := threeWindowConfig()
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, tz) // a Monday
	end := start.AddDate(0, 0, 7)

	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		perDay := map[string]int{}
		perWindow := map[string]int{}
		now := start
		for {
			next, _ := computeNextPollTime(cfg, now, tz, rng)
			if !next.Before(end) {
				break
			}
			if !next.After(now) && !next.Equal(now) {
				t.Fatalf("seed %d: next %s is before now %s", seed, next, now)
			}
			if isWeekend(next) {
				t.Fatalf("seed %d: scheduled a weekend poll at %s", seed, next)
			}
			we := windowEnd(cfg, next, tz)
			if we.Equal(next) {
				t.Fatalf("seed %d: scheduled poll %s is outside every window", seed, next)
			}
			perDay[next.Format("2006-01-02")]++
			perWindow[we.Format(time.RFC3339)]++
			now = we // what Watch does via notBefore
		}
		if len(perDay) != 5 {
			t.Fatalf("seed %d: polled on %d days, want 5 weekdays", seed, len(perDay))
		}
		for day, n := range perDay {
			if n != 3 {
				t.Fatalf("seed %d: %s had %d polls, want 3", seed, day, n)
			}
		}
		for w, n := range perWindow {
			if n != 1 {
				t.Fatalf("seed %d: window ending %s polled %d times, want 1", seed, w, n)
			}
		}
	}
}

// TestScheduler_FromFireTimeRepolls documents the defect windowEnd exists to
// prevent: scheduling from the fire time does revisit a window.
func TestScheduler_FromFireTimeRepolls(t *testing.T) {
	tz, err := loadTimezone("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	cfg := threeWindowConfig()
	repolls := 0
	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		first, _ := computeNextPollTime(cfg, time.Date(2026, 10, 12, 6, 0, 0, 0, tz), tz, rng)
		second, _ := computeNextPollTime(cfg, first, tz, rng)
		if windowEnd(cfg, second, tz).Equal(windowEnd(cfg, first, tz)) {
			repolls++
		}
	}
	if repolls == 0 {
		t.Fatal("expected scheduling from the fire time to revisit the same window for some seeds; the regression this guards no longer reproduces")
	}
}
