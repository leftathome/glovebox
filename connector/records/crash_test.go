package records

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Crash tests (spec 8, "crash mid-write, replay order"). A crash is
// injected at every file operation of the step under test (memFS.crashAt:
// that operation and every later one do nothing); the test then restarts
// on what the model kept durable -- only fsynced content and fsynced
// directory entries -- and checks that the spool holds either the state
// before the step or the state after it, never a torn file, and that a
// step that returned success survived. No sleeps: every crash point is
// enumerated.

// opsOf counts the file operations step makes on a fresh spool prepared by
// setup.
func opsOf(t *testing.T, setup func(*Spool, *memFS) any, step func(*Spool, any) error) int {
	t.Helper()
	clock := newClock(at(15, 0))
	mfs := newMemFS(clock.Now)
	sp := openMemSpool(t, mfs, clock)
	arg := setup(sp, mfs)
	mfs.setFailOnce(0)
	if err := step(sp, arg); err != nil {
		t.Fatal(err)
	}
	return mfs.opCount()
}

// crashEach runs setup, crashes step at operation k for every k (and once
// past the end), restarts, and calls check with what the new process sees.
func crashEach(t *testing.T, setup func(*Spool, *memFS) any, step func(*Spool, any) error,
	check func(t *testing.T, k int, arg any, stepErr error, sp *Spool, mfs *memFS)) {
	t.Helper()
	total := opsOf(t, setup, step)
	if total == 0 {
		t.Fatal("the step makes no file operation")
	}
	for k := 1; k <= total+1; k++ {
		clock := newClock(at(15, 0))
		mfs := newMemFS(clock.Now)
		sp := openMemSpool(t, mfs, clock)
		arg := setup(sp, mfs)
		mfs.setCrashAt(k)
		err := step(sp, arg)
		after := mfs.Restart()
		sp2, oerr := openSpool(after, "/state", testSpoolConfig(), nil, quietLog(), clock.Now)
		if oerr != nil {
			t.Fatalf("crash at op %d: restart failed: %v", k, oerr)
		}
		check(t, k, arg, err, sp2, after)
		if t.Failed() {
			t.Logf("crash at op %d of %d", k, total)
			return
		}
	}
}

func ids(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func noTempFiles(t *testing.T, k int, mfs *memFS) {
	t.Helper()
	for _, dir := range []string{"/state/spool", "/state/spool/rejected"} {
		for _, n := range mfs.names(dir) {
			if strings.Contains(n, tmpMarker) {
				t.Errorf("crash at op %d: temp file %s survives recovery", k, n)
			}
		}
	}
}

func TestCrashDuringPutLeavesOldOrNew(t *testing.T) {
	type pair struct{ a, b journalapi.Snapshot }
	setup := func(sp *Spool, _ *memFS) any {
		p := pair{testSnapshot(t, "e_child1", "c1", at(13, 0)), testSnapshot(t, "e_child1", "c1", at(14, 0))}
		if err := sp.Put(p.a); err != nil {
			t.Fatal(err)
		}
		return p
	}
	step := func(sp *Spool, arg any) error { return sp.Put(arg.(pair).b) }
	crashEach(t, setup, step, func(t *testing.T, k int, arg any, stepErr error, sp *Spool, mfs *memFS) {
		p := arg.(pair)
		pending, _ := sp.Pending()
		got := strings.Join(ids(pending), ",")
		old, new := p.a.SnapshotID, p.a.SnapshotID+","+p.b.SnapshotID
		if got != old && got != new {
			t.Errorf("crash at op %d: pending %s, want the old or the new state", k, got)
		}
		if stepErr == nil && got != new {
			t.Errorf("crash at op %d: Put returned nil but the snapshot did not survive", k)
		}
		if rej, _ := sp.Rejected(); len(rej) != 0 {
			t.Errorf("crash at op %d: a torn file was quarantined: %v", k, rej)
		}
		if got == new {
			want, _ := json.Marshal(p.b)
			body, _ := sp.Body(pending[1])
			if !bytes.Equal(body, want) {
				t.Errorf("crash at op %d: the new file is torn", k)
			}
		}
		noTempFiles(t, k, mfs)
	})
}

func TestCrashDuringRejectNeverLosesTheFile(t *testing.T) {
	setup := func(sp *Spool, _ *memFS) any {
		if err := sp.Put(testSnapshot(t, "e_child1", "c1", at(13, 0))); err != nil {
			t.Fatal(err)
		}
		es, _ := sp.Pending()
		return es[0]
	}
	step := func(sp *Spool, arg any) error { return sp.Reject(arg.(Entry), "schema") }
	crashEach(t, setup, step, func(t *testing.T, k int, arg any, stepErr error, sp *Spool, mfs *memFS) {
		e := arg.(Entry)
		pending, _ := sp.Pending()
		rej, _ := sp.Rejected()
		inP, inR := len(pending) == 1 && pending[0].ID == e.ID, len(rej) == 1 && rej[0].ID == e.ID && !rej[0].Unreadable
		if inP == inR || len(pending)+len(rej) != 1 {
			t.Errorf("crash at op %d: pending %v rejected %v: want the file in exactly one", k, ids(pending), ids(rej))
		}
		if stepErr == nil && !inR {
			t.Errorf("crash at op %d: Reject returned nil but the file is not in rejected/", k)
		}
	})
}

func TestCrashDuringDeleteIsOldOrNew(t *testing.T) {
	setup := func(sp *Spool, _ *memFS) any {
		_ = sp.Put(testSnapshot(t, "e_child1", "c1", at(13, 0)))
		_ = sp.Put(testSnapshot(t, "e_child2", "c1", at(13, 0)))
		es, _ := sp.Pending()
		return es
	}
	step := func(sp *Spool, arg any) error { return sp.Delete(arg.([]Entry)[0]) }
	crashEach(t, setup, step, func(t *testing.T, k int, arg any, stepErr error, sp *Spool, mfs *memFS) {
		es := arg.([]Entry)
		pending, _ := sp.Pending()
		got := strings.Join(ids(pending), ",")
		old, new := es[0].ID+","+es[1].ID, es[1].ID
		if got != old && got != new {
			t.Errorf("crash at op %d: pending %s", k, got)
		}
		if stepErr == nil && got != new {
			t.Errorf("crash at op %d: Delete returned nil but the file came back", k)
		}
	})
}

// Recovery itself can crash (while quarantining a file): the next start
// finishes the job and the file is in rejected/ exactly once.
func TestCrashDuringRecoveryQuarantine(t *testing.T) {
	clock := newClock(at(15, 0))
	base := newMemFS(clock.Now)
	openMemSpool(t, base, clock)
	garbage := "20261016T131000.000Z-0b9f6c8e-6a64-4c49-9f55-6f4cf1b3b0a1.json"
	base.put(filepath.Join("/state/spool", garbage), []byte("{torn"))
	// Opening makes about ten operations here; enumerate past them.
	for k := 1; k <= 16; k++ {
		mfs := base.Restart()
		mfs.setCrashAt(k)
		_, _ = openSpool(mfs, "/state", testSpoolConfig(), nil, quietLog(), clock.Now)
		after := mfs.Restart()
		sp, err := openSpool(after, "/state", testSpoolConfig(), nil, quietLog(), clock.Now)
		if err != nil {
			t.Fatalf("crash at op %d: %v", k, err)
		}
		pending, _ := sp.Pending()
		rej, _ := sp.Rejected()
		if len(pending) != 0 || len(rej) != 1 || rej[0].Name != garbage || !rej[0].Unreadable {
			t.Fatalf("crash at op %d: pending %v rejected %+v", k, pending, rej)
		}
	}
}
