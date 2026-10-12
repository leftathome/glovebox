// Package dailywindow is the arithmetic of "not between these hours": daily
// spans on the wall clock of a named time zone, and the question whether
// something started now could still be running inside one.
//
// It holds no policy. The PowerSchool login job
// (connectors/powerschool-auth-refresher) and the login broker
// (internal/loginbroker) both build their blackouts from it, so the two
// cannot disagree about what "16:00-21:00 America/Los_Angeles" means on the
// day the clocks change.
package dailywindow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const daySeconds = 86400

// Window is one daily span [Start, End) in seconds since local midnight. A
// span whose start is later than its end wraps past midnight (22:00-02:00).
type Window struct {
	Start, End int
}

// ContainsSecond reports whether sec (seconds since local midnight) is
// inside the window.
func (w Window) ContainsSecond(sec int) bool {
	if w.Start < w.End {
		return sec >= w.Start && sec < w.End
	}
	return sec >= w.Start || sec < w.End
}

// String renders the window as "HH:MM-HH:MM".
func (w Window) String() string {
	return ClockString(w.Start) + "-" + ClockString(w.End)
}

// Covers reports whether w contains every second of other.
func (w Window) Covers(other Window) bool {
	for sec := other.Start; sec != other.End; sec = (sec + 60) % daySeconds {
		if !w.ContainsSecond(sec) {
			return false
		}
	}
	// The last second before other.End.
	return w.ContainsSecond((other.End + daySeconds - 1) % daySeconds)
}

// ParseClock reads "HH:MM" (00:00 through 23:59) as seconds since midnight.
func ParseClock(v string) (int, error) {
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

// ClockString renders seconds since midnight as "HH:MM".
func ClockString(sec int) string {
	return fmt.Sprintf("%02d:%02d", sec/3600, sec%3600/60)
}

// Parse reads one window from its two bounds. Equal bounds are refused:
// they could mean "never" or "always".
func Parse(start, end string) (Window, error) {
	s, err := ParseClock(start)
	if err != nil {
		return Window{}, fmt.Errorf("start: %w", err)
	}
	e, err := ParseClock(end)
	if err != nil {
		return Window{}, fmt.Errorf("end: %w", err)
	}
	if s == e {
		return Window{}, fmt.Errorf("start and end are both %s: a window cannot be empty", strings.TrimSpace(start))
	}
	return Window{Start: s, End: e}, nil
}

// ParseList reads "HH:MM-HH:MM,HH:MM-HH:MM,...". Empty items are skipped;
// an empty list is no windows.
func ParseList(spec string) ([]Window, error) {
	var out []Window
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		bounds := strings.Split(item, "-")
		if len(bounds) != 2 {
			return nil, fmt.Errorf("window %q is not HH:MM-HH:MM", item)
		}
		w, err := Parse(bounds[0], bounds[1])
		if err != nil {
			return nil, fmt.Errorf("window %q: %w", item, err)
		}
		out = append(out, w)
	}
	return out, nil
}

// Set is a group of daily windows on the wall clock of one zone. It is
// evaluated against that wall clock for the instant given, so it follows
// daylight-saving changes: 16:00 means 16:00 on the local clock that day,
// whatever the UTC offset is.
type Set struct {
	Windows []Window
	Loc     *time.Location
}

// Contains reports whether the instant t falls inside any window. A Set
// with no location contains nothing.
func (s Set) Contains(t time.Time) bool {
	if s.Loc == nil {
		return false
	}
	h, m, sec := t.In(s.Loc).Clock()
	of := h*3600 + m*60 + sec
	for _, w := range s.Windows {
		if w.ContainsSecond(of) {
			return true
		}
	}
	return false
}

// blockStep is the resolution at which a span is checked against a Set. No
// window is shorter than a minute, so none can hide between two steps.
const blockStep = 15 * time.Second

// BlocksAt reports whether something started at t and lasting up to
// headroom could still be in progress inside a window: whether any instant
// from t to t + headroom is inside one.
func (s Set) BlocksAt(t time.Time, headroom time.Duration) bool {
	for d := time.Duration(0); d < headroom; d += blockStep {
		if s.Contains(t.Add(d)) {
			return true
		}
	}
	return s.Contains(t.Add(headroom))
}

// String renders the windows, e.g. "16:00-21:00,22:00-02:00".
func (s Set) String() string {
	parts := make([]string, 0, len(s.Windows))
	for _, w := range s.Windows {
		parts = append(parts, w.String())
	}
	return strings.Join(parts, ",")
}

// Schedule is several Sets, each in its own zone, that all apply.
type Schedule []Set

// BlocksAt reports whether any Set blocks t (see Set.BlocksAt).
func (sc Schedule) BlocksAt(t time.Time, headroom time.Duration) bool {
	for _, s := range sc {
		if s.BlocksAt(t, headroom) {
			return true
		}
	}
	return false
}

// nextClearHorizon bounds the search of NextClear. Daily windows repeat, so
// a schedule with no clear minute in two days has none.
const nextClearHorizon = 48 * time.Hour

// NextClear returns the first instant at or after t, at one-minute
// resolution, at which BlocksAt is false. ok is false when there is none
// within two days (the windows cover the whole day).
func (sc Schedule) NextClear(t time.Time, headroom time.Duration) (clear time.Time, ok bool) {
	if !sc.BlocksAt(t, headroom) {
		return t, true
	}
	// Whole minutes from here on: window bounds are whole minutes in every
	// zone whose offset is a multiple of a minute, which is all of them
	// since 1972.
	next := t.Truncate(time.Minute).Add(time.Minute)
	for end := t.Add(nextClearHorizon); next.Before(end); next = next.Add(time.Minute) {
		if !sc.BlocksAt(next, headroom) {
			return next, true
		}
	}
	return time.Time{}, false
}

// String renders every Set with its zone, e.g.
// "16:00-21:00 America/Los_Angeles; 02:00-03:00 UTC".
func (sc Schedule) String() string {
	parts := make([]string, 0, len(sc))
	for _, s := range sc {
		if s.Loc == nil || len(s.Windows) == 0 {
			continue
		}
		parts = append(parts, s.String()+" "+s.Loc.String())
	}
	return strings.Join(parts, "; ")
}
