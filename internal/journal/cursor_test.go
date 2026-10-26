package journal

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// Decision 3: event sequence numbers become visible in order, so a reader
// that follows after=<last seq> while several subjects are written
// concurrently sees every event it may see, exactly once, in order.
//
// Four writers (one per subject) apply snapshots of several records each, so
// each transaction draws several sequence numbers and holds them while the
// others commit. Three readers with different visibility follow their own
// cursors in small pages the whole time; a sampler watches the table. At the
// end every reader has seen exactly the events the Go visibility rule allows
// it, each once.
func TestReaderCursorNeverSkipsAnEvent(t *testing.T) {
	r := newRig(t)
	subjects := []string{"e_child1", "e_child2", "e_parent1", "e_parent2"}
	audiences := [][]string{{"household"}, {"guardians"}, {"subject"}, {"public"}, {"siblings"}, {"guardians", "subject"}}
	const perSubject = 40
	const perSnapshot = 3
	readers := []string{"e_parent1", "e_child2", "e_guest1"}

	var wg sync.WaitGroup
	done := make(chan struct{})
	for si, subject := range subjects {
		wg.Add(1)
		go func(si int, subject string) {
			defer wg.Done()
			for i := 0; i < perSubject; i++ {
				snapRecs := make([]recSpec, perSnapshot)
				for k := range snapRecs {
					snapRecs[k] = recSpec{key: fmt.Sprintf("k%03d-%d", i, k), aud: audiences[(i+k+si)%len(audiences)]}
				}
				s := snap(si*1000+i+1, subject, "c1", T0.Add(time.Duration(i)*time.Second-time.Hour), false)
				for _, sp := range snapRecs {
					rc := rec(sp.key, subject, `{"title": "x"}`)
					rc.Audience = sp.aud
					s.Records = append(s.Records, rc)
				}
				n, rej := record.ValidateSnapshot(r.reg, s, T0)
				if rej != nil {
					t.Error(rej)
					return
				}
				if _, err := r.j.Apply(ctx, writer, n, []byte(`{}`)); err != nil {
					t.Error(err)
					return
				}
			}
		}(si, subject)
	}
	go func() { wg.Wait(); close(done) }()

	// sampler: whenever a set of seqs is visible, no lower seq may appear
	// later. Each sample is one statement, so one snapshot of the table.
	type sample struct {
		max  int64
		seqs map[int64]bool
	}
	var samples []sample
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			rows, err := r.j.db.Query(`SELECT seq FROM record_event`)
			if err != nil {
				t.Error(err)
				return
			}
			sm := sample{seqs: map[int64]bool{}}
			for rows.Next() {
				var s int64
				_ = rows.Scan(&s)
				sm.seqs[s] = true
				sm.max = max(sm.max, s)
			}
			rows.Close()
			samples = append(samples, sm)
			time.Sleep(time.Millisecond)
		}
	}()

	seen := make([][]int64, len(readers))
	var rwg sync.WaitGroup
	for ri, reader := range readers {
		rwg.Add(1)
		go func(ri int, reader string) {
			defer rwg.Done()
			var cursor int64
			follow := func() {
				for {
					evs, err := r.j.Events(ctx, EventsQuery{Reader: reader, After: cursor, Limit: 7})
					if err != nil {
						t.Error(err)
						return
					}
					for _, e := range evs {
						if e.Seq <= cursor {
							t.Errorf("%s: seq %d returned after the cursor reached %d", reader, e.Seq, cursor)
						}
						seen[ri] = append(seen[ri], e.Seq)
						cursor = e.Seq
					}
					if len(evs) < 7 {
						return
					}
				}
			}
			for running := true; running; {
				select {
				case <-done:
					running = false
				default:
				}
				follow()
			}
			follow()
		}(ri, reader)
	}
	rwg.Wait()
	<-samplerDone

	var total int
	if err := r.j.db.QueryRow(`SELECT count(*) FROM record_event`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != len(subjects)*perSubject*perSnapshot {
		t.Fatalf("%d events written, want %d", total, len(subjects)*perSubject*perSnapshot)
	}
	for ri, reader := range readers {
		_, want, _ := r.visibleByGo(reader)
		if len(want) == 0 {
			t.Fatalf("%s may see no event at all; the test would prove nothing", reader)
		}
		got := map[int64]bool{}
		for _, s := range seen[ri] {
			if got[s] {
				t.Errorf("%s: seq %d returned twice", reader, s)
			}
			got[s] = true
		}
		if !sort.SliceIsSorted(seen[ri], func(i, k int) bool { return seen[ri][i] < seen[ri][k] }) {
			t.Errorf("%s: events came out of order", reader)
		}
		for s := range want {
			if !got[s] {
				t.Errorf("%s: the cursor skipped seq %d, which it may see", reader, s)
			}
		}
		for s := range got {
			if !want[s] {
				t.Errorf("%s: saw seq %d, which it may not see", reader, s)
			}
		}
		t.Logf("%s followed %d of %d events", reader, len(got), total)
	}
	final := map[int64]bool{}
	rows, err := r.j.db.Query(`SELECT seq FROM record_event`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s int64
		_ = rows.Scan(&s)
		final[s] = true
	}
	rows.Close()
	for i, sm := range samples {
		for s := range final {
			if s < sm.max && !sm.seqs[s] {
				t.Fatalf("sample %d showed seq %d but not seq %d, which became visible later", i, sm.max, s)
			}
		}
	}
	t.Logf("%d samples of the table", len(samples))
}

type recSpec struct {
	key string
	aud []string
}
