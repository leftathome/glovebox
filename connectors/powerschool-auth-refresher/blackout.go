package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The floor: the span in which this binary never logs in, whatever it is
// configured with. The portal keeps one live session per account and the
// account is its owner's own, so a login by this job signs him out; he uses
// the portal between 16:00 and 21:00 Pacific (owner decision, 2026-10-09;
// spec 16 section 4).
//
// These are constants, not defaults. Configuration can make the blackout
// larger -- an earlier start, a later end, further windows -- and cannot
// make it smaller or move it to another zone. A values typo, or an operator
// in a hurry, must not be able to switch the guard off.
const (
	floorBlackoutStart = "16:00"
	floorBlackoutEnd   = "21:00"
	blackoutZone       = "America/Los_Angeles"
)

// loginHeadroom is how far ahead of a login the blackout is checked. A
// login is not instantaneous: the browser may take up to loginTimeout to
// reach the submit, so a login started at 15:59:30 could sign in at
// 16:00:30. No login starts unless the whole span from now to now +
// loginTimeout + loginMargin is clear of every window.
const (
	loginMargin   = 60 * time.Second
	loginHeadroom = loginTimeout + loginMargin
)

// window is one daily span [start, end) in seconds since local midnight. A
// span whose start is later than its end wraps past midnight (22:00-02:00).
type window struct {
	start, end int
}

func (w window) containsSecond(sec int) bool {
	if w.start < w.end {
		return sec >= w.start && sec < w.end
	}
	return sec >= w.start || sec < w.end
}

func (w window) String() string {
	return clockString(w.start) + "-" + clockString(w.end)
}

// covers reports whether w contains every second of other.
func (w window) covers(other window) bool {
	for sec := other.start; sec != other.end; sec = (sec + 60) % 86400 {
		if !w.containsSecond(sec) {
			return false
		}
	}
	// The last second before other.end.
	return w.containsSecond((other.end + 86400 - 1) % 86400)
}

// blackout is the set of daily windows, all on the wall clock of one zone,
// during which no login may happen. It always includes the floor.
//
// It is evaluated against the wall clock of loc for the instant given, so
// it follows daylight-saving changes: 16:00 means 16:00 on the owner's
// clock on that day, whatever the UTC offset is.
type blackout struct {
	windows []window
	loc     *time.Location
}

// floorWindow is 16:00-21:00.
func floorWindow() window {
	s, _ := parseClock(floorBlackoutStart)
	e, _ := parseClock(floorBlackoutEnd)
	return window{start: s, end: e}
}

// parseBlackout builds the blackout.
//
//	start, end  the main window, "HH:MM". It must contain the floor; it may
//	            be larger. Empty means the floor itself.
//	extra       further windows, "HH:MM-HH:MM" separated by commas. May be
//	            empty.
//	zone        must be empty or exactly America/Los_Angeles.
//
// The floor is always part of the result, independently of the validation
// above: if that validation were ever wrong, the floor would still hold.
func parseBlackout(start, end, extra, zone string) (blackout, error) {
	loc, err := time.LoadLocation(blackoutZone)
	if err != nil {
		return blackout{}, fmt.Errorf("blackout time zone %s: %w", blackoutZone, err)
	}
	b := blackout{windows: []window{floorWindow()}, loc: loc}

	if z := strings.TrimSpace(zone); z != "" && z != blackoutZone {
		return b, fmt.Errorf("the blackout time zone is fixed at %s and cannot be changed", blackoutZone)
	}

	if strings.TrimSpace(start) == "" {
		start = floorBlackoutStart
	}
	if strings.TrimSpace(end) == "" {
		end = floorBlackoutEnd
	}
	main, err := parseWindow(start, end)
	if err != nil {
		return b, err
	}
	if !main.covers(floorWindow()) {
		return b, fmt.Errorf("the blackout %s does not contain %s-%s: it may be widened, never shortened or moved",
			main, floorBlackoutStart, floorBlackoutEnd)
	}
	if main != floorWindow() {
		b.windows = append(b.windows, main)
	}

	for _, item := range strings.Split(extra, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		bounds := strings.Split(item, "-")
		if len(bounds) != 2 {
			return b, fmt.Errorf("extra blackout window %q is not HH:MM-HH:MM", item)
		}
		w, err := parseWindow(bounds[0], bounds[1])
		if err != nil {
			return b, fmt.Errorf("extra blackout window %q: %w", item, err)
		}
		b.windows = append(b.windows, w)
	}
	return b, nil
}

// parseWindow reads one window. Equal bounds are refused: they could mean
// "never" or "always".
func parseWindow(start, end string) (window, error) {
	s, err := parseClock(start)
	if err != nil {
		return window{}, fmt.Errorf("blackout start: %w", err)
	}
	e, err := parseClock(end)
	if err != nil {
		return window{}, fmt.Errorf("blackout end: %w", err)
	}
	if s == e {
		return window{}, fmt.Errorf("blackout start and end are both %s: a window cannot be empty", strings.TrimSpace(start))
	}
	return window{start: s, end: e}, nil
}

// parseClock reads "HH:MM" (00:00 through 23:59) as seconds since midnight.
func parseClock(v string) (int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	return h*3600 + m*60, nil
}

// contains reports whether the instant t falls inside any window.
func (b blackout) contains(t time.Time) bool {
	h, m, s := t.In(b.loc).Clock()
	sec := h*3600 + m*60 + s
	for _, w := range b.windows {
		if w.containsSecond(sec) {
			return true
		}
	}
	return false
}

// blocksLoginAt reports whether a login started at t could still be in
// progress inside a window: whether any instant from t to t + loginHeadroom
// is inside one. This, not contains, is what gates a login.
func (b blackout) blocksLoginAt(t time.Time) bool {
	const step = 15 * time.Second
	for d := time.Duration(0); d < loginHeadroom; d += step {
		if b.contains(t.Add(d)) {
			return true
		}
	}
	return b.contains(t.Add(loginHeadroom))
}

// String renders the windows for a log line, e.g. "16:00-21:00".
func (b blackout) String() string {
	parts := make([]string, 0, len(b.windows))
	for _, w := range b.windows {
		parts = append(parts, w.String())
	}
	return strings.Join(parts, ",")
}

func clockString(sec int) string {
	return fmt.Sprintf("%02d:%02d", sec/3600, sec%3600/60)
}
