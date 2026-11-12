package schoology

import (
	"testing"
	"time"
)

// progressIdle and progressCap bound waitForProgress, which replaced the fixed
// 2 minute emitterWait. The emitter delivers over real HTTP with a spool delete
// and directory fsync per answer, so on a saturated CI node it is slow but
// moving. The wait gives up when
// the watched value has not moved for progressIdle, or after progressCap in
// all. A slow but steady emitter (CI nodes saturated by image builds) keeps
// resetting the idle window, so it is never failed for being slow
// (glovebox-062i; pipeline 4475).
const (
	progressIdle = 2 * time.Minute
	progressCap  = 10 * time.Minute
)

// waitForProgress polls get every 20ms until it reports at least want. It
// returns false, after logging the value and elapsed time, only when the value
// has not increased for progressIdle or progressCap has passed. Callers keep
// their own detailed assertions for the false case. A quantity that falls
// (a draining spool) is passed negated, with want 0.
func waitForProgress(t *testing.T, what string, get func() int64, want int64) bool {
	t.Helper()
	return waitForProgressWith(t, what, get, want, progressIdle, progressCap)
}

func waitForProgressWith(t *testing.T, what string, get func() int64, want int64, idle, hardCap time.Duration) bool {
	t.Helper()
	start := time.Now()
	last := get()
	lastMove := start
	for last < want {
		time.Sleep(20 * time.Millisecond)
		now := time.Now()
		v := get()
		if v > last {
			last, lastMove = v, now
		}
		if last >= want {
			break
		}
		if now.Sub(lastMove) >= idle || now.Sub(start) >= hardCap {
			t.Logf("%s: gave up at %d, want %d, after %s (idle %s, cap %s)",
				what, last, want, now.Sub(start).Round(time.Millisecond), idle, hardCap)
			return false
		}
	}
	return true
}

func TestWaitForProgressSlowSteadySucceeds(t *testing.T) {
	start := time.Now()
	// One step per ~60ms, 10 steps: 600ms in all, past the 200ms idle
	// window taken alone, because every step resets it.
	get := func() int64 { return int64(time.Since(start) / (60 * time.Millisecond)) }
	if !waitForProgressWith(t, "slow", get, 10, 200*time.Millisecond, 5*time.Second) {
		t.Fatal("steady progress failed")
	}
}

func TestWaitForProgressStallFailsOnIdle(t *testing.T) {
	start := time.Now()
	get := func() int64 {
		if time.Since(start) < 100*time.Millisecond {
			return int64(time.Since(start) / (50 * time.Millisecond))
		}
		return 1 // stalled
	}
	if waitForProgressWith(t, "stall", get, 10, 200*time.Millisecond, 5*time.Second) {
		t.Fatal("stalled wait succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s, want idle failure well before the cap", d)
	}
}

func TestWaitForProgressHardCap(t *testing.T) {
	var n int64
	get := func() int64 { n++; return n / 100000 } // keeps creeping, never arrives
	if waitForProgressWith(t, "cap", get, 1<<40, time.Hour, 100*time.Millisecond) {
		t.Fatal("capped wait succeeded")
	}
}
