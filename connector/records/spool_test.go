package records

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func TestPutWritesOneFileNamedByTimeAndID(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	snap := testSnapshot(t, "e_child1", "c1", at(13, 0))
	if err := sp.Put(snap); err != nil {
		t.Fatal(err)
	}
	names := listNames(t, spoolDir(dir))
	want := "20261016T130000.000Z-" + snap.SnapshotID + ".json"
	if len(names) != 1 || names[0] != want {
		t.Fatalf("files %v, want [%s]", names, want)
	}
	got := readSnapshot(t, filepath.Join(spoolDir(dir), want))
	if !reflect.DeepEqual(got, snap) {
		t.Fatalf("round trip changed the snapshot:\n%+v\n%+v", got, snap)
	}
	info, err := os.Stat(filepath.Join(spoolDir(dir), want))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v err %v", info.Mode(), err)
	}
	dinfo, err := os.Stat(spoolDir(dir))
	if err != nil || dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("spool dir mode %v err %v", dinfo.Mode(), err)
	}
}

func TestOpenSpoolNeedsAnAbsoluteStateDir(t *testing.T) {
	for _, d := range []string{"", "relative/state"} {
		if _, err := OpenSpool(d, testSpoolConfig(), nil, quietLog(), nil); err == nil {
			t.Errorf("state dir %q accepted", d)
		}
	}
}

func TestPutRefusesWhatCannotBeSpooled(t *testing.T) {
	sp := openTestSpool(t, t.TempDir())
	m := newTestMetrics(t)
	sp.m = m.Metrics
	bad := []journalapi.Snapshot{
		func() journalapi.Snapshot {
			s := testSnapshot(t, "e_child1", "c1", at(13, 0))
			s.SnapshotID = "x"
			return s
		}(),
		func() journalapi.Snapshot { s := testSnapshot(t, "e_child1", "c1", at(13, 0)); s.Scope = nil; return s }(),
		func() journalapi.Snapshot {
			s := testSnapshot(t, "e_child1", "c1", at(13, 0))
			s.ObservedAt = s.ObservedAt.AddDate(-3000, 0, 0)
			return s
		}(),
	}
	for i, s := range bad {
		if err := sp.Put(s); err == nil {
			t.Errorf("%d: accepted", i)
		}
	}
	if es, _ := sp.Pending(); len(es) != 0 {
		t.Fatalf("pending %v", es)
	}
	if m.counter(t, metricWriteErrors) != 3 {
		t.Fatal("write errors not counted")
	}
}

func TestPendingIsOldestFirstWithScopeAndSubject(t *testing.T) {
	sp := openTestSpool(t, t.TempDir())
	b := testSnapshot(t, "e_child1", "c1", at(14, 0))
	a := testSnapshot(t, "e_child1", "c2", at(13, 0))
	c := testSnapshot(t, "e_child2", "c1", at(13, 0))
	for _, s := range []journalapi.Snapshot{b, a, c} {
		if err := sp.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	es, err := sp.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("pending %v", es)
	}
	// Ties on observed_at sort by name (the id), so the order is total.
	if es[0].ObservedAt.After(es[1].ObservedAt) || es[0].Name > es[1].Name || es[2].ID != b.SnapshotID {
		t.Fatalf("order %v", es)
	}
	if es[2].DataSubject != "e_child1" || es[2].ScopeKey == "" || es[2].RecordType != "t1" ||
		es[2].ScopePrefix != "source=src/surface=s/" || es[2].Rejected {
		t.Fatalf("entry fields %+v", es[2])
	}
	if es[0].ScopeKey == es[1].ScopeKey {
		t.Fatal("different scopes share a key")
	}
}

func TestOpenRemovesTempFilesAndQuarantinesUnreadable(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	good := testSnapshot(t, "e_child1", "c1", at(13, 0))
	if err := sp.Put(good); err != nil {
		t.Fatal(err)
	}
	spool := spoolDir(dir)
	// What a crash mid-write leaves behind, and kinds of garbage.
	tmp := filepath.Join(spool, "20261016T130500.000Z-x.json.tmp-1-2")
	mustWrite(t, tmp, `{"snapshot_id":`)
	rtmp := filepath.Join(rejectedDir(dir), "y.json.tmp-1-2")
	mustWrite(t, rtmp, `{`)
	mustWrite(t, filepath.Join(spool, "20261016T131000.000Z-"+uuid.NewString()+".json"), `{not json`)
	mustWrite(t, filepath.Join(spool, "notes.txt"), "x")
	other := testSnapshot(t, "e_child1", "c1", at(13, 0))
	mustWrite(t, filepath.Join(spool, "20261016T131500.000Z-"+other.SnapshotID+".json"), `{"snapshot_id":"`+other.SnapshotID+`"}`)

	m := newTestMetrics(t)
	sp2, err := OpenSpool(dir, testSpoolConfig(), m.Metrics, quietLog(), fixedNow(at(15, 0)))
	if err != nil {
		t.Fatal(err)
	}
	es, _ := sp2.Pending()
	if len(es) != 1 || es[0].ID != good.SnapshotID {
		t.Fatalf("pending after recovery: %v", es)
	}
	if n := len(listNames(t, rejectedDir(dir))); n != 3 {
		t.Fatalf("rejected/ has %d files, want 3 (unparseable json, stray file, content not matching its name)", n)
	}
	if m.counter(t, metricRejected, "reason", RejectUnreadable) != 3 {
		t.Fatal("unreadable files not counted")
	}
	if fileExists(tmp) || fileExists(rtmp) {
		t.Fatal("temp file left behind")
	}
	rej, _ := sp2.Rejected()
	for _, e := range rej {
		if !e.Unreadable || !e.Rejected {
			t.Fatalf("rejected entry %+v", e)
		}
	}
}

func TestOpenKeepsTheRejectedCopyOfAFileInBoth(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	s := testSnapshot(t, "e_child1", "c1", at(13, 0))
	if err := sp.Put(s); err != nil {
		t.Fatal(err)
	}
	name := fileName(s)
	raw, _ := os.ReadFile(filepath.Join(spoolDir(dir), name))
	mustWrite(t, filepath.Join(rejectedDir(dir), name), string(raw))
	sp2 := openTestSpool(t, dir)
	if es, _ := sp2.Pending(); len(es) != 0 {
		t.Fatalf("pending %v", es)
	}
	if rej, _ := sp2.Rejected(); len(rej) != 1 || rej[0].ID != s.SnapshotID {
		t.Fatalf("rejected %v", rej)
	}
}

func TestRejectAndDelete(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	m := newTestMetrics(t)
	sp.m = m.Metrics
	a := testSnapshot(t, "e_child1", "c1", at(13, 0))
	b := testSnapshot(t, "e_child1", "c2", at(13, 1))
	_ = sp.Put(a)
	_ = sp.Put(b)
	es, _ := sp.Pending()
	if err := sp.Reject(es[0], "schema"); err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(es[1]); err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(es[1]); err != nil {
		t.Fatalf("deleting a file twice: %v", err)
	}
	if n := listNames(t, spoolDir(dir)); len(n) != 0 {
		t.Fatalf("spool %v", n)
	}
	if n := listNames(t, rejectedDir(dir)); len(n) != 1 || !strings.Contains(n[0], a.SnapshotID) {
		t.Fatalf("rejected %v", n)
	}
	if m.counter(t, metricRejected, "reason", "schema") != 1 {
		t.Fatal("reject not counted")
	}
	body, err := sp.Body(es[0])
	if err == nil || body != nil {
		t.Fatal("body of a moved file read from its old path")
	}
}

// A write that fails at any step leaves no file behind (and a temp file
// never), reports the error and counts it. Only a failure of the final
// directory fsync leaves the complete file: it may still be sent.
func TestPutFailureLeavesNothing(t *testing.T) {
	clock := newClock(at(15, 0))
	probe := newMemFS(clock.Now)
	sp := openMemSpool(t, probe, clock)
	probe.setFailOnce(0)
	if err := sp.Put(testSnapshot(t, "e_child1", "c1", at(13, 0))); err != nil {
		t.Fatal(err)
	}
	total := probe.opCount()
	for k := 1; k <= total; k++ {
		mfs := newMemFS(clock.Now)
		sp := openMemSpool(t, mfs, clock)
		m := newTestMetrics(t)
		sp.m = m.Metrics
		mfs.setFailOnce(k)
		s := testSnapshot(t, "e_child1", "c1", at(13, 0))
		err := sp.Put(s)
		names := mfs.names("/state/spool")
		if err == nil {
			// The failing operation was not on the write path (a gauge
			// refresh reads only).
			t.Fatalf("op %d (%s): Put did not report the failure", k, probe.trace[k-1])
		}
		if m.counter(t, metricWriteErrors) != 1 {
			t.Errorf("op %d: write error not counted", k)
		}
		for _, n := range names {
			if strings.Contains(n, tmpMarker) {
				t.Errorf("op %d (%s): temp file left: %v", k, probe.trace[k-1], names)
			}
		}
		lastStep := probe.trace[k-1] == "syncdir"
		if !lastStep && len(names) != 0 {
			t.Errorf("op %d (%s): files after a failed put: %v", k, probe.trace[k-1], names)
		}
	}
}

// openMemSpool opens a spool on a memFS under /state.
func openMemSpool(t testing.TB, mfs *memFS, clock *fakeClock) *Spool {
	t.Helper()
	sp, err := openSpool(mfs, "/state", testSpoolConfig(), nil, quietLog(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// A corrupt file that turns up in spool/ while the spool runs (by hand, or
// disk corruption) is quarantined in rejected/ and counted, never deleted.
func TestPendingQuarantinesACorruptFileFoundAfterOpen(t *testing.T) {
	dir := t.TempDir()
	m := newTestMetrics(t)
	sp, err := OpenSpool(dir, testSpoolConfig(), m.Metrics, quietLog(), fixedNow(at(15, 0)))
	if err != nil {
		t.Fatal(err)
	}
	other := testSnapshot(t, "e_child1", "c1", at(13, 0))
	bad := filepath.Join(spoolDir(dir), fileName(other))
	mustWrite(t, bad, `{"snapshot_id":"`+other.SnapshotID+`"`)
	if es, _ := sp.Pending(); len(es) != 0 {
		t.Fatalf("pending %v", ids(es))
	}
	if fileExists(bad) || !fileExists(filepath.Join(rejectedDir(dir), fileName(other))) {
		t.Fatal("the corrupt file was not moved to rejected/")
	}
	if m.counter(t, metricRejected, "reason", RejectUnreadable) != 1 {
		t.Fatal("not counted")
	}
}

// A read that fails (EMFILE, EIO) says nothing about the file: at
// recovery and in a later listing it is kept in spool/, not quarantined,
// logged once by kind, and sent once it can be read.
func TestReadErrorKeepsTheFileForALaterPass(t *testing.T) {
	clock := newClock(at(15, 0))
	mfs := newMemFS(clock.Now)
	first := openMemSpool(t, mfs, clock)
	atOpen := testSnapshot(t, "e_child1", "c1", at(13, 0))
	if err := first.Put(atOpen); err != nil {
		t.Fatal(err)
	}
	pathOpen := filepath.Join("/state", spoolDirName, fileName(atOpen))
	mfs.failReads(pathOpen, errInjected)

	lb, log := newLogBuffer()
	sp, err := openSpool(mfs, "/state", testSpoolConfig(), nil, log, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	later := testSnapshot(t, "e_child2", "c1", at(13, 1))
	raw, _ := json.Marshal(later)
	pathLater := filepath.Join("/state", spoolDirName, fileName(later))
	mfs.put(pathLater, raw)
	mfs.failReads(pathLater, errInjected)
	for i := 0; i < 3; i++ {
		if es, err := sp.Pending(); err != nil || len(es) != 0 {
			t.Fatalf("pending %v, %v", ids(es), err)
		}
	}
	if n := mfs.names(filepath.Join("/state", spoolDirName, rejectedDirName)); len(n) != 0 {
		t.Fatalf("quarantined after a read error: %v", n)
	}
	if n := mfs.names(filepath.Join("/state", spoolDirName)); len(n) != 2 {
		t.Fatalf("spool/ %v", n)
	}
	if got := strings.Count(lb.String(), "could not be read; keeping it"); got != 2 {
		t.Fatalf("logged %d times, want once per file:\n%s", got, lb.String())
	}
	if strings.Contains(lb.String(), ".json") || !strings.Contains(lb.String(), `"error":"i/o error"`) {
		t.Fatalf("log line:\n%s", lb.String())
	}

	mfs.failReads(pathOpen, nil)
	mfs.failReads(pathLater, nil)
	j := recordstest.New(t, "tok-1")
	j.Now = clock.Now
	c, err := NewClient(j.URL, TokenFile(newTokenFile(t, "tok-1")), 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := NewSender(sp, c, nil, testSpoolConfig(), quietLog(), clock.Now).Drain(context.Background())
	if res.Sent != 2 || res.Remaining != 0 {
		t.Fatalf("%+v", res)
	}
}

// A file that stays unreadable is still bound by max_age, by its name.
func TestMaxAgeDropsAFileThatCannotBeRead(t *testing.T) {
	clock := newClock(at(15, 0))
	mfs := newMemFS(clock.Now)
	first := openMemSpool(t, mfs, clock)
	s := testSnapshot(t, "e_child1", "c1", at(13, 0))
	if err := first.Put(s); err != nil {
		t.Fatal(err)
	}
	mfs.failReads(filepath.Join("/state", spoolDirName, fileName(s)), errInjected)
	sp := openMemSpool(t, mfs, clock)
	m := newTestMetrics(t)
	sp.m = m.Metrics
	clock.Advance(8 * 24 * time.Hour)
	if err := sp.Enforce(); err != nil {
		t.Fatal(err)
	}
	if n := mfs.names(filepath.Join("/state", spoolDirName)); len(n) != 0 {
		t.Fatalf("spool/ %v", n)
	}
	if m.counter(t, metricDropped, "reason", DropMaxAge) != 1 {
		t.Fatal("not counted")
	}
}
