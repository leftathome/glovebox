package dailywindow

import (
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestParseClockAndWindow(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "16:00": 57600, "23:59": 86340, " 07:05 ": 25500} {
		if got, err := ParseClock(in); err != nil || got != want {
			t.Errorf("ParseClock(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "4:00", "16:0", "24:00", "15:60", "16:00:00", "4pm", "ab:cd", "-1:00"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) succeeded", bad)
		}
	}
	if _, err := Parse("16:00", "16:00"); err == nil {
		t.Error("an empty window was accepted")
	}
	w, err := Parse("22:00", "02:00")
	if err != nil || w.String() != "22:00-02:00" {
		t.Fatalf("Parse = %v, %v", w, err)
	}
	for sec, inside := range map[int]bool{22 * 3600: true, 23*3600 + 59: true, 0: true, 2*3600 - 1: true, 2 * 3600: false, 12 * 3600: false} {
		if w.ContainsSecond(sec) != inside {
			t.Errorf("22:00-02:00 contains %s = %v", ClockString(sec), !inside)
		}
	}
}

func TestParseList(t *testing.T) {
	ws, err := ParseList(" 16:00-21:00 , 02:00-03:30,")
	if err != nil || len(ws) != 2 || ws[1].String() != "02:00-03:30" {
		t.Fatalf("ParseList = %v, %v", ws, err)
	}
	if ws, err := ParseList(""); err != nil || len(ws) != 0 {
		t.Errorf("empty list = %v, %v", ws, err)
	}
	for _, bad := range []string{"16:00", "1600-2100", "16:00-21:00-22:00", "16:00-16:00", "16:00 21:00"} {
		if _, err := ParseList(bad); err == nil {
			t.Errorf("ParseList(%q) succeeded", bad)
		}
	}
}

func TestCovers(t *testing.T) {
	floor := Window{Start: 16 * 3600, End: 21 * 3600}
	for _, c := range []struct {
		w    Window
		want bool
	}{
		{Window{16 * 3600, 21 * 3600}, true},
		{Window{15 * 3600, 22 * 3600}, true},
		{Window{12 * 3600, 2 * 3600}, true}, // wraps midnight, still holds the afternoon
		{Window{17 * 3600, 21 * 3600}, false},
		{Window{16 * 3600, 20*3600 + 59*60}, false},
		{Window{21 * 3600, 16 * 3600}, false}, // everything but
	} {
		if got := c.w.Covers(floor); got != c.want {
			t.Errorf("%s covers %s = %v", c.w, floor, got)
		}
	}
}

// A window is on the wall clock of its zone, daylight saving included, and
// "blocks" reaches back from its start by the headroom.
func TestSetFollowsTheWallClockAndTheHeadroom(t *testing.T) {
	la := zone(t, "America/Los_Angeles")
	s := Set{Windows: []Window{{16 * 3600, 21 * 3600}}, Loc: la}
	const headroom = 150 * time.Second
	for _, day := range []time.Time{
		time.Date(2026, 7, 6, 0, 0, 0, 0, la),  // PDT
		time.Date(2026, 12, 7, 0, 0, 0, 0, la), // PST
		time.Date(2026, 3, 8, 0, 0, 0, 0, la),  // clocks go forward
		time.Date(2026, 11, 1, 0, 0, 0, 0, la), // clocks go back
	} {
		at := func(h, m, sec int) time.Time {
			return time.Date(day.Year(), day.Month(), day.Day(), h, m, sec, 0, la)
		}
		for _, c := range []struct {
			t                time.Time
			contains, blocks bool
		}{
			{at(15, 57, 29), false, false},
			{at(15, 57, 30), false, true},
			{at(15, 59, 59), false, true},
			{at(16, 0, 0), true, true},
			{at(20, 59, 59), true, true},
			{at(21, 0, 0), false, false},
			{at(5, 0, 0), false, false},
		} {
			if s.Contains(c.t) != c.contains || s.BlocksAt(c.t, headroom) != c.blocks {
				t.Errorf("%s: contains %v blocks %v, want %v %v", c.t.Format("2006-01-02 15:04:05 MST"),
					s.Contains(c.t), s.BlocksAt(c.t, headroom), c.contains, c.blocks)
			}
		}
	}
	if (Set{Windows: []Window{{0, 86399}}}).Contains(time.Now()) {
		t.Error("a Set without a zone contains something")
	}
}

func TestScheduleNextClear(t *testing.T) {
	la := zone(t, "America/Los_Angeles")
	sc := Schedule{
		{Windows: []Window{{16 * 3600, 21 * 3600}}, Loc: la},
		{Windows: []Window{{4 * 3600, 5 * 3600}}, Loc: time.UTC}, // 21:00-22:00 PDT: back to back with the first
	}
	const headroom = 150 * time.Second

	clear := time.Date(2026, 7, 6, 9, 0, 0, 0, la)
	if got, ok := sc.NextClear(clear, headroom); !ok || !got.Equal(clear) {
		t.Errorf("a clear instant: %v, %v", got, ok)
	}
	// From inside the first window: the end of the SECOND, which begins
	// where the first ends.
	got, ok := sc.NextClear(time.Date(2026, 7, 6, 18, 0, 0, 0, la), headroom)
	if want := time.Date(2026, 7, 6, 22, 0, 0, 0, la); !ok || !got.Equal(want) {
		t.Errorf("NextClear = %v, %v; want %v", got, ok, want)
	}
	// From the headroom before a window.
	got, ok = sc.NextClear(time.Date(2026, 7, 6, 15, 58, 10, 0, la), headroom)
	if want := time.Date(2026, 7, 6, 22, 0, 0, 0, la); !ok || !got.Equal(want) {
		t.Errorf("NextClear from the headroom = %v, %v", got, ok)
	}
	// Windows that cover the whole day have no clear instant.
	always := Schedule{{Windows: []Window{{0, 12 * 3600}, {12 * 3600, 0}}, Loc: time.UTC}}
	if _, ok := always.NextClear(clear, headroom); ok {
		t.Error("a schedule covering the whole day reported a clear instant")
	}
	if got := sc.String(); got != "16:00-21:00 America/Los_Angeles; 04:00-05:00 UTC" {
		t.Errorf("String = %q", got)
	}
	if (Schedule{}).BlocksAt(clear, headroom) {
		t.Error("an empty schedule blocks")
	}
}
