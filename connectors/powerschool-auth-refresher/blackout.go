package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Defaults for the span in which this binary refuses to log in. The portal
// keeps one live session per account and the account is its owner's own:
// a login by this job signs him out. He uses the portal between 16:00 and
// 21:00 Pacific (owner decision, 2026-10-09; spec 16 section 4).
const (
	defaultBlackoutStart = "16:00"
	defaultBlackoutEnd   = "21:00"
	defaultBlackoutZone  = "America/Los_Angeles"
)

// blackout is a daily span of wall-clock time in one zone during which no
// login may happen. The span is [start, end): a run at exactly start is
// refused, a run at exactly end proceeds. A span whose start is later than
// its end wraps past midnight (22:00-02:00).
//
// It is evaluated against the wall clock of loc for the instant given, so
// it follows daylight-saving changes: 16:00 means 16:00 on the owner's
// clock on that day, whatever the UTC offset is.
type blackout struct {
	// start and end are seconds since local midnight.
	start, end int
	loc        *time.Location
}

// parseBlackout builds a blackout from "HH:MM" bounds and an IANA zone
// name. Equal bounds are refused: they could mean "never" or "always", and
// a login guard must not be switched off by a typo.
func parseBlackout(start, end, zone string) (blackout, error) {
	s, err := parseClock(start)
	if err != nil {
		return blackout{}, fmt.Errorf("blackout start: %w", err)
	}
	e, err := parseClock(end)
	if err != nil {
		return blackout{}, fmt.Errorf("blackout end: %w", err)
	}
	if s == e {
		return blackout{}, fmt.Errorf("blackout start and end are both %s: the window cannot be empty", start)
	}
	if strings.TrimSpace(zone) == "" {
		return blackout{}, fmt.Errorf("blackout time zone is empty")
	}
	// "Local" would follow the pod's TZ, which nobody sets on purpose.
	if zone == "Local" {
		return blackout{}, fmt.Errorf("blackout time zone must be an IANA name, not Local")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return blackout{}, fmt.Errorf("blackout time zone %q: %w", zone, err)
	}
	return blackout{start: s, end: e, loc: loc}, nil
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

// contains reports whether the instant t falls inside the window.
func (b blackout) contains(t time.Time) bool {
	h, m, s := t.In(b.loc).Clock()
	sec := h*3600 + m*60 + s
	if b.start < b.end {
		return sec >= b.start && sec < b.end
	}
	return sec >= b.start || sec < b.end
}

// String renders the window for a log line, e.g. "16:00-21:00".
func (b blackout) String() string {
	return clockString(b.start) + "-" + clockString(b.end)
}

func clockString(sec int) string {
	return fmt.Sprintf("%02d:%02d", sec/3600, sec%3600/60)
}
