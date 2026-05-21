package schoology

import (
	"math/rand"
	"testing"
	"time"
)

func TestScheduler_NextPollTime_Determinism(t *testing.T) {
	// Same date + same RNG seed -> same splay time, always.
	cfg := Config{
		PollSchedule: PollSchedule{
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
				{Start: "15:30", End: "17:30"},
			},
		},
	}
	tz := time.UTC
	// Tuesday 2026-05-19 at 05:00 UTC -- before any window.
	now := time.Date(2026, 5, 19, 5, 0, 0, 0, tz)
	rng1 := rand.New(rand.NewSource(42))
	rng2 := rand.New(rand.NewSource(42))
	t1, _ := computeNextPollTime(cfg, now, tz, rng1)
	t2, _ := computeNextPollTime(cfg, now, tz, rng2)
	if !t1.Equal(t2) {
		t.Errorf("non-deterministic with same seed: %v vs %v", t1, t2)
	}
	// Verify the time falls within the first window.
	hh := t1.Hour()
	if hh < 7 || hh >= 9 {
		t.Errorf("not in morning window: %v (hour=%d)", t1, hh)
	}
}

func TestScheduler_SkipWeekends(t *testing.T) {
	cfg := Config{
		PollSchedule: PollSchedule{
			WeekdaysOnly: true,
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
			},
		},
	}
	tz := time.UTC
	// Saturday 2026-05-23 -- weekend, should be skipped.
	now := time.Date(2026, 5, 23, 5, 0, 0, 0, tz)
	rng := rand.New(rand.NewSource(1))
	next, skipped := computeNextPollTime(cfg, now, tz, rng)
	if !skipped {
		t.Errorf("expected Saturday to be skipped, next=%v", next)
	}
	if next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		t.Errorf("scheduler returned a weekend day: %v", next.Weekday())
	}
	// Monday should NOT be skipped.
	mon := time.Date(2026, 5, 25, 5, 0, 0, 0, tz)
	next2, skipped2 := computeNextPollTime(cfg, mon, tz, rng)
	if skipped2 {
		t.Errorf("Monday unexpectedly skipped")
	}
	if next2.Weekday() != time.Monday {
		t.Errorf("expected Monday, got %v", next2.Weekday())
	}
}

func TestScheduler_AfterAllWindows_RollsToNextDay(t *testing.T) {
	cfg := Config{
		PollSchedule: PollSchedule{
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
				{Start: "15:30", End: "17:30"},
			},
		},
	}
	tz := time.UTC
	// 18:00 -- past both windows.
	now := time.Date(2026, 5, 19, 18, 0, 0, 0, tz)
	rng := rand.New(rand.NewSource(1))
	next, _ := computeNextPollTime(cfg, now, tz, rng)
	if next.Day() == now.Day() {
		t.Errorf("expected next day, got %v same day as %v", next, now)
	}
}

func TestScheduler_MidWindow_NeverReturnsBeforeNow(t *testing.T) {
	// Property: with now inside a window, the scheduler must always
	// return a time strictly after now, even when the splayed time within
	// the current window happens to roll earlier than now. Use a one-window
	// config at now=08:30 so most seeds roll a splay before now, which
	// forces the t.After(now) guard to take the rollover branch.
	cfg := Config{
		PollSchedule: PollSchedule{
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
			},
		},
	}
	tz := time.UTC
	now := time.Date(2026, 5, 19, 8, 30, 0, 0, tz) // Tuesday, 30 min into window

	rolledToTomorrow := 0
	for seed := int64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		next, _ := computeNextPollTime(cfg, now, tz, rng)
		if !next.After(now) {
			t.Errorf("seed %d: next %v not strictly after now %v", seed, next, now)
		}
		if next.Day() != now.Day() {
			rolledToTomorrow++
		}
	}
	if rolledToTomorrow == 0 {
		t.Errorf("no seeds triggered day-rollover; t.After(now) guard untested by this sweep")
	}
}

func TestScheduler_FridayAfterWindows_RollsToMonday(t *testing.T) {
	// Compound case: weekdays_only + all today's windows past + today is Friday.
	// Scheduler must roll past Saturday and Sunday and land on Monday.
	cfg := Config{
		PollSchedule: PollSchedule{
			WeekdaysOnly: true,
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
				{Start: "15:30", End: "17:30"},
			},
		},
	}
	tz := time.UTC
	// Friday 2026-05-22 at 18:00 UTC -- past both windows.
	now := time.Date(2026, 5, 22, 18, 0, 0, 0, tz)
	if now.Weekday() != time.Friday {
		t.Fatalf("fixture date is not Friday: %v", now.Weekday())
	}
	rng := rand.New(rand.NewSource(1))
	next, _ := computeNextPollTime(cfg, now, tz, rng)
	if next.Weekday() != time.Monday {
		t.Errorf("expected Monday, got %v (%v)", next.Weekday(), next)
	}
}

func TestLoadTimezone_Default(t *testing.T) {
	tz, err := loadTimezone("")
	if err != nil {
		t.Fatalf("loadTimezone empty: %v", err)
	}
	if tz.String() != "America/Los_Angeles" {
		t.Errorf("default tz: got %q", tz.String())
	}
}

func TestLoadTimezone_Explicit(t *testing.T) {
	tz, err := loadTimezone("UTC")
	if err != nil {
		t.Fatalf("loadTimezone UTC: %v", err)
	}
	if tz.String() != "UTC" {
		t.Errorf("explicit tz: got %q", tz.String())
	}
}

func TestLoadTimezone_Invalid(t *testing.T) {
	_, err := loadTimezone("Definitely/Not/A/Timezone")
	if err == nil {
		t.Errorf("expected error for invalid timezone")
	}
}
