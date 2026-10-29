package records

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Crash and restart of the emitter (spec 8, "crash mid-write, replay
// order"). See crash_test.go for the model.

// A process killed between the temp write and the rename leaves a temp
// file; a restart removes it and sends nothing for it.
func TestCrashDuringPutLeavesNoSnapshot(t *testing.T) {
	r := newEmitterRig(t)
	_ = r.emitter(t, nil, nil) // creates the spool
	s := testSnapshot(t, "e_child1", "c1", at(13, 0))
	raw, _ := json.Marshal(s)
	tmp := filepath.Join(spoolDir(r.dir), fileName(s)+".tmp-4242-1761700000000000000")
	mustWrite(t, tmp, string(raw[:len(raw)/2]))
	e := r.emitter(t, nil, nil)
	start(t, e)
	r.nextPass(t)
	if len(r.j.Answers()) != 0 || fileExists(tmp) {
		t.Fatalf("answers %d, temp file kept %v", len(r.j.Answers()), fileExists(tmp))
	}
}

// A process killed after the journal applied a snapshot but before the
// file was deleted resends it after the restart; the journal answers
// duplicate and the file is deleted. The journal applied it once. The
// crash is injected at every file operation of a drain pass.
func TestCrashAfterSendBeforeDeleteIsADuplicate(t *testing.T) {
	run := func(t *testing.T, crashAt int) (ops int) {
		clock := newClock(at(15, 0))
		j := recordstest.New(t, "tok-1")
		j.Registry = testRegistry(t)
		j.Now = clock.Now
		c, _ := NewClient(j.URL, func() (string, error) { return "tok-1", nil }, 5*time.Second, nil)
		mfs := newMemFS(clock.Now)
		sp := openMemSpool(t, mfs, clock)
		var want []string
		for i, course := range []string{"c1", "c2", "c1"} {
			s := testSnapshot(t, "e_child1", course, at(13, i))
			if err := sp.Put(s); err != nil {
				t.Fatal(err)
			}
			want = append(want, s.SnapshotID)
		}
		snd := NewSender(sp, c, nil, testSpoolConfig(), quietLog(), clock.Now)
		snd.jitter = func() float64 { return 0 }
		if crashAt == 0 {
			mfs.setFailOnce(0)
		} else {
			mfs.setCrashAt(crashAt)
		}
		snd.Drain(context.Background())
		ops = mfs.opCount()
		// The restarted process.
		sp2, err := openSpool(mfs.Restart(), "/state", testSpoolConfig(), nil, quietLog(), clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		snd2 := NewSender(sp2, c, nil, testSpoolConfig(), quietLog(), clock.Now)
		if res := snd2.Drain(context.Background()); res.Remaining != 0 {
			t.Fatalf("crash at op %d: %+v", crashAt, res)
		}
		if rej, _ := sp2.Rejected(); len(rej) != 0 {
			t.Fatalf("crash at op %d: rejected %v", crashAt, ids(rej))
		}
		got := map[string]int{}
		for _, s := range j.Received() {
			got[s.SnapshotID]++
		}
		for _, id := range want {
			if got[id] != 1 {
				t.Fatalf("crash at op %d: snapshot applied %d times", crashAt, got[id])
			}
		}
		for _, a := range j.Answers() {
			if a.Code != 200 {
				t.Fatalf("crash at op %d: answer %+v", crashAt, a)
			}
		}
		return ops
	}
	total := run(t, 0)
	if total == 0 {
		t.Fatal("a drain pass made no file operation")
	}
	for k := 1; k <= total; k++ {
		run(t, k)
	}
}

// Files written by a process that died before sending are delivered by
// the next process oldest first per scope, scopes by their oldest file.
func TestRestartReplaysInOrder(t *testing.T) {
	r := newEmitterRig(t)
	first := r.emitter(t, nil, nil)
	a1 := testSnapshot(t, "e_child1", "c1", at(13, 5))
	b1 := testSnapshot(t, "e_child1", "c2", at(13, 0))
	a2 := testSnapshot(t, "e_child1", "c1", at(13, 6))
	b2 := testSnapshot(t, "e_child1", "c2", at(13, 7))
	if err := first.Emit([]journalapi.Snapshot{a2, b2, a1, b1}); err != nil {
		t.Fatal(err)
	}
	// first dies without ever running; a new process starts.
	second := r.emitter(t, nil, nil)
	start(t, second)
	if res := r.nextPass(t); res.Sent != 4 {
		t.Fatalf("%+v", res)
	}
	want := strings.Join([]string{b1.SnapshotID, b2.SnapshotID, a1.SnapshotID, a2.SnapshotID}, ",")
	if got := strings.Join(receivedIDs(r.j), ","); got != want {
		t.Fatalf("order %s, want %s", got, want)
	}
}

// A restart forgets backoff: the first pass sends at once.
func TestRestartSendsAtOnce(t *testing.T) {
	r := newEmitterRig(t)
	first := r.emitter(t, nil, nil)
	_ = first.Emit([]journalapi.Snapshot{testSnapshot(t, "e_child1", "c1", at(13, 0))})
	r.j.SetReady(false)
	if res := first.pass(context.Background()); res.NextDue.IsZero() {
		t.Fatal("no backoff")
	}
	r.j.SetReady(true)
	second := r.emitter(t, nil, nil)
	start(t, second)
	if res := r.nextPass(t); res.Sent != 1 {
		t.Fatalf("%+v", res)
	}
}
