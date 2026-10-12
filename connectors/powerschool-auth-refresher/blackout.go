package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/dailywindow"
	"github.com/leftathome/glovebox/internal/psportal"
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
//
// The values live in internal/psportal, which the connector imports too: it
// must not ask for a login in the hours this binary would refuse one.
const (
	floorBlackoutStart = psportal.FloorStart
	floorBlackoutEnd   = psportal.FloorEnd
	blackoutZone       = psportal.FloorZone
)

// loginHeadroom is how far ahead of a login the blackout is checked. A
// login is not instantaneous: the browser may take up to loginTimeout to
// reach the submit, so a login started at 15:59:30 could sign in at
// 16:00:30. No login starts unless the whole span from now to now +
// loginTimeout + loginMargin is clear of every window.
const (
	loginMargin   = psportal.LoginMargin
	loginHeadroom = psportal.LoginHeadroom
)

// window is one daily span on a wall clock; see internal/dailywindow, which
// the login broker builds its blackout from as well.
type window = dailywindow.Window

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
	s, _ := dailywindow.ParseClock(floorBlackoutStart)
	e, _ := dailywindow.ParseClock(floorBlackoutEnd)
	return window{Start: s, End: e}
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
	if !main.Covers(floorWindow()) {
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
	w, err := dailywindow.Parse(start, end)
	if err != nil {
		return window{}, fmt.Errorf("blackout %w", err)
	}
	return w, nil
}

// set is the blackout as the shared arithmetic sees it.
func (b blackout) set() dailywindow.Set {
	return dailywindow.Set{Windows: b.windows, Loc: b.loc}
}

// contains reports whether the instant t falls inside any window.
func (b blackout) contains(t time.Time) bool { return b.set().Contains(t) }

// blocksLoginAt reports whether a login started at t could still be in
// progress inside a window: whether any instant from t to t + loginHeadroom
// is inside one. This, not contains, is what gates a login.
func (b blackout) blocksLoginAt(t time.Time) bool { return b.set().BlocksAt(t, loginHeadroom) }

// String renders the windows for a log line, e.g. "16:00-21:00".
func (b blackout) String() string { return b.set().String() }
