package powerschool

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

func threeWindowConfig() Config {
	cfg := validConfig()
	cfg.Assignments.CourseDelayMS = 0
	cfg.PollSchedule.WeekdaysOnly = true
	cfg.PollSchedule.Windows = []PollWindow{
		{Start: "07:00", End: "07:30"},
		{Start: "15:45", End: "16:15"},
		{Start: "19:00", End: "19:30"},
	}
	return cfg
}

func losAngeles(t *testing.T) *time.Location {
	t.Helper()
	tz, err := loadTimezone("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	return tz
}

func TestWindowEnd(t *testing.T) {
	tz := losAngeles(t)
	cfg := threeWindowConfig()
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 12, h, m, s, 0, tz) }

	for _, tc := range []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"window start", at(7, 0, 0), at(7, 30, 0)},
		{"mid window", at(7, 12, 41), at(7, 30, 0)},
		{"last second", at(7, 29, 59), at(7, 30, 0)},
		{"window end is exclusive", at(7, 30, 0), at(7, 30, 0)},
		{"between windows", at(12, 0, 0), at(12, 0, 0)},
		{"evening window", at(19, 5, 0), at(19, 30, 0)},
	} {
		if got := windowEnd(cfg, tc.in, tz); !got.Equal(tc.want) {
			t.Errorf("%s: windowEnd(%s) = %s, want %s", tc.name, tc.in.Format("15:04:05"), got.Format("15:04:05"), tc.want.Format("15:04:05"))
		}
	}
}

func TestLastWindowStart(t *testing.T) {
	tz := losAngeles(t)
	cfg := threeWindowConfig()
	// 2026-10-12 is a Monday.
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, tz) }

	for _, tc := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"before the first window of Monday: Friday evening", at(12, 6, 0), at(9, 19, 0)},
		{"exactly at a window start", at(12, 7, 0), at(12, 7, 0)},
		{"inside the morning window", at(12, 7, 10), at(12, 7, 0)},
		{"between windows", at(12, 12, 0), at(12, 7, 0)},
		{"after the last window", at(12, 23, 0), at(12, 19, 0)},
		{"Saturday: Friday evening", at(17, 12, 0), at(16, 19, 0)},
		{"Sunday night: Friday evening", at(18, 23, 59), at(16, 19, 0)},
	} {
		if got := lastWindowStart(cfg, tc.now, tz); !got.Equal(tc.want) {
			t.Errorf("%s: lastWindowStart(%s) = %s, want %s", tc.name, tc.now.Format("Mon 15:04"), got.Format("Mon 01-02 15:04"), tc.want.Format("Mon 01-02 15:04"))
		}
	}

	every := threeWindowConfig()
	every.PollSchedule.WeekdaysOnly = false
	if got, want := lastWindowStart(every, at(17, 12, 0), tz), at(17, 7, 0); !got.Equal(want) {
		t.Errorf("weekends allowed: lastWindowStart(Sat noon) = %s, want %s", got, want)
	}
}

// TestScheduler_OnePollPerWindow walks the scheduling arithmetic for a full
// week under many seeds. Scheduling the next poll from the moment the last
// one fired re-rolls inside the same window and polls it again most of the
// time; scheduling from the end of that window must give exactly one poll
// per window, three per weekday, none at the weekend.
func TestScheduler_OnePollPerWindow(t *testing.T) {
	tz := losAngeles(t)
	cfg := threeWindowConfig()
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, tz) // a Monday
	end := start.AddDate(0, 0, 7)

	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		perDay := map[string]int{}
		perWindow := map[string]int{}
		now := start
		for {
			next, _ := computeNextPollTime(cfg, now, tz, rng)
			if !next.Before(end) {
				break
			}
			if next.Before(now) {
				t.Fatalf("seed %d: next %s is before now %s", seed, next, now)
			}
			if isWeekend(next) {
				t.Fatalf("seed %d: scheduled a weekend poll at %s", seed, next)
			}
			we := windowEnd(cfg, next, tz)
			if we.Equal(next) {
				t.Fatalf("seed %d: scheduled poll %s is outside every window", seed, next)
			}
			perDay[next.Format("2006-01-02")]++
			perWindow[we.Format(time.RFC3339)]++
			now = we // what Watch does via notBefore
		}
		assertWeek(t, seed, perDay, perWindow, 3)
	}
}

func assertWeek(t *testing.T, seed int64, perDay, perWindow map[string]int, pollsPerDay int) {
	t.Helper()
	if len(perDay) != 5 {
		t.Fatalf("seed %d: polled on %d days, want 5 weekdays (%v)", seed, len(perDay), perDay)
	}
	for day, n := range perDay {
		if n != pollsPerDay {
			t.Fatalf("seed %d: %s had %d polls, want %d", seed, day, n, pollsPerDay)
		}
	}
	for w, n := range perWindow {
		if n != 1 {
			t.Fatalf("seed %d: window ending %s polled %d times, want 1", seed, w, n)
		}
	}
}

// TestScheduler_FromFireTimeRepolls documents the defect windowEnd exists to
// prevent: scheduling from the fire time does revisit a window.
func TestScheduler_FromFireTimeRepolls(t *testing.T) {
	tz := losAngeles(t)
	cfg := threeWindowConfig()
	repolls := 0
	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		first, _ := computeNextPollTime(cfg, time.Date(2026, 10, 12, 6, 0, 0, 0, tz), tz, rng)
		second, _ := computeNextPollTime(cfg, first, tz, rng)
		if windowEnd(cfg, second, tz).Equal(windowEnd(cfg, first, tz)) {
			repolls++
		}
	}
	if repolls == 0 {
		t.Fatal("expected scheduling from the fire time to revisit the same window for some seeds; the regression this guards no longer reproduces")
	}
}

// simClock is a settable clock for the connector.
type simClock struct{ now time.Time }

func (s *simClock) Now() time.Time { return s.now }

// simulateWeek drives a real connector through one simulated week the way
// the framework does, and returns how many times the portal's home page was
// read per day and per window.
//
// Each "process" is a fresh connector on the shared checkpoint: the
// framework calls Poll once, then Watch. Watch's loop is replayed here step
// for step (scheduleFrom -> nextPoll -> advance the clock -> deferUntil ->
// pollNow) because the real one sleeps on a wall-clock timer. restartEvery
// > 0 kills the process after that many scheduled polls and also restarts
// it at a seeded random moment before the next one, including mid-window;
// the framework's periodic re-poll (a second Poll call) is issued after
// every scheduled poll.
func simulateWeek(t *testing.T, seed int64, restartEvery int) (perDay, perWindow map[string]int) {
	t.Helper()
	tz := losAngeles(t)
	cfg := threeWindowConfig()
	clock := &simClock{now: time.Date(2026, 10, 12, 0, 0, 0, 0, tz)} // a Monday
	end := clock.now.AddDate(0, 0, 7)
	chaos := rand.New(rand.NewSource(seed + 1_000_000))

	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	w, err := connector.NewStagingWriter(t.TempDir(), "powerschool")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	// An established deployment: the previous Friday's last window was
	// polled. (A first-ever start polls at once; see
	// TestPoll_RestartDoesNotRepoll.)
	prior := time.Date(2026, 10, 9, 19, 10, 0, 0, tz)
	if err := cp.Save(lastPollKey, prior.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed last poll: %v", err)
	}

	perDay, perWindow = map[string]int{}, map[string]int{}
	client := pollFixture("111111")
	// A poll makes several requests (one turn per kid). Any request at the
	// wrong time is a failure; a poll is counted once, by `observe`.
	inner := client.HomeFunc
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
		at := clock.now.In(tz)
		if isWeekend(at) {
			t.Fatalf("seed %d: portal read at the weekend: %s", seed, at)
		}
		if windowEnd(cfg, at, tz).Equal(at) {
			t.Fatalf("seed %d: portal read outside every window: %s", seed, at)
		}
		return inner(ctx)
	}
	// observe runs one framework entry point and records a poll at the
	// current simulated time if it reached the portal.
	observe := func(what string, call func() error) {
		before := client.HomeCalls
		if err := call(); err != nil {
			t.Fatalf("seed %d: %s: %v", seed, what, err)
		}
		if client.HomeCalls == before {
			return
		}
		at := clock.now.In(tz)
		perDay[at.Format("2006-01-02")]++
		perWindow[windowEnd(cfg, at, tz).Format(time.RFC3339)]++
	}

	ctx := context.Background()
	boot := func() *PowerSchoolConnector {
		c := wireConnector(t, client, cfg, w)
		c.now = clock.Now
		c.rng = rand.New(rand.NewSource(seed*7919 + chaos.Int63n(1000)))
		observe("startup Poll", func() error { return c.Poll(ctx, cp) }) // framework catch-up
		return c
	}

	c := boot()
	sinceRestart := 0
	for {
		next := c.nextPoll(c.scheduleFrom(clock.now))
		if !next.Before(end) {
			break
		}
		if next.Before(clock.now) {
			t.Fatalf("seed %d: next %s is before now %s", seed, next, clock.now)
		}

		// Sometimes the process is replaced while it waits.
		if restartEvery > 0 && chaos.Intn(3) == 0 {
			gap := next.Sub(clock.now)
			if gap > time.Second {
				clock.now = clock.now.Add(time.Duration(chaos.Int63n(int64(gap))))
				c = boot()
				sinceRestart = 0
				continue // the new process schedules for itself
			}
		}

		clock.now = next
		c.deferUntil(windowEnd(cfg, next, tz))
		observe("scheduled poll", func() error { return c.pollNow(ctx, cp, "scheduled") })
		// The framework's interval re-poll must never reach the portal.
		observe("framework re-poll", func() error { return c.Poll(ctx, cp) })

		sinceRestart++
		if restartEvery > 0 && sinceRestart >= restartEvery {
			// Crash right after a poll, still inside its window, and once
			// more a little later.
			c = boot()
			clock.now = clock.now.Add(time.Duration(chaos.Int63n(int64(20 * time.Minute))))
			c = boot()
			sinceRestart = 0
		}
	}
	return perDay, perWindow
}

// TestConnector_SimulatedWeek_OnePollPerWindow: across many seeds, one
// process running all week reads the portal exactly three times per
// weekday -- once per window -- whatever the framework's re-poll does.
func TestConnector_SimulatedWeek_OnePollPerWindow(t *testing.T) {
	captureLogs(t) // keep the simulated polls out of the test output
	for seed := int64(0); seed < 60; seed++ {
		perDay, perWindow := simulateWeek(t, seed, 0)
		assertWeek(t, seed, perDay, perWindow, 3)
	}
}

// TestConnector_SimulatedWeek_RestartsDoNotAddPolls: the same week with the
// process restarted over and over -- right after a poll, mid-window, between
// windows, at the weekend. Restarts must not add a single portal read, and
// because every restart here happens before or after a window's poll
// (never instead of it) none is lost either.
func TestConnector_SimulatedWeek_RestartsDoNotAddPolls(t *testing.T) {
	captureLogs(t)
	for seed := int64(0); seed < 60; seed++ {
		perDay, perWindow := simulateWeek(t, seed, 2)
		assertWeek(t, seed, perDay, perWindow, 3)
	}
}

// TestPoll_GatedToFirstCallPerProcess: the framework's later Poll calls
// never reach the portal.
func TestPoll_GatedToFirstCallPerProcess(t *testing.T) {
	client := pollFixture("111111")
	c, cp, _ := newWiredConnector(t, client, testConfig())
	ctx := context.Background()
	if err := c.Poll(ctx, cp); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	afterFirst := client.HomeCalls
	if afterFirst == 0 {
		t.Fatal("the first Poll did not read the portal")
	}
	for i := 0; i < 5; i++ {
		if err := c.Poll(ctx, cp); err != nil {
			t.Fatalf("Poll %d: %v", i, err)
		}
	}
	if client.HomeCalls != afterFirst || len(client.AssignmentsCalls) != 2 {
		t.Errorf("framework re-polls reached the portal: %d home reads (was %d), %d lookups",
			client.HomeCalls, afterFirst, len(client.AssignmentsCalls))
	}
}

// TestPoll_RestartDoesNotRepoll: a new process on the same state directory
// polls on startup only when a window has opened since the last poll.
func TestPoll_RestartDoesNotRepoll(t *testing.T) {
	cfg := testConfig() // one window, 16:30-18:00, every day
	tz := losAngeles(t)
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	w, err := connector.NewStagingWriter(t.TempDir(), "powerschool")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	client := pollFixture("111111")
	ctx := context.Background()
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, tz) }

	startAt := func(now time.Time) *PowerSchoolConnector {
		c := wireConnector(t, client, cfg, w)
		c.now = func() time.Time { return now }
		if err := c.Poll(ctx, cp); err != nil {
			t.Fatalf("Poll at %s: %v", now, err)
		}
		return c
	}

	polls := 0
	steps := []struct {
		name      string
		now       time.Time
		wantPolls int
	}{
		{"first ever start, mid-window: polls", at(7, 17, 0), 1},
		{"restart 20 minutes later, same window: no poll", at(7, 17, 20), 1},
		{"restart that evening: no poll", at(7, 21, 0), 1},
		{"restart next morning, before the window: no poll", at(8, 9, 0), 1},
		{"restart after sleeping through the next window: polls once", at(8, 20, 0), 2},
		{"and again an hour later: no poll", at(8, 21, 0), 2},
	}
	for _, s := range steps {
		before := client.HomeCalls
		c := startAt(s.now)
		if client.HomeCalls != before {
			polls++
		}
		if polls != s.wantPolls {
			t.Fatalf("%s: %d polls so far, want %d", s.name, polls, s.wantPolls)
		}
		// Whatever happened, the schedule must not come back to the window
		// that has been polled: it starts no earlier than that window's
		// end.
		last, ok := loadLastPoll(cp)
		if !ok {
			t.Fatalf("%s: no last-poll time in the checkpoint", s.name)
		}
		if from := c.scheduleFrom(s.now); from.Before(windowEnd(cfg, last, tz)) {
			t.Errorf("%s: next poll is scheduled from %s, inside the window polled at %s",
				s.name, from.Format(time.RFC3339), last.In(tz).Format(time.RFC3339))
		}
	}
}
