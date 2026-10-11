package main

import (
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func defaultBlackout(t *testing.T) blackout {
	t.Helper()
	b, err := parseBlackout(defaultBlackoutStart, defaultBlackoutEnd, defaultBlackoutZone)
	if err != nil {
		t.Fatalf("default blackout: %v", err)
	}
	return b
}

// blackoutDays are the dates every boundary is checked on: an ordinary
// winter and summer day, and the day before, of and after each 2026
// daylight-saving change in America/Los_Angeles (clocks go forward on
// 8 March and back on 1 November).
var blackoutDays = []struct {
	name       string
	month, day int
}{
	{"winter", 1, 14},
	{"summer", 7, 15},
	{"day before spring forward", 3, 7},
	{"spring forward", 3, 8},
	{"day after spring forward", 3, 9},
	{"day before fall back", 10, 31},
	{"fall back", 11, 1},
	{"day after fall back", 11, 2},
}

// blackoutClock are the owner's boundary cases: 16:00:00, 18:30 and
// 20:59:59 are refused; 15:59:59 and 21:00:00 proceed.
var blackoutClock = []struct {
	h, m, s int
	inside  bool
}{
	{15, 59, 59, false},
	{16, 0, 0, true},
	{18, 30, 0, true},
	{20, 59, 59, true},
	{21, 0, 0, false},
	{5, 0, 0, false},
	{21, 30, 0, false},
	{0, 0, 0, false},
}

func TestBlackoutDefaultWindowBoundariesOnEveryKindOfDay(t *testing.T) {
	b := defaultBlackout(t)
	la := mustZone(t, "America/Los_Angeles")
	for _, day := range blackoutDays {
		for _, c := range blackoutClock {
			local := time.Date(2026, time.Month(day.month), day.day, c.h, c.m, c.s, 0, la)
			// The process clock is UTC in the pod; the answer must not
			// depend on the location the instant is carried in.
			for _, instant := range []time.Time{local, local.UTC()} {
				if got := b.contains(instant); got != c.inside {
					t.Errorf("%s %02d:%02d:%02d (%s): contains = %v, want %v",
						day.name, c.h, c.m, c.s, instant.Format(time.RFC3339), got, c.inside)
				}
			}
		}
	}
}

// The table above builds its instants with the same zone database the code
// uses. These are fixed UTC instants worked out by hand from the published
// offsets (PST = UTC-8, PDT = UTC-7), so a wrong offset cannot hide.
func TestBlackoutAgainstHandComputedUTCInstants(t *testing.T) {
	b := defaultBlackout(t)
	cases := []struct {
		name   string
		utc    time.Time
		inside bool
	}{
		// 7 March 2026 is still PST: 16:00 local is 00:00 UTC next day.
		{"PST 15:59:59", time.Date(2026, 3, 7, 23, 59, 59, 0, time.UTC), false},
		{"PST 16:00:00", time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), true},
		{"PST 20:59:59", time.Date(2026, 3, 8, 4, 59, 59, 0, time.UTC), true},
		{"PST 21:00:00", time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), false},
		// 8 March 2026 is PDT from 02:00: 16:00 local is 23:00 UTC.
		{"PDT 15:59:59", time.Date(2026, 3, 8, 22, 59, 59, 0, time.UTC), false},
		{"PDT 16:00:00", time.Date(2026, 3, 8, 23, 0, 0, 0, time.UTC), true},
		{"PDT 18:30:00", time.Date(2026, 3, 9, 1, 30, 0, 0, time.UTC), true},
		{"PDT 20:59:59", time.Date(2026, 3, 9, 3, 59, 59, 0, time.UTC), true},
		{"PDT 21:00:00", time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC), false},
		// 31 October 2026 is the last PDT day.
		{"last PDT day 16:00:00", time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), true},
		{"last PDT day 21:00:00", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), false},
		// 1 November 2026 is PST again from 02:00: 16:00 local is 00:00 UTC.
		{"first PST day 15:59:59", time.Date(2026, 11, 1, 23, 59, 59, 0, time.UTC), false},
		{"first PST day 16:00:00", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), true},
		{"first PST day 18:30:00", time.Date(2026, 11, 2, 2, 30, 0, 0, time.UTC), true},
		{"first PST day 20:59:59", time.Date(2026, 11, 2, 4, 59, 59, 0, time.UTC), true},
		{"first PST day 21:00:00", time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC), false},
		// 23:00 UTC on 1 November was 16:00 the day before the change and is
		// 15:00 on the day of it: the window moved with the wall clock.
		{"23:00 UTC after fall back is 15:00 local", time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := b.contains(c.utc); got != c.inside {
			t.Errorf("%s (%s): contains = %v, want %v", c.name, c.utc.Format(time.RFC3339), got, c.inside)
		}
	}
}

func TestBlackoutConfigurableAndWrapsMidnight(t *testing.T) {
	b, err := parseBlackout("22:30", "02:15", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	ny := mustZone(t, "America/New_York")
	for _, c := range []struct {
		h, m, s int
		inside  bool
	}{
		{22, 29, 59, false}, {22, 30, 0, true}, {23, 59, 59, true},
		{0, 0, 0, true}, {2, 14, 59, true}, {2, 15, 0, false}, {12, 0, 0, false},
	} {
		at := time.Date(2026, 5, 4, c.h, c.m, c.s, 0, ny)
		if got := b.contains(at); got != c.inside {
			t.Errorf("%02d:%02d:%02d: contains = %v, want %v", c.h, c.m, c.s, got, c.inside)
		}
	}
	if b.String() != "22:30-02:15" {
		t.Errorf("String() = %q", b.String())
	}
}

func TestParseBlackoutRefusesWhatCouldSwitchTheGuardOff(t *testing.T) {
	for _, c := range []struct{ name, start, end, zone string }{
		{"empty window", "16:00", "16:00", "America/Los_Angeles"},
		{"not a time", "4pm", "21:00", "America/Los_Angeles"},
		{"hour out of range", "24:00", "21:00", "America/Los_Angeles"},
		{"minute out of range", "16:60", "21:00", "America/Los_Angeles"},
		{"single digit hour", "4:00", "21:00", "America/Los_Angeles"},
		{"seconds given", "16:00:00", "21:00", "America/Los_Angeles"},
		{"empty end", "16:00", "", "America/Los_Angeles"},
		{"unknown zone", "16:00", "21:00", "America/Seattle"},
		{"empty zone", "16:00", "21:00", " "},
		{"Local zone", "16:00", "21:00", "Local"},
	} {
		if _, err := parseBlackout(c.start, c.end, c.zone); err == nil {
			t.Errorf("%s: parseBlackout(%q, %q, %q) succeeded", c.name, c.start, c.end, c.zone)
		}
	}
}
