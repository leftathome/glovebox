package journal

import (
	"sync"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

// Replicas starting together all call SyncTypes: the migrate lock makes one
// of them activate the renaming version and the rest find it stored.
func TestConcurrentSyncTypesActivatesOnce(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 20; i++ {
		if _, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
			first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq)
			VALUES ('assignment', 'e_child1', 'k' || $1::int::text, 's', '{subject}', '{"title": "W", "comment": "ok"}',
			now(), now(), now(), '\x00', 1)`, i); err != nil {
			t.Fatal(err)
		}
	}
	reg := journaltest.Registry(t, assignmentV2())
	const n = 6
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = New(r.j.db, Options{Registry: reg, Scanner: r.scanner}).SyncTypes(ctx)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("sync %d: %v", i, e)
		}
	}
	if got := r.count("record_type"); got != len(journaltest.Types())+1 {
		t.Errorf("record_type rows %d", got)
	}
	var renamed int
	if err := r.j.db.QueryRow(`SELECT count(*) FROM record_current WHERE fields = '{"name": "W", "teacher_comment": "ok"}'`).Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	if renamed != 20 {
		t.Errorf("%d of 20 rows carry the new names", renamed)
	}
}
