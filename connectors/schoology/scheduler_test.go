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

func TestScheduler_MidWindow_PicksLaterToday(t *testing.T) {
	// Test: 08:00 (inside morning window) but if splay rolls a time before 08:00,
	// the scheduler should roll forward to the afternoon window (or next day).
	cfg := Config{
		PollSchedule: PollSchedule{
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
				{Start: "15:30", End: "17:30"},
			},
		},
	}
	tz := time.UTC
	// 08:00 on a Tuesday, inside morning window
	now := time.Date(2026, 5, 19, 8, 0, 0, 0, tz)
	rng := rand.New(rand.NewSource(12345))
	next, _ := computeNextPollTime(cfg, now, tz, rng)
	// "next" must be strictly AFTER now.
	if !next.After(now) {
		t.Errorf("expected next > now, got next=%v now=%v", next, now)
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
