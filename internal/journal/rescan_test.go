package journal

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// releasingScanner passes everything: the "upgrade" that clears a false
// positive.
type releasingScanner struct{}

func (releasingScanner) Verdict(string) string { return record.VerdictPass }
func (releasingScanner) Version() string       { return "scanner-2" }

// funcScanner is scanner-2 with a verdict function and an optional hook that
// runs before every verdict (the scan phase of a rescan, or an Apply).
type funcScanner struct {
	mu      sync.Mutex
	version string
	calls   int
	verdict func(text string) string
	hook    func(call int)
}

func newFuncScanner(verdict func(string) string) *funcScanner {
	return &funcScanner{version: "scanner-2", verdict: verdict}
}

func (s *funcScanner) Version() string { return s.version }
func (s *funcScanner) Verdict(text string) string {
	s.mu.Lock()
	s.calls++
	n, hook := s.calls, s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return s.verdict(text)
}

func (s *funcScanner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func passAll(string) string { return record.VerdictPass }

// injectAware withholds INJECT, as the rig's fake scanner does.
func injectAware(text string) string {
	if strings.Contains(text, "INJECT") {
		return "quarantine"
	}
	return record.VerdictPass
}

// firstOnly lets a hook act once even when the action scans again.
type firstOnly struct{ v atomic.Bool }

func (f *firstOnly) first() bool { return f.v.CompareAndSwap(false, true) }

func rescanCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func (r *rig) rescan(t *testing.T, all bool) journalapi.RescanResult {
	t.Helper()
	res, err := r.j.Rescan(rescanCtx(t), all)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (r *rig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.j.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// commentVerdict is the verdict stored for the comment of one record.
func (r *rig) commentVerdict(subject, key string) record.ScanEntry {
	r.t.Helper()
	c, ok := r.current("assignment", subject, key)
	if !ok {
		r.t.Fatalf("no row %s", key)
	}
	return c.Scan["comment"]
}

func evScan(e eventView) map[string]record.EventScan {
	var s map[string]record.EventScan
	_ = json.Unmarshal(e.Scan, &s)
	return s
}

func TestRescanReleasesValuesInBothTables(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT again"}`)))
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, false)
	if res.CurrentRows != 1 || res.EventRows != 2 || res.Released != 4 || res.Withheld != 0 {
		t.Errorf("%+v", res) // current comment; event 1 new; event 2 old and new
	}
	c, _ := r.current("assignment", "e_child1", "k")
	if c.Scan["comment"] != (record.ScanEntry{Verdict: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("current %+v", c.Scan)
	}
	ev := r.events("e_child1")
	if s := evScan(ev[1])["comment"]; s != (record.EventScan{Old: "pass", New: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("updated event scan %s", ev[1].Scan)
	}
	if s := evScan(ev[0])["comment"]; s != (record.EventScan{New: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("created event scan %s", ev[0].Scan)
	}
	again := r.rescan(t, false)
	if again.CurrentRows != 0 || again.EventRows != 0 {
		t.Errorf("values already judged by this version were rescanned: %+v", again)
	}
	all := r.rescan(t, true)
	if all.CurrentRows != 1 || all.EventRows != 2 || all.Released != 0 {
		t.Errorf("all: %+v", all)
	}
}

func TestRescanCoversValuesWithAVerdictWhateverTheActiveType(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A"}`)))
	r.exec(t, `UPDATE record_current SET scan = '{"title": {"verdict": "quarantine", "scanner_version": "old"}}'`)
	r.exec(t, `UPDATE record_event SET scan = '{"title": {"new": "quarantine", "scanner_version": "old"}}'`)
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, false)
	if res.CurrentRows != 1 || res.EventRows != 1 || res.Released != 2 { // current and event
		t.Errorf("%+v", res)
	}
	if got := evScan(r.events("e_child1")[0])["title"]; got != (record.EventScan{New: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("event scan %+v", got)
	}
}

// An Apply that starts while a row is locked waits for it (the rows are read
// FOR UPDATE) and keeps the verdict written meanwhile for a value it does
// not change.
func TestApplyWaitsForALockedRowAndKeepsItsVerdicts(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT"}`)))
	tx, err := r.j.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT 1 FROM record_current WHERE key = 'k' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		s := snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "B", "comment": "INJECT"}`))
		n, rej := record.ValidateSnapshot(r.reg, s, T0)
		if rej != nil {
			done <- rej
			return
		}
		_, err := r.j.Apply(ctx, writer, n, []byte(`{}`))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Apply did not wait for the locked row: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(`UPDATE record_current SET scan = '{"comment": {"verdict": "pass", "scanner_version": "scanner-2"}}' WHERE key = 'k'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c, _ := r.current("assignment", "e_child1", "k")
	if string(c.Fields["title"]) != `"B"` || c.Scan["comment"] != (record.ScanEntry{Verdict: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("the other writer's verdict was overwritten: %+v", c)
	}
}

func TestRescanCanWithholdAndCoversRemovedEvents(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "now suspicious INJECT"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true))
	r.scanner.SetVersion("scanner-1b")
	// The fake withholds INJECT both times; flip the stored verdicts to pass
	// first, as if an older scanner had passed them.
	r.exec(t, `UPDATE record_event SET scan = replace(scan::text, 'quarantine', 'pass')::jsonb`)
	res := r.rescan(t, false)
	if res.Withheld != 2 || res.EventRows != 2 { // the created value and the removed event's last value
		t.Errorf("%+v", res)
	}
	ev := r.events("e_child1")
	if s := evScan(ev[0])["comment"]; s != (record.EventScan{New: "quarantine", ScannerVersion: "scanner-1b"}) {
		t.Errorf("created %s", ev[0].Scan)
	}
	if s := evScan(ev[1])["comment"]; ev[1].Kind != "removed" || s != (record.EventScan{Old: "quarantine", ScannerVersion: "scanner-1b"}) {
		t.Errorf("removed %s", ev[1].Scan)
	}
}

// Values are selected one by one by the scanner version of their verdict, not
// by the verdict: a passing value of an older version is judged again (and
// may be withheld), a failing value of the current version is left alone
// unless all is asked for.
func TestRescanSelectsByScannerVersionPerValue(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true,
		rec("old_pass", "e_child1", `{"comment": "fine"}`),
		rec("new_fail", "e_child1", `{"comment": "fine"}`),
		rec("mixed", "e_child1", `{"title": "t", "comment": "fine"}`)))
	r.exec(t, `UPDATE record_current SET scan = '{"comment": {"verdict": "pass", "scanner_version": "scanner-1"}}' WHERE key = 'old_pass'`)
	r.exec(t, `UPDATE record_current SET scan = '{"comment": {"verdict": "quarantine", "scanner_version": "scanner-2"}}' WHERE key = 'new_fail'`)
	r.exec(t, `UPDATE record_current SET scan = '{"comment": {"verdict": "quarantine", "scanner_version": "scanner-2"},
		"title": {"verdict": "quarantine", "scanner_version": "scanner-1"}}' WHERE key = 'mixed'`)
	r.j.o.Scanner = newFuncScanner(passAll)
	// old_pass (comment v1) and mixed (title v1) are judged; new_fail and
	// mixed.comment come from the current version and are not.
	res := r.rescan(t, false)
	if res.CurrentRows != 2 || res.EventRows != 3 || res.Released != 1 || res.Withheld != 0 {
		t.Errorf("%+v", res)
	}
	if got := r.commentVerdict("e_child1", "old_pass"); got != (record.ScanEntry{Verdict: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("old_pass %+v", got)
	}
	if got := r.commentVerdict("e_child1", "new_fail"); got != (record.ScanEntry{Verdict: "quarantine", ScannerVersion: "scanner-2"}) {
		t.Errorf("a value judged by the current version was rescanned: %+v", got)
	}
	m, _ := r.current("assignment", "e_child1", "mixed")
	if m.Scan["comment"] != (record.ScanEntry{Verdict: "quarantine", ScannerVersion: "scanner-2"}) ||
		m.Scan["title"] != (record.ScanEntry{Verdict: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("mixed %+v", m.Scan)
	}
	// A new version judges passing values again, whatever their verdict.
	s3 := newFuncScanner(func(string) string { return "quarantine" })
	s3.version = "scanner-3"
	r.j.o.Scanner = s3
	res = r.rescan(t, false)
	if res.CurrentRows != 3 || res.EventRows != 3 || res.Withheld != 5 || res.Released != 0 { // old_pass.comment, mixed.title, three events
		t.Errorf("%+v", res)
	}
	if again := r.rescan(t, false); again.CurrentRows != 0 || again.EventRows != 0 {
		t.Errorf("%+v", again)
	}
	if all := r.rescan(t, true); all.CurrentRows != 3 || all.Withheld != 0 {
		t.Errorf("all rescans every value: %+v", all)
	}
}

// An event's values are skipped one by one too: the verdicts of a field the
// current version already judged stay as they are while its sibling is
// rescanned.
func TestRescanKeepsTheVerdictsOfEventFieldsItSkips(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "x"}`)))
	r.exec(t, `UPDATE record_event SET scan = '{"comment": {"new": "quarantine", "scanner_version": "old"},
		"title": {"new": "quarantine", "scanner_version": "scanner-2"}}'`)
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, false)
	if res.EventRows != 1 || res.Released != 1 {
		t.Errorf("%+v", res)
	}
	s := evScan(r.events("e_child1")[0])
	if s["comment"] != (record.EventScan{New: "pass", ScannerVersion: "scanner-2"}) ||
		s["title"] != (record.EventScan{New: "quarantine", ScannerVersion: "scanner-2"}) {
		t.Errorf("event scan %+v", s)
	}
}

// Only an exact "pass" releases; an empty answer, a panic and any other word
// withhold.
func TestRescanOnlyPassReleases(t *testing.T) {
	r := newRig(t)
	cases := []string{"pass", "empty", "panic", "quarantine", "PASS", "error"}
	var recs []journalapi.Record
	for _, c := range cases {
		recs = append(recs, rec(c, "e_child1", fmt.Sprintf(`{"comment": "INJECT %s"}`, c)))
	}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, recs...))
	r.j.o.Scanner = newFuncScanner(func(text string) string {
		switch strings.TrimPrefix(text, "INJECT ") {
		case "pass":
			return "pass"
		case "empty":
			return ""
		case "panic":
			panic("scanner bug")
		case "PASS":
			return "PASS"
		case "error":
			return "error"
		}
		return "quarantine"
	})
	res := r.rescan(t, false)
	if res.Released != 2 || res.Withheld != 0 || res.CurrentRows != len(cases) || res.EventRows != len(cases) { // pass: current and event
		t.Errorf("%+v", res)
	}
	want := map[string]string{"pass": "pass", "empty": "error", "panic": "error", "quarantine": "quarantine", "PASS": "PASS", "error": "error"}
	for k, v := range want {
		if got := r.commentVerdict("e_child1", k).Verdict; got != v {
			t.Errorf("%s: verdict %q, want %q", k, got, v)
		}
	}
	for _, e := range r.events("e_child1") {
		if got := evScan(e)["comment"].New; got != want[e.Key] {
			t.Errorf("event %s: %q, want %q", e.Key, got, want[e.Key])
		}
	}
}

// A previously passing value that now fails is withheld, and an error is
// not counted as a withholding of a value that was never released.
func TestRescanCountsOnlyChangesOfRelease(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true,
		rec("was_pass", "e_child1", `{"comment": "fine"}`),
		rec("was_fail", "e_child1", `{"comment": "INJECT"}`)))
	r.j.o.Scanner = newFuncScanner(func(string) string { return "quarantine" })
	res := r.rescan(t, false)
	if res.Withheld != 2 || res.Released != 0 { // was_pass: current and event
		t.Errorf("%+v", res)
	}
}

// A value that is not a string cannot be judged and is withheld.
func TestRescanWithholdsAValueThatIsNotAString(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT x"}`)))
	r.exec(t, `UPDATE record_current SET fields = '{"comment": 5}'`)
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, false)
	var got string
	if err := r.j.db.QueryRow(`SELECT scan->'comment'->>'verdict' FROM record_current`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "error" || res.Released != 1 { // only the event's string is released
		t.Errorf("verdict %q %+v", got, res)
	}
}

// A type the registry does not know has no field kinds: every field counts
// as text, as on read, so title (a string field of a known type) is judged.
func TestRescanJudgesEveryFieldOfAnUnknownType(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "x"}`)))
	r.exec(t, `UPDATE record_current SET record_type = 'ghost', scan = '{}'`)
	r.exec(t, `ALTER TABLE record_event DISABLE TRIGGER USER`) // the append-only guard forbids the retyping below
	r.exec(t, `UPDATE record_event SET record_type = 'ghost', scan = '{}'`)
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, false)
	if res.CurrentRows != 1 || res.EventRows != 1 || res.Released != 4 { // title and comment, current and event
		t.Errorf("%+v", res)
	}
}

func TestRescanBatches(t *testing.T) {
	for _, n := range []int{99, 100, 101, 250} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r := newRig(t)
			var recs []journalapi.Record
			for i := 0; i < n; i++ {
				recs = append(recs, rec(fmt.Sprintf("k%04d", i), "e_child1", `{"comment": "INJECT"}`))
			}
			r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, recs...))
			r.j.o.Scanner = releasingScanner{}
			res := r.rescan(t, false)
			if res.CurrentRows != n || res.EventRows != n || res.Released != 2*n {
				t.Errorf("%+v", res)
			}
			var notPass int
			if err := r.j.db.QueryRow(`SELECT (SELECT count(*) FROM record_current WHERE scan->'comment'->>'verdict' <> 'pass')
				+ (SELECT count(*) FROM record_event WHERE scan->'comment'->>'new' <> 'pass')`).Scan(&notPass); err != nil || notPass != 0 {
				t.Errorf("%d values left unjudged: %v", notPass, err)
			}
		})
	}
}

// An interrupted rescan keeps the batches it committed, and the next one
// continues with what is left without judging a value twice.
func TestRescanResumesAfterAnInterruption(t *testing.T) {
	r := newRig(t)
	var recs []journalapi.Record
	for i := 0; i < 250; i++ {
		recs = append(recs, rec(fmt.Sprintf("k%04d", i), "e_child1", `{"comment": "INJECT"}`))
	}
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, recs...))
	c, cancel := context.WithCancel(rescanCtx(t))
	defer cancel()
	s := newFuncScanner(passAll)
	s.hook = func(call int) {
		if call == 150 { // inside the second batch of current rows
			cancel()
		}
	}
	r.j.o.Scanner = s
	first, err := r.j.Rescan(c, false)
	if err == nil {
		t.Fatal("an interrupted rescan reported success")
	}
	if first.CurrentRows != 100 || first.EventRows != 0 || first.Released != 100 {
		t.Errorf("first run %+v", first)
	}
	s.mu.Lock()
	s.hook = nil
	s.mu.Unlock()
	before := s.count()
	second := r.rescan(t, false)
	if second.CurrentRows != 150 || second.EventRows != 250 || second.Released != 400 {
		t.Errorf("second run %+v", second)
	}
	if got := s.count() - before; got != 400 {
		t.Errorf("%d values judged on resume, want 400", got)
	}
}

func TestRescanWithoutAScannerFailsAndChangesNothing(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
	r.j.o.Scanner = nil
	if _, err := r.j.Rescan(ctx, true); err == nil {
		t.Error("a rescan without a scanner succeeded")
	}
	if got := r.commentVerdict("e_child1", "k").Verdict; got != "quarantine" {
		t.Errorf("verdict %q", got)
	}
}

func TestRescanFailsOnACancelledContext(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
	r.j.o.Scanner = releasingScanner{}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.j.Rescan(c, false); err == nil {
		t.Error("a rescan with a cancelled context succeeded")
	}
	if got := r.commentVerdict("e_child1", "k").Verdict; got != "quarantine" {
		t.Errorf("verdict %q", got)
	}
}

// A database that cannot be read fails the rescan.
func TestRescanFailsWhenTheTablesAreGone(t *testing.T) {
	r := newRig(t)
	r.j.o.Scanner = releasingScanner{}
	r.exec(t, `ALTER TABLE record_current RENAME TO record_current_gone`)
	if _, err := r.j.Rescan(ctx, false); err == nil {
		t.Error("a rescan of a broken database succeeded")
	}
}

// digest hashes everything a rescan must not touch: every column of both
// tables except scan.
func digest(t *testing.T, r *rig) string {
	t.Helper()
	var a, b sql.NullString
	if err := r.j.db.QueryRow(`SELECT
		(SELECT string_agg((to_jsonb(c) - 'scan')::text, '|' ORDER BY record_type, data_subject, key) FROM record_current c),
		(SELECT string_agg((to_jsonb(e) - 'scan')::text, '|' ORDER BY seq) FROM record_event e)`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(a.String+"##"+b.String)))
}

func TestRescanChangesNothingButVerdicts(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT"}`), rec("z", "e_child1", `{"comment": "ok"}`)))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "INJECT 2"}`), rec("z", "e_child1", `{"comment": "ok"}`)))
	before, withheldBefore := digest(t, r), r.rec.withheld
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, true)
	if res.CurrentRows != 2 || res.EventRows != 3 {
		t.Errorf("%+v", res)
	}
	if after := digest(t, r); after != before {
		t.Error("a rescan changed something besides verdicts")
	}
	if r.rec.withheld != withheldBefore {
		t.Error("a rescan reported to the recorder")
	}
}

// ---- concurrency -----------------------------------------------------

// A rescan scans with no lock held: no row or advisory lock of the rescan
// exists while a value is being scanned.
func TestRescanScansWithoutHoldingLocks(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
	var probeErrs []string
	probe := func() {
		if n := advisoryLocks(t, r.j); n != 0 {
			probeErrs = append(probeErrs, fmt.Sprintf("%d advisory locks held while scanning", n))
		}
		for _, q := range []string{`SELECT 1 FROM record_current`, `SELECT 1 FROM record_event`} {
			if rowLocked(t, r, q) {
				probeErrs = append(probeErrs, "a row of "+q+" is locked while scanning")
			}
		}
	}
	probed := 0
	s := newFuncScanner(passAll)
	s.hook = func(int) { probed++; probe() }
	r.j.o.Scanner = s
	r.rescan(t, false)
	if probed != 2 {
		t.Errorf("probed %d times, want 2 (current, event)", probed)
	}
	for _, e := range probeErrs {
		t.Error(e)
	}
}

// A change that lands between the rescan's read and its write is never
// overwritten with a verdict computed from the value it replaced.
func TestRescanDoesNotWriteAStaleVerdict(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, r *rig)
		want   record.ScanEntry
	}{
		{"an Apply replaced the value", func(t *testing.T, r *rig) {
			r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Minute), true, rec("k", "e_child1", `{"comment": "INJECT replaced"}`)))
		}, record.ScanEntry{Verdict: "quarantine", ScannerVersion: "scanner-2"}},
		{"the content hash moved", func(t *testing.T, r *rig) {
			r.exec(t, `UPDATE record_current SET content_hash = decode('00', 'hex')`)
		}, record.ScanEntry{Verdict: "pass", ScannerVersion: "scanner-1"}},
		{"the event sequence moved", func(t *testing.T, r *rig) {
			r.exec(t, `UPDATE record_current SET last_event_seq = last_event_seq + 1000`)
		}, record.ScanEntry{Verdict: "pass", ScannerVersion: "scanner-1"}},
		{"the verdicts moved", func(t *testing.T, r *rig) {
			r.exec(t, `UPDATE record_current SET scan = '{"comment": {"verdict": "error", "scanner_version": "scanner-9"}}'`)
		}, record.ScanEntry{Verdict: "error", ScannerVersion: "scanner-9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "fine"}`)))
			var once firstOnly
			s := newFuncScanner(injectAware)
			s.hook = func(int) {
				if once.first() {
					tc.change(t, r)
				}
			}
			r.j.o.Scanner = s
			res := r.rescan(t, false)
			if res.CurrentRows != 0 || res.Released != 0 || res.Withheld != 0 {
				t.Errorf("a stale row was counted: %+v", res)
			}
			if got := r.commentVerdict("e_child1", "k"); got != tc.want {
				t.Errorf("verdict %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Events keep their own stale-write guard: verdicts changed under the
// rescan (by another rescan, say) are not replaced by what it computed.
func TestRescanDoesNotWriteAStaleEventVerdict(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
	var once firstOnly
	s := newFuncScanner(passAll)
	s.hook = func(call int) {
		if call == 2 && once.first() { // the first scan is the current row's, the second the event's
			r.exec(t, `UPDATE record_event SET scan = '{"comment": {"new": "error", "scanner_version": "scanner-9"}}'`)
		}
	}
	r.j.o.Scanner = s
	res := r.rescan(t, false)
	if res.EventRows != 0 || res.CurrentRows != 1 || res.Released != 1 {
		t.Errorf("%+v", res)
	}
	if got := evScan(r.events("e_child1")[0])["comment"]; got != (record.EventScan{New: "error", ScannerVersion: "scanner-9"}) {
		t.Errorf("event scan %+v", got)
	}
}

// Erasing a subject while its rows are being scanned leaves nothing behind:
// the write finds no row and resurrects none. Erase completing at all shows
// the rescan holds no lock while it scans.
func TestRescanDoesNotResurrectAnErasedSubject(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
	r.apply(snap(2, "e_child2", "c1", T0.Add(-time.Hour), true, rec("k", "e_child2", `{"comment": "INJECT"}`)))
	var once firstOnly
	s := newFuncScanner(passAll)
	s.hook = func(int) {
		if once.first() {
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := r.j.Erase(c, "a", "e_child1"); err != nil {
				t.Errorf("erase during a rescan: %v", err)
			}
		}
	}
	r.j.o.Scanner = s
	res := r.rescan(t, false)
	var n1 int
	if err := r.j.db.QueryRow(`SELECT (SELECT count(*) FROM record_current WHERE data_subject = 'e_child1')
		+ (SELECT count(*) FROM record_event WHERE data_subject = 'e_child1')`).Scan(&n1); err != nil || n1 != 0 {
		t.Errorf("%d rows of the erased subject, %v", n1, err)
	}
	// e_child2's rows are judged; e_child1's are not counted.
	if res.CurrentRows != 1 || res.EventRows != 1 || res.Released != 2 {
		t.Errorf("%+v", res)
	}
	if got := r.commentVerdict("e_child2", "k").Verdict; got != "pass" {
		t.Errorf("e_child2 %q", got)
	}
}

func TestRescanOfAnErasedSubjectHasNothingToDo(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
	if _, err := r.j.Erase(ctx, "a", "e_child1"); err != nil {
		t.Fatal(err)
	}
	r.j.o.Scanner = releasingScanner{}
	if res := r.rescan(t, true); res != (journalapi.RescanResult{}) {
		t.Errorf("%+v", res)
	}
	if r.count("record_current") != 0 || r.count("record_event") != 0 {
		t.Error("a rescan brought rows back")
	}
}

// holdRows locks the rows q selects in a transaction that stays open until
// the returned function is called (or the test ends).
func holdRows(t *testing.T, r *rig, q string) func() {
	t.Helper()
	tx, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	rows, err := tx.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	_ = rows.Close()
	return func() { _ = tx.Rollback() }
}

// rowLocked reports whether a row selected by q is locked by another
// transaction.
func rowLocked(t *testing.T, r *rig, q string) bool {
	t.Helper()
	tx, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(q + ` FOR UPDATE NOWAIT`)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		_ = rows.Close()
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "55P03" {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Rows are locked in primary-key (current) and seq (events) order, so a
// batch that is waiting for the last row already holds the earlier ones: the
// global order Erase and Apply use.
func TestRescanLocksRowsInKeyAndSeqOrder(t *testing.T) {
	for _, tc := range []struct{ name, hold, first string }{
		{"current", `SELECT 1 FROM record_current WHERE key = 'k3'`, `SELECT 1 FROM record_current WHERE key = 'k1'`},
		{"events", `SELECT 1 FROM record_event WHERE key = 'k3'`, `SELECT 1 FROM record_event WHERE key = 'k1'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true,
				rec("k1", "e_child1", `{"comment": "INJECT"}`), rec("k2", "e_child1", `{"comment": "INJECT"}`), rec("k3", "e_child1", `{"comment": "INJECT"}`)))
			r.j.o.Scanner = releasingScanner{}
			release := holdRows(t, r, tc.hold+` FOR UPDATE`)
			c := rescanCtx(t)
			done := make(chan error, 1)
			go func() { _, err := r.j.Rescan(c, false); done <- err }()
			waitFor(t, "the rescan to lock the earlier row", func() bool { return rowLocked(t, r, tc.first) })
			select {
			case err := <-done:
				t.Fatalf("the rescan did not wait for the held row: %v", err)
			default:
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The current rows are finished (committed) before the first event row is
// locked, and a transaction never holds both: with an event row held by
// someone else, the current rows are already judged and unlocked.
func TestRescanFinishesCurrentRowsBeforeLockingEvents(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k1", "e_child1", `{"comment": "INJECT"}`)))
	r.j.o.Scanner = releasingScanner{}
	release := holdRows(t, r, `SELECT 1 FROM record_event FOR UPDATE`)
	c := rescanCtx(t)
	done := make(chan error, 1)
	go func() { _, err := r.j.Rescan(c, false); done <- err }()
	waitFor(t, "the current row to be judged", func() bool {
		var v string
		return r.j.db.QueryRow(`SELECT scan->'comment'->>'verdict' FROM record_current`).Scan(&v) == nil && v == "pass"
	})
	if rowLocked(t, r, `SELECT 1 FROM record_current`) {
		t.Error("a current row is still locked while the rescan waits for an event row")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A writer that holds a row (uncommitted) when the rescan gets to it makes
// the rescan wait for the lock, and the rescan then sees the committed row,
// not the one it read: the lock is what makes compare-then-write atomic.
func TestRescanWaitsForAConcurrentWriterAndSkipsItsRow(t *testing.T) {
	for _, tc := range []struct{ name, change, read string }{
		{"current", `UPDATE record_current SET content_hash = decode('00', 'hex'), last_event_seq = last_event_seq + 1`,
			`SELECT scan->'comment'->>'scanner_version' FROM record_current`},
		{"events", `UPDATE record_event SET scan = '{"comment": {"new": "error", "scanner_version": "scanner-9"}}'`,
			`SELECT scan->'comment'->>'scanner_version' FROM record_event`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"comment": "INJECT"}`)))
			if tc.name == "events" {
				// the current row must not be the one that waits
				r.exec(t, `UPDATE record_current SET scan = '{"comment": {"verdict": "pass", "scanner_version": "scanner-2"}}'`)
			}
			r.j.o.Scanner = releasingScanner{}
			holder, err := r.j.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback() }()
			if _, err := holder.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			c := rescanCtx(t)
			type out struct {
				res journalapi.RescanResult
				err error
			}
			done := make(chan out, 1)
			go func() { res, err := r.j.Rescan(c, false); done <- out{res, err} }()
			time.Sleep(700 * time.Millisecond) // the rescan has read, scanned and is waiting for the row
			select {
			case o := <-done:
				t.Fatalf("the rescan did not wait for the held row: %+v %v", o.res, o.err)
			default:
			}
			if err := holder.Commit(); err != nil {
				t.Fatal(err)
			}
			o := <-done
			if o.err != nil {
				t.Fatal(o.err)
			}
			var sv string
			if err := r.j.db.QueryRow(tc.read).Scan(&sv); err != nil {
				t.Fatal(err)
			}
			if tc.name == "current" && (o.res.CurrentRows != 0 || sv != "scanner-1") {
				t.Errorf("the rescan overwrote a row changed under it: %+v %q", o.res, sv)
			}
			if tc.name == "events" && (o.res.EventRows != 0 || sv != "scanner-9") {
				t.Errorf("the rescan overwrote an event changed under it: %+v %q", o.res, sv)
			}
		})
	}
}

// Event fields are judged under the name they have in the active schema: a
// text field of an old event that a later version renames is text on read
// (withheld until a verdict passes it), so a rescan must judge it even when
// the event holds no verdict for it, and records the verdict under the name
// the event stores.
func TestRescanJudgesAnOldEventsFieldByItsActiveName(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "A", "comment": "x"}`)))
	r.exec(t, `UPDATE record_event SET scan = '{}'`)
	reg := journaltest.Registry(t, assignmentV2()) // comment becomes teacher_comment
	r.j.o.Registry = reg
	r.reg = reg
	if err := r.j.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	r.j.o.Scanner = releasingScanner{}
	res := r.rescan(t, false)
	if res.Released != 1 || res.EventRows != 1 || res.CurrentRows != 1 { // the event's comment; the current verdict already passed
		t.Errorf("%+v", res)
	}
	if got := evScan(r.events("e_child1")[0])["comment"]; got != (record.EventScan{New: "pass", ScannerVersion: "scanner-2"}) {
		t.Errorf("event scan %s", r.events("e_child1")[0].Scan)
	}
}
