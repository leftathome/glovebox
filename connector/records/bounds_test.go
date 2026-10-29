package records

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// boundsSpool opens a spool on a real directory with clock and cfg.
func boundsSpool(t *testing.T, dir string, clock *fakeClock, cfg SpoolConfig) (*Spool, *testMetrics) {
	t.Helper()
	m := newTestMetrics(t)
	sp, err := OpenSpool(dir, cfg, m.Metrics, quietLog(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return sp, m
}

func put(t *testing.T, sp *Spool, s journalapi.Snapshot) Entry {
	t.Helper()
	if err := sp.Put(s); err != nil {
		t.Fatal(err)
	}
	es, _ := sp.Pending()
	for _, e := range es {
		if e.ID == s.SnapshotID {
			return e
		}
	}
	t.Fatalf("snapshot %s not pending", s.SnapshotID)
	return Entry{}
}

func TestMaxAgeDropsEveryOldFileNewestIncluded(t *testing.T) {
	clock := newClock(at(13, 0))
	sp, m := boundsSpool(t, t.TempDir(), clock, testSpoolConfig())
	put(t, sp, testSnapshot(t, "e_child1", "c1", at(13, 0)))
	clock.Advance(7 * 24 * time.Hour)
	if err := sp.Enforce(); err != nil {
		t.Fatal(err)
	}
	if es, _ := sp.Pending(); len(es) != 1 {
		t.Fatal("a file exactly max_age old was dropped")
	}
	clock.Advance(time.Millisecond)
	if err := sp.Enforce(); err != nil {
		t.Fatal(err)
	}
	if es, _ := sp.Pending(); len(es) != 0 {
		t.Fatalf("pending %v", es)
	}
	if m.counter(t, metricDropped, "reason", DropMaxAge) != 1 {
		t.Fatal("not counted")
	}
}

func TestMaxAgeCountsUnknownSubjectSeparately(t *testing.T) {
	clock := newClock(at(13, 0))
	sp, m := boundsSpool(t, t.TempDir(), clock, testSpoolConfig())
	e := put(t, sp, testSnapshot(t, "e_child2", "c1", at(13, 0)))
	put(t, sp, testSnapshot(t, "e_child1", "c1", at(13, 0)))
	sp.MarkUnknown(e.ScopeKey, true)
	clock.Advance(8 * 24 * time.Hour)
	_ = sp.Enforce()
	if m.counter(t, metricDropped, "reason", DropUnknownSubjectExpired) != 1 || m.counter(t, metricDropped, "reason", DropMaxAge) != 1 {
		t.Fatal("labels wrong")
	}
	if len(sp.unknown) != 0 {
		t.Fatal("the mark of an empty scope is kept")
	}
	// MarkUnknown(false) clears.
	sp.MarkUnknown("k", true)
	sp.MarkUnknown("k", false)
	if sp.unknown["k"] {
		t.Fatal("not cleared")
	}
}

func TestMaxAgeAppliesToRejected(t *testing.T) {
	clock := newClock(at(13, 0))
	dir := t.TempDir()
	sp, m := boundsSpool(t, dir, clock, testSpoolConfig())
	e := put(t, sp, testSnapshot(t, "e_child1", "c1", at(13, 0)))
	_ = sp.Reject(e, "schema")
	clock.Advance(8 * 24 * time.Hour)
	_ = sp.Enforce()
	if n := listNames(t, rejectedDir(dir)); len(n) != 0 {
		t.Fatalf("rejected %v", n)
	}
	if m.counter(t, metricDropped, "reason", DropMaxAge) != 1 {
		t.Fatal("not counted")
	}
}

func TestMaxAgeAppliesToUnreadableRejectedFiles(t *testing.T) {
	clock := newClock(at(13, 0))
	dir := t.TempDir()
	sp, _ := boundsSpool(t, dir, clock, testSpoolConfig())
	stray := filepath.Join(rejectedDir(dir), "notes.txt")
	mustWrite(t, stray, "x")
	old := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(stray, old, old)
	clock.t = time.Now()
	_ = sp.Enforce()
	if fileExists(stray) {
		t.Fatal("an old unreadable file is kept forever")
	}
	fresh := filepath.Join(rejectedDir(dir), "fresh.txt")
	mustWrite(t, fresh, "x")
	_ = sp.Enforce()
	if !fileExists(fresh) {
		t.Fatal("a fresh unreadable file was dropped")
	}
}

func TestMaxBytesDropsRejectedFirstThenIntermediates(t *testing.T) {
	clock := newClock(at(15, 0))
	dir := t.TempDir()
	cfg := testSpoolConfig()
	cfg.MaxBytes = 1 << 30
	sp, m := boundsSpool(t, dir, clock, cfg)
	r1 := put(t, sp, testSnapshot(t, "e_child1", "c9", at(12, 0)))
	_ = sp.Reject(r1, "schema")
	a1 := put(t, sp, testSnapshot(t, "e_child1", "cA", at(13, 0)))
	a2 := put(t, sp, testSnapshot(t, "e_child1", "cA", at(13, 1)))
	a3 := put(t, sp, testSnapshot(t, "e_child1", "cA", at(13, 2)))
	b1 := put(t, sp, testSnapshot(t, "e_child2", "cB", at(12, 30)))
	// Room for exactly three files: r1 goes, then a1 (oldest intermediate);
	// a2 stays because the limit is then met; a3 and b1 are newest.
	sp.cfg.MaxBytes = a2.Size + a3.Size + b1.Size
	if err := sp.Enforce(); err != nil {
		t.Fatal(err)
	}
	es, _ := sp.Pending()
	if got := strings.Join(ids(es), ","); got != strings.Join([]string{b1.ID, a2.ID, a3.ID}, ",") {
		t.Fatalf("pending %v (a1 %s)", got, a1.ID)
	}
	if rej, _ := sp.Rejected(); len(rej) != 0 {
		t.Fatal("rejected file kept over an intermediate")
	}
	if m.counter(t, metricDropped, "reason", DropMaxBytes) != 2 {
		t.Fatal("not counted")
	}
}

func TestMaxBytesNeverDropsTheNewestOfAScope(t *testing.T) {
	clock := newClock(at(15, 0))
	lb, log := newLogBuffer()
	m := newTestMetrics(t)
	cfg := testSpoolConfig()
	sp, err := OpenSpool(t.TempDir(), cfg, m.Metrics, log, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	put(t, sp, testSnapshot(t, "e_child1", "cA", at(13, 0)))
	a2 := put(t, sp, testSnapshot(t, "e_child1", "cA", at(13, 1)))
	b1 := put(t, sp, testSnapshot(t, "e_child2", "cB", at(13, 0)))
	sp.cfg.MaxBytes = 10
	_ = sp.Enforce()
	_ = sp.Enforce()
	es, _ := sp.Pending()
	if len(es) != 2 || es[0].ID != b1.ID || es[1].ID != a2.ID {
		t.Fatalf("pending %v", ids(es))
	}
	if n := strings.Count(lb.String(), "over max_bytes"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	sp.cfg.MaxBytes = 1 << 30
	_ = sp.Enforce()
	sp.cfg.MaxBytes = 10
	_ = sp.Enforce()
	if n := strings.Count(lb.String(), "over max_bytes"); n != 2 {
		t.Fatalf("not logged again after recovering: %d", n)
	}
}

func TestPutEnforcesBounds(t *testing.T) {
	clock := newClock(at(15, 0))
	sp, _ := boundsSpool(t, t.TempDir(), clock, testSpoolConfig())
	sp.cfg.MaxBytes = 10
	put(t, sp, testSnapshot(t, "e_child1", "cA", at(13, 0)))
	_ = sp.Put(testSnapshot(t, "e_child1", "cA", at(13, 1)))
	if es, _ := sp.Pending(); len(es) != 1 {
		t.Fatalf("Put did not enforce: %v", ids(es))
	}
}

func TestPurgeSubjectRemovesPendingAndRejected(t *testing.T) {
	clock := newClock(at(15, 0))
	dir := t.TempDir()
	sp, m := boundsSpool(t, dir, clock, testSpoolConfig())
	r := put(t, sp, testSnapshot(t, "e_child1", "c1", at(12, 0)))
	_ = sp.Reject(r, "schema")
	put(t, sp, testSnapshot(t, "e_child1", "c1", at(13, 0)))
	put(t, sp, testSnapshot(t, "e_child1", "c2", at(13, 0)))
	keep := put(t, sp, testSnapshot(t, "e_child2", "c1", at(13, 0)))
	mustWrite(t, filepath.Join(rejectedDir(dir), "notes.txt"), "x")
	n, err := sp.PurgeSubject("e_child1")
	if err != nil || n != 3 {
		t.Fatalf("n %d err %v", n, err)
	}
	es, _ := sp.Pending()
	if len(es) != 1 || es[0].ID != keep.ID {
		t.Fatalf("pending %v", ids(es))
	}
	if names := listNames(t, rejectedDir(dir)); len(names) != 1 || names[0] != "notes.txt" {
		t.Fatalf("rejected %v", names)
	}
	if m.counter(t, metricDropped, "reason", DropErasedSubject) != 3 {
		t.Fatal("not counted")
	}
	// Later snapshots about the subject are not written.
	if err := sp.Put(testSnapshot(t, "e_child1", "c1", at(14, 0))); err != nil {
		t.Fatal(err)
	}
	if es, _ := sp.Pending(); len(es) != 1 {
		t.Fatal("a snapshot about an erased subject was spooled")
	}
	if m.counter(t, metricDropped, "reason", DropErasedSubject) != 4 {
		t.Fatal("not counted")
	}
	// An empty subject purges nothing.
	if n, _ := sp.PurgeSubject(""); n != 0 {
		t.Fatal("purged files without a subject")
	}
}
