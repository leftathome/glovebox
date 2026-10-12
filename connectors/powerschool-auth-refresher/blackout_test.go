package main

import (
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/psportal"
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
	b, err := parseBlackout("", "", "", "")
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

// A login is refused when it could still be in progress inside the window:
// from loginHeadroom (login timeout 90s + 60s margin) before the start.
func TestBlackoutBlocksALoginThatCouldRunIntoTheWindow(t *testing.T) {
	if loginTimeout != 90*time.Second || loginHeadroom != 150*time.Second {
		t.Fatalf("this table assumes a 90s login timeout and 150s headroom; got %s and %s", loginTimeout, loginHeadroom)
	}
	b := defaultBlackout(t)
	la := mustZone(t, "America/Los_Angeles")
	for _, day := range blackoutDays {
		for _, c := range []struct {
			h, m, s int
			blocked bool
		}{
			{15, 57, 29, false}, // ends 15:59:59
			{15, 57, 30, true},  // could reach 16:00:00
			{15, 58, 0, true},
			{15, 58, 31, true}, // the 90s timeout alone reaches 16:00:01
			{15, 59, 30, true},
			{15, 59, 59, true},
			{16, 0, 0, true},
			{18, 30, 0, true},
			{20, 59, 59, true},
			{21, 0, 0, false},
			{5, 0, 0, false},
			{21, 5, 0, false},
		} {
			at := time.Date(2026, time.Month(day.month), day.day, c.h, c.m, c.s, 0, la).UTC()
			if got := b.blocksLoginAt(at); got != c.blocked {
				t.Errorf("%s %02d:%02d:%02d: blocksLoginAt = %v, want %v", day.name, c.h, c.m, c.s, got, c.blocked)
			}
		}
	}
}

func clockAt(t *testing.T, h, m int) time.Time {
	t.Helper()
	return time.Date(2026, 5, 4, h, m, 0, 0, mustZone(t, "America/Los_Angeles"))
}

// Configuration may widen the blackout: an earlier start, a later end, a
// window that runs past midnight, further windows.
func TestBlackoutCanBeWidened(t *testing.T) {
	for _, c := range []struct {
		name              string
		start, end, extra string
		inside, outside   [][2]int
		want              string
	}{
		{"earlier start", "15:00", "", "", [][2]int{{15, 0}, {15, 30}, {16, 0}, {20, 59}}, [][2]int{{14, 59}, {21, 0}}, "16:00-21:00,15:00-21:00"},
		{"later end", "", "22:30", "", [][2]int{{16, 0}, {22, 29}}, [][2]int{{15, 59}, {22, 30}}, "16:00-21:00,16:00-22:30"},
		{"both", "14:00", "23:00", "", [][2]int{{14, 0}, {22, 59}}, [][2]int{{13, 59}, {23, 0}}, "16:00-21:00,14:00-23:00"},
		{"past midnight", "15:00", "02:00", "", [][2]int{{15, 0}, {23, 59}, {0, 0}, {1, 59}, {18, 0}}, [][2]int{{2, 0}, {14, 59}}, "16:00-21:00,15:00-02:00"},
		{"starting before midnight the day before", "23:00", "21:30", "", [][2]int{{23, 0}, {3, 0}, {16, 0}, {21, 29}}, [][2]int{{21, 30}, {22, 59}}, "16:00-21:00,23:00-21:30"},
		{"extra windows", "", "", "07:30-08:30, 23:00-01:00", [][2]int{{7, 30}, {8, 29}, {23, 0}, {0, 59}, {18, 0}}, [][2]int{{7, 29}, {8, 30}, {1, 0}, {5, 0}}, "16:00-21:00,07:30-08:30,23:00-01:00"},
		{"the floor spelled out", "16:00", "21:00", "", [][2]int{{16, 0}, {20, 59}}, [][2]int{{15, 59}, {21, 0}}, "16:00-21:00"},
		{"nothing configured", "", "", "", nil, nil, "16:00-21:00"},
	} {
		b, err := parseBlackout(c.start, c.end, c.extra, "America/Los_Angeles")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		for _, hm := range c.inside {
			if !b.contains(clockAt(t, hm[0], hm[1])) {
				t.Errorf("%s: %02d:%02d is not inside", c.name, hm[0], hm[1])
			}
		}
		for _, hm := range c.outside {
			if b.contains(clockAt(t, hm[0], hm[1])) {
				t.Errorf("%s: %02d:%02d is inside", c.name, hm[0], hm[1])
			}
		}
		if b.String() != c.want {
			t.Errorf("%s: String() = %q, want %q", c.name, b.String(), c.want)
		}
	}
}

// Configuration cannot shorten the blackout, move it, change its zone or
// empty it. Each attempt is a config error -- and the blackout returned
// alongside the error still holds the floor, so no code path that ignored
// the error could log in at 18:00.
func TestBlackoutCannotBeWeakened(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	for _, c := range []struct{ name, start, end, extra, zone string }{
		{"later start", "17:00", "21:00", "", ""},
		{"earlier end", "16:00", "20:00", "", ""},
		{"one minute short at the start", "16:01", "", "", ""},
		{"one minute short at the end", "", "20:59", "", ""},
		{"moved to the night", "22:00", "02:00", "", ""},
		{"moved to the morning", "05:00", "09:00", "", ""},
		{"inverted, leaving the afternoon out", "21:00", "16:00", "", ""},
		{"empty window", "16:00", "16:00", "", ""},
		{"another zone", "", "", "", "UTC"},
		{"another zone that looks close", "", "", "", "America/Denver"},
		{"Local zone", "", "", "", "Local"},
		{"not a time", "4pm", "21:00", "", ""},
		{"hour out of range", "24:00", "", "", ""},
		{"minute out of range", "15:60", "", "", ""},
		{"single digit hour", "4:00", "", "", ""},
		{"seconds given", "16:00:00", "", "", ""},
		{"malformed extra window", "", "", "0700-0800", ""},
		{"extra window without an end", "", "", "07:00", ""},
		{"empty extra window", "", "", "07:00-07:00", ""},
	} {
		b, err := parseBlackout(c.start, c.end, c.extra, c.zone)
		if err == nil {
			t.Errorf("%s: parseBlackout(%q, %q, %q, %q) succeeded", c.name, c.start, c.end, c.extra, c.zone)
		}
		if b.loc == nil || b.loc.String() != "America/Los_Angeles" {
			t.Errorf("%s: the returned blackout is not in America/Los_Angeles", c.name)
			continue
		}
		for _, hm := range [][3]int{{16, 0, 0}, {18, 30, 0}, {20, 59, 59}} {
			if !b.contains(time.Date(2026, 5, 4, hm[0], hm[1], hm[2], 0, la)) {
				t.Errorf("%s: the returned blackout does not cover %02d:%02d:%02d", c.name, hm[0], hm[1], hm[2])
			}
		}
	}
}

// The connector decides whether to ASK for a login with
// psportal.FloorBlocksLoginAt; this binary decides whether to log in with
// its own blackout. With nothing widened the two must agree at every
// instant, or the connector would create Jobs this binary then refuses (or
// hold back when a login would have been fine).
func TestDefaultBlackoutAgreesWithTheSharedFloor(t *testing.T) {
	b := defaultBlackout(t)
	la := mustZone(t, "America/Los_Angeles")
	for _, day := range blackoutDays {
		start := time.Date(2026, time.Month(day.month), day.day, 0, 0, 0, 0, la)
		for at := start; at.Before(start.Add(25 * time.Hour)); at = at.Add(7 * time.Second) {
			if b.blocksLoginAt(at) != psportal.FloorBlocksLoginAt(at) || b.contains(at) != psportal.InFloor(at) {
				t.Fatalf("%s %s: blackout says blocks=%v inside=%v, the shared floor says blocks=%v inside=%v",
					day.name, at.Format("15:04:05"), b.blocksLoginAt(at), b.contains(at),
					psportal.FloorBlocksLoginAt(at), psportal.InFloor(at))
			}
		}
	}
	if loginTimeout != psportal.LoginTimeout || loginHeadroom != psportal.LoginHeadroom {
		t.Error("the login timeout or headroom differs from the shared one")
	}
}
