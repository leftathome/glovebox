package psportal

import (
	"sync"
	"time"

	// The floor is evaluated in a named zone. Embedding the zone database
	// means that cannot fail, or silently become UTC, because an image
	// lacks /usr/share/zoneinfo (the connector's is distroless).
	_ "time/tzdata"
)

// The floor: the daily span in which nobody logs in to the account, and in
// which the connector does not ask for a login either.
//
// The portal keeps one live session per account and the account is its
// owner's own, so a login signs him out; he uses the portal between 16:00
// and 21:00 Pacific (owner decision, 2026-10-09; spec 16 section 4). These
// are constants. The login job can be configured to widen its own blackout
// around them; nothing can shorten them or move them to another zone.
const (
	FloorStart = "16:00"
	FloorEnd   = "21:00"
	FloorZone  = "America/Los_Angeles"

	floorStartSec = 16 * 3600
	floorEndSec   = 21 * 3600
)

// A login is not instantaneous: the browser may take up to LoginTimeout to
// reach the submit, so a login started at 15:59:30 could sign in at
// 16:00:30. No login starts, and none is asked for, unless the whole span
// from now to now + LoginHeadroom is clear of the floor.
const (
	LoginTimeout  = 90 * time.Second
	LoginMargin   = 60 * time.Second
	LoginHeadroom = LoginTimeout + LoginMargin
)

var (
	floorLocOnce sync.Once
	floorLoc     *time.Location
)

// FloorLocation returns America/Los_Angeles.
func FloorLocation() *time.Location {
	floorLocOnce.Do(func() {
		loc, err := time.LoadLocation(FloorZone)
		if err != nil {
			// Unreachable with time/tzdata linked in. A fixed offset would
			// be wrong for half the year, so fail loudly instead.
			panic("psportal: cannot load " + FloorZone + ": " + err.Error())
		}
		floorLoc = loc
	})
	return floorLoc
}

// InFloor reports whether the instant t is inside 16:00-21:00 Pacific,
// [start, end): 16:00:00 is inside, 21:00:00 is not. It follows the wall
// clock across daylight-saving changes.
func InFloor(t time.Time) bool {
	h, m, s := t.In(FloorLocation()).Clock()
	sec := h*3600 + m*60 + s
	return sec >= floorStartSec && sec < floorEndSec
}

// FloorBlocksLoginAt reports whether a login started (or asked for) at t
// could still be in progress inside the floor: whether any instant from t
// to t + LoginHeadroom is inside it. The last instant for which this is
// false before the floor is 15:57:29.
func FloorBlocksLoginAt(t time.Time) bool {
	const step = 15 * time.Second
	for d := time.Duration(0); d < LoginHeadroom; d += step {
		if InFloor(t.Add(d)) {
			return true
		}
	}
	return InFloor(t.Add(LoginHeadroom))
}
