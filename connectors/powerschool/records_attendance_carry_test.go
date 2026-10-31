package powerschool

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// Decision S16 (Steve 2026-10-29): days the grid could not settle before
// it rolled to the next term are carried in the kid's state and settled by
// the next term's school days.

// The next term's first week.
var (
	nMon, nTue, nWed = day(2026, 10, 19), day(2026, 10, 20), day(2026, 10, 21)
)

// carry runs one poll of the builder with carried days and checks every
// snapshot against the journal.
func carry(t *testing.T, b Binding, page *pslib.AttendancePage, settle int, today time.Time, in []pendingDay) ([]journalapi.Snapshot, []pendingDay) {
	t.Helper()
	snaps, out := buildAttendanceDaysCarry(b, page, defaultDaysConfig(), settle, at(t, today), testTZ(t), at(t, today), in)
	validateAll(t, snaps)
	return snaps, out
}

// carriedSnapshot returns the one complete=false attendance_day snapshot.
func carriedSnapshot(t *testing.T, snaps []journalapi.Snapshot) journalapi.Snapshot {
	t.Helper()
	var out []journalapi.Snapshot
	for _, s := range snaps {
		if !s.Complete {
			out = append(out, s)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d carried snapshots, want 1", len(out))
	}
	s := out[0]
	if s.CompleteRange != nil || s.RecordType != TypeAttendanceDay {
		t.Fatalf("carried snapshot %+v", s)
	}
	return s
}

func noCarriedSnapshot(t *testing.T, snaps []journalapi.Snapshot) {
	t.Helper()
	for _, s := range snaps {
		if !s.Complete {
			t.Fatalf("carried snapshot with keys %v", keysOfSnap(s))
		}
	}
}

func pendingDates(days []pendingDay) []string {
	var out []string
	for _, d := range days {
		out = append(out, d.Date)
	}
	return out
}

// The last day of a term, fully marked, is polled on that day (today is
// never settled). After the grid rolls to the next term it settles at the
// next poll, in its own complete=false snapshot: the next term's range is
// not widened over the break.
func TestCarryFullyMarkedLastDayOfTermSettlesAfterRollover(t *testing.T) {
	term1 := gridPage([]time.Time{thu, fri}, []string{"", "UA"}, []string{"", "EA"})
	snaps, pending := carry(t, bind1, term1, 3, fri, nil)
	noCarriedSnapshot(t, snaps)
	if !reflect.DeepEqual(pendingDates(pending), []string{"2026-10-09"}) {
		t.Fatalf("pending %v", pendingDates(pending))
	}

	term2 := gridPage([]time.Time{nMon, nTue}, []string{"", ""}, []string{"", ""})
	snaps, pending = carry(t, bind1, term2, 3, nTue, pending)
	if len(pending) != 0 {
		t.Fatalf("still pending %v", pendingDates(pending))
	}
	s := carriedSnapshot(t, snaps)
	wantScope(t, s, "powerschool", "attendance_days", "e_child1", "school_year", "2026-27")
	wantJSON(t, fieldsOf(t, s, "2026-10-09"), map[string]string{"date": `"2026-10-09"`, "periods_met": "2",
		"absent_unexcused": "1", "absent_excused": "1", "any_unexcused": "true", "classification": `"full_day"`})
	// The next term's own complete snapshot starts at its first date.
	for _, c := range snaps {
		if c.Complete {
			wantRange(t, c, "2026-10-19", "2026-10-19")
		}
	}
}

// A day with an unmarked cell in the last days of a term needs `settle`
// school days after it, counted across the boundary: Thursday's grid
// followed it with Friday only, so the next term's Monday and Tuesday
// complete the three.
func TestCarryUnmarkedDaySettlesWithNextTermSchoolDays(t *testing.T) {
	term1 := gridPage([]time.Time{thu, fri}, []string{"UA", ""}, []string{".", "-"})
	snaps, pending := carry(t, bind1, term1, 3, fri, nil)
	noCarriedSnapshot(t, snaps)
	if !reflect.DeepEqual(pendingDates(pending), []string{"2026-10-08"}) {
		t.Fatalf("pending %v", pendingDates(pending))
	}
	// Next term, Monday: two school days have followed (Fri, Mon).
	snaps, pending = carry(t, bind1, gridPage([]time.Time{nMon}, []string{""}), 3, nMon, pending)
	noCarriedSnapshot(t, snaps)
	if len(pending) != 1 {
		t.Fatalf("pending %v", pendingDates(pending))
	}
	// A no-meeting day is not a school day; a second poll on the same day
	// counts nothing twice.
	snaps, pending = carry(t, bind1, gridPage([]time.Time{nMon, nTue}, []string{"", "-"}), 3, nTue, pending)
	noCarriedSnapshot(t, snaps)
	snaps, pending = carry(t, bind1, gridPage([]time.Time{nMon, nTue}, []string{"", "-"}), 3, nTue, pending)
	noCarriedSnapshot(t, snaps)
	if len(pending) != 1 {
		t.Fatalf("pending %v", pendingDates(pending))
	}
	// Wednesday is the third school day.
	snaps, pending = carry(t, bind1, gridPage([]time.Time{nMon, nTue, nWed}, []string{"", "-", ""}), 3, nWed, pending)
	if len(pending) != 0 {
		t.Fatalf("still pending %v", pendingDates(pending))
	}
	s := carriedSnapshot(t, snaps)
	wantJSON(t, fieldsOf(t, s, "2026-10-08"), map[string]string{"periods_met": "2", "periods_unmarked": "1",
		"classification": `"full_day"`})
}

// A carried day still in the grid is taken from the grid: a code removed
// there is no record and is not carried any more.
func TestCarryCorrectionWhileStillInTheGrid(t *testing.T) {
	_, pending := carry(t, bind1, gridPage([]time.Time{thu, fri}, []string{"", "UA"}), 3, fri, nil)
	if len(pending) != 1 {
		t.Fatalf("pending %v", pendingDates(pending))
	}
	// Monday, same term: Friday's code was corrected away.
	snaps, pending := carry(t, bind1, gridPage([]time.Time{thu, fri, nextMon}, []string{"", "", ""}), 3, nextMon, pending)
	noCarriedSnapshot(t, snaps)
	if len(pending) != 0 {
		t.Fatalf("a corrected day is still carried: %v", pendingDates(pending))
	}
	if len(snaps) != 1 || len(snaps[0].Records) != 0 {
		t.Fatalf("snapshots %+v", snaps)
	}
	wantRange(t, snaps[0], "2026-10-08", "2026-10-09")

	// And a code changed in the grid replaces the carried tally.
	_, pending = carry(t, bind1, gridPage([]time.Time{thu, fri}, []string{"", "UA"}), 3, fri, nil)
	snaps, _ = carry(t, bind1, gridPage([]time.Time{thu, fri, nextMon}, []string{"", "EA"}), 3, nextMon, pending)
	noCarriedSnapshot(t, snaps)
	wantJSON(t, fieldsOf(t, snaps[0], "2026-10-09"), map[string]string{"absent_excused": "1", "absent_unexcused": "0"})
}

// A carried day that never settles is dropped after the horizon, counted
// and logged with the record type only.
func TestCarryExpiresAfterTheHorizon(t *testing.T) {
	_, pending := carry(t, bind1, gridPage([]time.Time{thu, fri}, []string{".", ""}, []string{"UA", "-"}), 3, fri, nil)
	if len(pending) != 1 {
		t.Fatalf("pending %v", pendingDates(pending))
	}
	var skipped []string
	b := bind1
	b.OnSkip = func(r string) { skipped = append(skipped, r) }
	logs := captureLogs(t)
	// Thirty days later, still no school day after it (a break of "-").
	day30 := thu.AddDate(0, 0, carryHorizonDays)
	_, kept := carry(t, b, gridPage([]time.Time{day30}, []string{"-"}), 3, day30, pending)
	if len(kept) != 1 || len(skipped) != 0 {
		t.Fatalf("dropped on the horizon's last day: kept %v skipped %v", pendingDates(kept), skipped)
	}
	day31 := day30.AddDate(0, 0, 1)
	snaps, kept := carry(t, b, gridPage([]time.Time{day31}, []string{"-"}), 3, day31, kept)
	noCarriedSnapshot(t, snaps)
	if len(kept) != 0 || !reflect.DeepEqual(skipped, []string{"attendance_day_carry_expired"}) {
		t.Fatalf("kept %v skipped %v", pendingDates(kept), skipped)
	}
	if out := logs.String(); !strings.Contains(out, "record_type=attendance_day") || strings.Contains(out, "2026-10-08") {
		t.Fatalf("log: %s", out)
	}
}

// Carried days are in the kid's checkpoint, survive a restart, and settle
// in the next process. Records off: nothing is written.
func TestCarrySurvivesARestart(t *testing.T) {
	stateDir := t.TempDir()
	pollAt := func(today time.Time, page func(selected string) *pslib.AttendancePage) []journalapi.Snapshot {
		t.Helper()
		cp, err := connector.NewCheckpoint(filepath.Join(stateDir, "cp"))
		if err != nil {
			t.Fatal(err)
		}
		client := extraFixture("111111")
		client.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
			return page(client.Selected), nil
		}
		sink := &recorderSink{}
		w, _ := connector.NewStagingWriter(t.TempDir(), "powerschool")
		c := newRecordsConnector(t, client, recordsConfig(), w, func() (RecordSink, error) { return sink, nil })
		c.now = func() time.Time { return at(t, today) }
		captureLogs(t)
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatal(err)
		}
		return sink.all()
	}
	term1 := func(sel string) *pslib.AttendancePage {
		p := gridPage([]time.Time{thu, fri}, []string{"", "UA"})
		p.Student = pageStudentOf(sel)
		return p
	}
	term2 := func(sel string) *pslib.AttendancePage {
		p := gridPage([]time.Time{nMon}, []string{""})
		p.Student = pageStudentOf(sel)
		return p
	}
	first := pollAt(fri, term1)
	for _, s := range snapsOfType(first, TypeAttendanceDay) {
		if !s.Complete {
			t.Fatal("carried snapshot on the last day of term")
		}
	}
	cp, _ := connector.NewCheckpoint(filepath.Join(stateDir, "cp"))
	if raw, ok := cp.Load(pendingDaysKey("k1")); !ok || !strings.Contains(raw, "2026-10-09") {
		t.Fatalf("k1's carried days not in its state: %q", raw)
	}
	// A new process on the same state, after the rollover.
	second := pollAt(nMon, term2)
	carried := 0
	for _, s := range snapsOfType(second, TypeAttendanceDay) {
		if !s.Complete {
			carried++
			if len(s.Records) != 1 || s.Records[0].Key != "2026-10-09" {
				t.Fatalf("carried keys %v", keysOfSnap(s))
			}
		}
	}
	if carried != 2 {
		t.Fatalf("%d carried snapshots, want one per kid", carried)
	}
	cp, _ = connector.NewCheckpoint(filepath.Join(stateDir, "cp"))
	if raw, ok := cp.Load(pendingDaysKey("k1")); ok && strings.Contains(raw, "2026-10-09") {
		t.Fatalf("an emitted day is still carried: %q", raw)
	}
}

// Records off: a poll that would carry a day writes no records state.
func TestCarryNothingWithRecordsOff(t *testing.T) {
	client := extraFixture("111111")
	client.AttendanceFunc = func(ctx context.Context) (*pslib.AttendancePage, error) {
		p := gridPage([]time.Time{thu, fri}, []string{"", "UA"})
		p.Student = pageStudentOf(client.Selected)
		return p, nil
	}
	for _, on := range []bool{false, true} {
		cfg := recentConfigOnRecordRules()
		var mk func() (RecordSink, error)
		if on {
			cfg = recordsConfig()
			mk = func() (RecordSink, error) { return &recorderSink{}, nil }
		}
		cp := newMemCheckpoint()
		w, _ := connector.NewStagingWriter(t.TempDir(), "powerschool")
		c := newRecordsConnector(t, client, cfg, w, mk)
		c.now = func() time.Time { return at(t, fri) }
		captureLogs(t)
		if err := c.pollNow(context.Background(), cp, "scheduled"); err != nil {
			t.Fatal(err)
		}
		if _, written := cp.Load(pendingDaysKey("k1")); written != on {
			t.Fatalf("records on=%v: carried state written=%v", on, written)
		}
	}
}

// State from before this field (no key) or an unreadable value loads as
// nothing carried.
func TestCarryStateLoadsCleanly(t *testing.T) {
	cp := newMemCheckpoint()
	if got := loadPendingDays(cp, "k1"); len(got) != 0 {
		t.Fatalf("no key: %v", got)
	}
	_ = cp.Save(pendingDaysKey("k1"), "not json")
	captureLogs(t)
	if got := loadPendingDays(cp, "k1"); len(got) != 0 {
		t.Fatalf("bad value: %v", got)
	}
	in := []pendingDay{{Date: "2026-10-09", Met: 2, AbsUnex: 1, AnyUnex: true, Followed: 1, Through: "2026-10-09"}}
	if err := savePendingDays(cp, "k1", in); err != nil {
		t.Fatal(err)
	}
	if got := loadPendingDays(cp, "k1"); !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip %+v", got)
	}
	if err := savePendingDays(cp, "k1", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := cp.Load(pendingDaysKey("k1")); ok {
		t.Fatal("an empty carried set leaves a key behind")
	}
}
