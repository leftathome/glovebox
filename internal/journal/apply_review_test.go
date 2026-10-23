package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// daySnap is a complete attendance_day snapshot over September 1 to October 15.
func daySnap(n int, observed time.Time, recs ...journalapi.Record) journalapi.Snapshot {
	return journalapi.Snapshot{SnapshotID: uid(n), RecordType: "attendance_day", SchemaVersion: 1,
		Scope:    []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "attendance_days"}, {"data_subject", "e_child1"}, {"school_year", "2026-27"}},
		Complete: true, CompleteRange: &journalapi.CompleteRange{Field: "date", From: "2026-09-01", To: "2026-10-15"},
		ObservedAt: observed, Records: recs}
}

func dayRec(date, class string) journalapi.Record {
	return rec(date, "e_child1", fmt.Sprintf(`{"date": %q, "classification": %q, "any_unexcused": true}`, date, class))
}

// normalize validates a snapshot for direct Apply calls.
func (r *rig) normalize(s journalapi.Snapshot) (*record.Normalized, []byte) {
	r.t.Helper()
	n, rej := record.ValidateSnapshot(r.reg, s, r.clock.Now().Add(time.Hour))
	if rej != nil {
		r.t.Fatalf("validate: %v", rej)
	}
	p, _ := json.Marshal(s)
	return n, p
}

// gateScanner blocks the first scan of a text containing GATE until released.
type gateScanner struct {
	TextScanner
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateScanner) Verdict(text string) string {
	if strings.Contains(text, "GATE") {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.TextScanner.Verdict(text)
}

// Scanning happens before the global events lock is taken: a slow scan for
// one subject does not hold up another subject's write (decision 3).
func TestSlowScanDoesNotHoldTheEventsLock(t *testing.T) {
	g := &gateScanner{entered: make(chan struct{}), release: make(chan struct{})}
	r := newRig(t, func(o *Options) { g.TextScanner = o.Scanner; o.Scanner = g })
	n1, p1 := r.normalize(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "t", "comment": "GATE"}`)))
	n2, p2 := r.normalize(snap(2, "e_child2", "c1", T0.Add(-time.Hour), true, rec("x", "e_child2", `{"title": "t"}`)))
	done := make(chan error, 1)
	go func() { _, err := r.j.Apply(ctx, writer, n1, p1); done <- err }()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first Apply never reached the scanner")
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := r.j.Apply(c, writer, n2, p2)
	if err != nil || out.Status != journalapi.StatusApplied {
		close(g.release)
		t.Fatalf("another subject's Apply while a scan is slow: %+v %v", out, err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := r.events("e_child1"); len(got) != 1 {
		t.Errorf("the slow write: %d events", len(got))
	}
}

// A snapshot id belongs to the writer, type and scope that first used it.
func TestSnapshotIDReuseIsRefused(t *testing.T) {
	r := newRig(t)
	first := r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, tenRecords(3, "")...))
	check := func(what string, out Outcome, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if out.Rejection == nil || out.Rejection.Status != 409 || !strings.Contains(out.Rejection.Detail, "snapshot_id reused") ||
			out.Status != "" || out.Stats != (journalapi.Stats{}) {
			t.Errorf("%s: %+v (first %+v)", what, out, first)
		}
	}
	out, err := r.applyErr(snap(1, "e_child2", "c1", T0.Add(-time.Hour), true, rec("x", "e_child2", `{"title": "x"}`)))
	check("another subject", out, err)
	out, err = r.applyErr(snap(1, "e_child1", "c2", T0.Add(-time.Hour), true, rec("x", "e_child1", `{"title": "x"}`)))
	check("another scope", out, err)
	// Same scope string, writer and id; only the record type differs.
	d := daySnap(1, T0.Add(-time.Hour))
	d.Scope, d.CompleteRange = course("e_child1", "c1"), nil
	out, err = r.applyErr(d)
	check("another type", out, err)
	n, p := r.normalize(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, tenRecords(3, "")...))
	out, err = r.j.Apply(ctx, "openclaw/glovebox-other-connector", n, p)
	check("another writer", out, err)
	if o, found, err := r.j.Lookup(ctx, uid(1), "someone-else", "assignment", scopeKey("e_child1", "c1")); err != nil || !found || o.Rejection == nil {
		t.Errorf("Lookup with another writer: %+v %v %v", o, found, err)
	}
	if r.count("snapshot") != 1 || len(r.events("e_child2")) != 0 || r.count("record_event") != 3 {
		t.Error("a refused reuse stored something")
	}
	if _, ok := r.current("assignment", "e_child2", "x"); ok {
		t.Error("a refused reuse created a record")
	}
	if len(r.rec.snapshots) != 1 || r.rec.snapshots["applied"] != 1 {
		t.Errorf("recorder %+v", r.rec.snapshots)
	}
	// The real owner still gets the stored answer.
	if again := r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, tenRecords(3, "")...)); again.Status != journalapi.StatusDuplicate || again.Stats != first.Stats {
		t.Errorf("resend after the refusals: %+v", again)
	}
}

// Two writers racing for one id on different subjects: one wins, the other
// is refused with the 409, neither fails with an error.
func TestConcurrentSnapshotIDReuseIsRefusedNotAnError(t *testing.T) {
	r := newRig(t)
	for round := 1; round <= 6; round++ {
		id := 100 + round
		var wg sync.WaitGroup
		type result struct {
			out Outcome
			err error
		}
		res := make([]result, 2)
		for i, subj := range []string{"e_child1", "e_child2"} {
			n, p := r.normalize(snap(id, subj, "c1", T0.Add(-time.Hour), true, rec("k", subj, `{"title": "x"}`)))
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i].out, res[i].err = r.j.Apply(ctx, writer, n, p)
			}()
		}
		wg.Wait()
		applied, refused := 0, 0
		for _, x := range res {
			switch {
			case x.err != nil:
				t.Fatalf("round %d: error %v", round, x.err)
			case x.out.Status == journalapi.StatusApplied:
				applied++
			case x.out.Rejection != nil && x.out.Rejection.Status == 409:
				refused++
			default:
				t.Fatalf("round %d: %+v", round, x.out)
			}
		}
		if applied != 1 || refused != 1 {
			t.Fatalf("round %d: %d applied, %d refused", round, applied, refused)
		}
	}
	if r.count("snapshot") != 6 {
		t.Errorf("snapshots stored: %d", r.count("snapshot"))
	}
}

// One snapshot sent eight times at once is applied once.
func TestConcurrentResendIsAppliedOnce(t *testing.T) {
	r := newRig(t)
	n, p := r.normalize(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, tenRecords(5, "")...))
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := r.j.Apply(ctx, writer, n, p)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				got["error"]++
			case out.Rejection != nil:
				got["rejected"]++
			default:
				got[out.Status]++
			}
		}()
	}
	wg.Wait()
	if got[journalapi.StatusApplied] != 1 || got[journalapi.StatusDuplicate] != 7 || len(got) != 2 {
		t.Errorf("outcomes %v", got)
	}
	if r.count("record_event") != 5 || r.count("snapshot") != 1 {
		t.Error("a resend wrote something")
	}
}

// An erasure refuses snapshots observed before erased_at plus the allowed
// future skew, because the writer's clock may run that far ahead.
func TestErasureSkewBoundary(t *testing.T) {
	r := newRig(t)
	erasedAt := T0.Add(-time.Hour)
	if _, err := r.j.db.Exec(`INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ('e_child1', $1, 'test', '{}')`, erasedAt); err != nil {
		t.Fatal(err)
	}
	for name, at := range map[string]time.Time{
		"at erased_at":    erasedAt,
		"inside the skew": erasedAt.Add(record.MaxFutureSkew - time.Microsecond),
	} {
		out := r.apply(snap(1, "e_child1", "c1", at, true, rec("k", "e_child1", `{"title": "x"}`)))
		if out.Rejection == nil || out.Rejection.Status != 410 {
			t.Errorf("%s: %+v", name, out)
		}
	}
	out := r.apply(snap(2, "e_child1", "c1", erasedAt.Add(record.MaxFutureSkew), true, rec("k", "e_child1", `{"title": "x"}`)))
	if out.Status != journalapi.StatusApplied {
		t.Errorf("exactly erased_at + skew: %+v", out)
	}
}

func TestAnotherSubjectsErasureDoesNotRefuse(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ('e_child2', $1, 'test', '{}')`, T0); err != nil {
		t.Fatal(err)
	}
	out := r.apply(snap(1, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "x"}`)))
	if out.Status != journalapi.StatusApplied {
		t.Errorf("%+v", out)
	}
}

// Erasure is answered before the roster check (decision 5): an erased
// subject that has left the roster gets 410, not a retryable 409.
func TestErasedSubjectOffTheRosterGets410(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ('e_stranger', $1, 'test', '{}')`, T0); err != nil {
		t.Fatal(err)
	}
	out := r.apply(snap(1, "e_stranger", "c1", T0.Add(-time.Hour), true, rec("k", "e_stranger", `{"title": "x"}`)))
	if out.Rejection == nil || out.Rejection.Status != 410 || out.Rejection.Reason != journalapi.ReasonErasedSubject {
		t.Errorf("%+v", out)
	}
}

// Only a strictly newer applied snapshot makes another late.
func TestEqualObservedAtIsNotLate(t *testing.T) {
	r := newRig(t)
	at := T0.Add(-time.Hour)
	r.apply(snap(1, "e_child1", "c1", at, true, rec("k", "e_child1", `{"title": "a"}`)))
	out := r.apply(snap(2, "e_child1", "c1", at, true, rec("k", "e_child1", `{"title": "b"}`)))
	if out.Status != journalapi.StatusApplied || out.Stats.Updated != 1 {
		t.Errorf("%+v", out)
	}
}

// The late check looks at one record type: another type's newer snapshot of
// the same scope string does not make this one late.
func TestLateCheckIsPerRecordType(t *testing.T) {
	r := newRig(t)
	same := course("e_child1", "c1")
	d := daySnap(1, T0)
	d.Scope = same
	d.CompleteRange = nil
	d.Records = []journalapi.Record{rec("d1", "e_child1", `{"date": "2026-10-14", "classification": "present"}`)}
	if out := r.apply(d); out.Status != journalapi.StatusApplied {
		t.Fatalf("%+v", out)
	}
	out := r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "a"}`)))
	if out.Status != journalapi.StatusApplied {
		t.Errorf("another type made this late: %+v", out)
	}
}

// Rules see Before=nil for restored and After=nil for removed (4.10).
func TestRulesSeeTheRightViewOfRestoredAndRemoved(t *testing.T) {
	gone := rules.Rule{Name: "gone", Importance: rules.High, GroupBy: []string{"data_subject", "field:date"},
		Match: rules.Match{RecordType: "attendance_day", Kind: []string{"removed"}}}
	r := newRig(t, func(o *Options) {
		rs, err := rules.Compile(append(journaltest.Rules(), gone), o.Registry)
		if err != nil {
			t.Fatal(err)
		}
		o.Rules = rs
	})
	t1, t2, t3 := T0.Add(-3*time.Hour), T0.Add(-2*time.Hour), T0.Add(-time.Hour)
	r.apply(daySnap(1, t1, dayRec("2026-10-14", "partial_day")))
	r.apply(daySnap(2, t2))
	r.apply(daySnap(3, t3, dayRec("2026-10-14", "partial_day")))
	ev := r.events("e_child1")
	if len(ev) != 3 || ev[0].Kind != "created" || ev[1].Kind != "removed" || ev[2].Kind != "restored" {
		t.Fatalf("events %+v", ev)
	}
	if ev[0].Rule != "partial_day_absence" || ev[0].Importance != "normal" {
		t.Errorf("created: %+v", ev[0])
	}
	// The removed record's date still groups the event: Before is set.
	if ev[1].Rule != "gone" || ev[1].Importance != "high" || ev[1].GroupKey != "data_subject=e_child1/field:date=2026-10-14" {
		t.Errorf("removed: rule %q importance %q group %q", ev[1].Rule, ev[1].Importance, ev[1].GroupKey)
	}
	// The restored record becomes partial_day again: Before is nil, so the
	// field_becomes rule matches although the tombstone held the same values.
	if ev[2].Rule != "partial_day_absence" || ev[2].Importance != "normal" {
		t.Errorf("restored: rule %q importance %q", ev[2].Rule, ev[2].Importance)
	}
}

// Changing a non-text field keeps the stored verdict of an unchanged text
// field and scans nothing.
func TestNonTextChangeKeepsTheTextVerdict(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "a", "comment": "INJECT x"}`)))
	calls := r.scanner.Calls()
	r.apply(snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "b", "comment": "INJECT x"}`)))
	c, _ := r.current("assignment", "e_child1", "k")
	if c.Scan["comment"].Verdict != "quarantine" || c.Scan["comment"].ScannerVersion != "scanner-1" {
		t.Errorf("the verdict was lost: %+v", c.Scan)
	}
	if r.scanner.Calls() != calls {
		t.Error("an unchanged text value was scanned")
	}
	var scan map[string]json.RawMessage
	_ = json.Unmarshal(r.events("e_child1")[1].Scan, &scan)
	if len(scan) != 0 {
		t.Errorf("event scan %v", scan)
	}
}

// An audience change reaches the current row, not only the event.
func TestAudienceChangeUpdatesTheCurrentRow(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, rec("k", "e_child1", `{"title": "a"}`)))
	s := snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, rec("k", "e_child1", `{"title": "a"}`))
	s.Records[0].Audience = []string{"guardians"}
	r.apply(s)
	if c, _ := r.current("assignment", "e_child1", "k"); strings.Join(c.Audience, ",") != "guardians" {
		t.Errorf("audience %v", c.Audience)
	}
}

// last_event_seq is the seq of the record's newest event; content_hash is
// the hash of its fields.
func TestCurrentRowBookkeepingMatchesEvents(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-3*time.Hour), true, tenRecords(4, "")...))
	r.apply(snap(2, "e_child1", "c1", T0.Add(-2*time.Hour), true, tenRecords(4, "x")...))
	r.apply(snap(3, "e_child1", "c1", T0.Add(-time.Hour), true, tenRecords(3, "x")...))
	var rows, bad int
	if err := r.j.db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE last_event_seq IS DISTINCT FROM
		(SELECT max(seq) FROM record_event e WHERE e.record_type = c.record_type AND e.data_subject = c.data_subject AND e.key = c.key))
		FROM record_current c`).Scan(&rows, &bad); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || bad != 0 {
		t.Errorf("%d rows, %d with a wrong last_event_seq", rows, bad)
	}
	for i := 1; i <= 4; i++ {
		key := fmt.Sprintf("c1:%d", i)
		c, _ := r.current("assignment", "e_child1", key)
		var hash []byte
		if err := r.j.db.QueryRow(`SELECT content_hash FROM record_current WHERE record_type = 'assignment' AND data_subject = 'e_child1' AND key = $1`, key).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(hash, c.Fields.Hash()) {
			t.Errorf("%s: content_hash is not the hash of the fields (created then updated)", key)
		}
	}
	// A tombstone keeps the fields it had.
	c, _ := r.current("assignment", "e_child1", "c1:4")
	if !c.Removed || string(c.Fields["title"]) != `"W4x"` {
		t.Errorf("tombstone %+v", c)
	}
}

// A refused snapshot reports no snapshot metric; the stored row has the
// journal's receive time and the snapshot's observation time.
func TestRefusalsAreNotCountedAndTimesAreStored(t *testing.T) {
	r := newRig(t)
	r.clock.Set(T0.Add(5 * time.Minute))
	if out := r.apply(snap(1, "e_stranger", "c1", T0, true, rec("k", "e_stranger", `{"title": "x"}`))); out.Rejection == nil {
		t.Fatalf("%+v", out)
	}
	if len(r.rec.snapshots) != 0 {
		t.Errorf("a refusal was counted: %v", r.rec.snapshots)
	}
	observed := T0.Add(-time.Hour)
	r.apply(snap(2, "e_child1", "c1", observed, true, rec("k", "e_child1", `{"title": "x"}`)))
	var got, rcv time.Time
	if err := r.j.db.QueryRow(`SELECT observed_at, received_at FROM snapshot WHERE id = $1::uuid`, uid(2)).Scan(&got, &rcv); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(observed) || !rcv.Equal(T0.Add(5*time.Minute)) {
		t.Errorf("observed %v received %v", got, rcv)
	}
}

// A held snapshot counts once, when it is first stored; its resends count
// as duplicates.
func TestHeldIsCountedOnceNotOnResend(t *testing.T) {
	r := newRig(t)
	r.apply(snap(1, "e_child1", "c1", T0.Add(-2*time.Hour), true, tenRecords(10, "")...))
	s := snap(2, "e_child1", "c1", T0.Add(-time.Hour), true, tenRecords(2, "")...)
	a, b := r.apply(s), r.apply(s)
	if a.Status != journalapi.StatusHeld || b.Status != journalapi.StatusHeld {
		t.Fatalf("%+v %+v", a, b)
	}
	if r.rec.snapshots["held"] != 1 || r.rec.snapshots["duplicate"] != 1 || r.rec.snapshots["applied"] != 1 {
		t.Errorf("recorder %v", r.rec.snapshots)
	}
}

// Metrics are reported after the commit: a write that fails after its
// inserts reports nothing and leaves nothing behind.
func TestFailedWriteReportsNothing(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`CREATE FUNCTION fail_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$;
		CREATE TRIGGER fail_snapshot BEFORE INSERT ON snapshot FOR EACH ROW EXECUTE FUNCTION fail_snapshot()`); err != nil {
		t.Fatal(err)
	}
	s := snap(1, "e_child1", "c1", T0.Add(-time.Hour), true,
		rec("a", "e_child1", `{"title": "a", "comment": "INJECT x"}`), rec("b", "e_child1", `{"title": "b"}`))
	if _, err := r.applyErr(s); err == nil {
		t.Fatal("the injected failure did not surface")
	}
	if len(r.rec.events) != 0 || len(r.rec.snapshots) != 0 || r.rec.withheld != 0 {
		t.Errorf("reported after a rollback: %v %v %d", r.rec.events, r.rec.snapshots, r.rec.withheld)
	}
	if r.count("record_event") != 0 || r.count("record_current") != 0 {
		t.Error("a failed write left rows")
	}
	if _, err := r.j.db.Exec(`DROP TRIGGER fail_snapshot ON snapshot`); err != nil {
		t.Fatal(err)
	}
	if out := r.apply(s); out.Status != journalapi.StatusApplied {
		t.Fatalf("%+v", out)
	}
	if r.rec.events["created/low"] != 2 || r.rec.withheld != 1 || r.rec.snapshots["applied"] != 1 {
		t.Errorf("after commit: %v %d %v", r.rec.events, r.rec.withheld, r.rec.snapshots)
	}
}
