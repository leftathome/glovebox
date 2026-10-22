package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

var ctx = context.Background()

// T0 is the rig's start time; tests observe relative to it.
var T0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

// countingRecorder captures what the journal reports.
type countingRecorder struct {
	mu        sync.Mutex
	snapshots map[string]int
	events    map[string]int
	withheld  int
	mismatch  int
}

func (c *countingRecorder) Snapshot(_, status string) {
	c.mu.Lock()
	c.snapshots[status]++
	c.mu.Unlock()
}
func (c *countingRecorder) Event(_, kind, importance string) {
	c.mu.Lock()
	c.events[kind+"/"+importance]++
	c.mu.Unlock()
}
func (c *countingRecorder) ScanWithheld(_ string, n int) { c.mu.Lock(); c.withheld += n; c.mu.Unlock() }
func (c *countingRecorder) ApplyDuration(time.Duration)  {}
func (c *countingRecorder) VisibilityMismatch()          { c.mu.Lock(); c.mismatch++; c.mu.Unlock() }

type rig struct {
	t       *testing.T
	j       *Journal
	clock   *testClock
	scanner *journaltest.FakeScanner
	reg     *record.Registry
	ros     atomic.Pointer[roster.Roster]
	rec     *countingRecorder
}

func newRig(t *testing.T, mods ...func(*Options)) *rig {
	t.Helper()
	r := &rig{t: t, clock: &testClock{t: T0}, scanner: journaltest.NewFakeScanner("scanner-1"),
		reg: journaltest.Registry(t), rec: &countingRecorder{snapshots: map[string]int{}, events: map[string]int{}}}
	r.ros.Store(journaltest.Roster(t))
	rs, err := rules.Compile(journaltest.Rules(), r.reg)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Registry: r.reg, Rules: rs, Guard: diff.DefaultGuard, Scanner: r.scanner,
		Roster: r.ros.Load, Now: r.clock.Now, Metrics: r.rec}
	for _, m := range mods {
		m(&o)
	}
	r.reg = o.Registry
	r.j = New(journaltest.NewDB(t), o)
	if err := r.j.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.j.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	return r
}

// uid is a deterministic UUID for snapshot n.
func uid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// rec builds a record; fields is a JSON object.
func rec(key, subject, fields string) journalapi.Record {
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fields), &f); err != nil {
		panic(err)
	}
	return journalapi.Record{Key: key, DataSubject: subject, Fields: f}
}

// course is the scope of one subject's assignment list for one course.
func course(subject, c string) []journalapi.ScopePart {
	return []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"}, {"data_subject", subject}, {"course", c}}
}

func scopeKey(subject, c string) string {
	return "source=powerschool/surface=assignments/data_subject=" + subject + "/course=" + c
}

// snap is an assignment snapshot.
func snap(n int, subject, c string, observed time.Time, complete bool, recs ...journalapi.Record) journalapi.Snapshot {
	return journalapi.Snapshot{SnapshotID: uid(n), RecordType: "assignment", SchemaVersion: 1, Scope: course(subject, c),
		Complete: complete, ObservedAt: observed, Records: recs}
}

type currentView struct {
	Scope           string
	Audience        []string
	Fields          record.Fields
	Removed         bool
	FirstSeen       time.Time
	LastSeen        time.Time
	LastConfirmedAt time.Time
	ConfirmedBy     sql.NullString
	Scan            record.ScanMap
	Seq             int64
}

func (r *rig) current(typ, subject, key string) (currentView, bool) {
	r.t.Helper()
	var c currentView
	var aud, fields, scan []byte
	err := r.j.db.QueryRow(`SELECT scope, to_jsonb(audience), fields, removed, first_seen_at, last_seen_at,
		last_confirmed_at, last_confirmed_snapshot::text, scan, last_event_seq
		FROM record_current WHERE record_type = $1 AND data_subject = $2 AND key = $3`, typ, subject, key).
		Scan(&c.Scope, &aud, &fields, &c.Removed, &c.FirstSeen, &c.LastSeen, &c.LastConfirmedAt, &c.ConfirmedBy, &scan, &c.Seq)
	if err == sql.ErrNoRows {
		return c, false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	_ = json.Unmarshal(aud, &c.Audience)
	t, _ := r.reg.Active(typ)
	if c.Fields, err = record.Reload(t, fields); err != nil {
		r.t.Fatal(err)
	}
	_ = json.Unmarshal(scan, &c.Scan)
	return c, true
}

type eventView struct {
	Seq                    int64
	Type, Key, Kind        string
	Version                int
	Scope, PrevScope       string
	Audience, PrevAudience []string
	Fields, Changed        []byte
	Scan, Notify           []byte
	Observed               time.Time
	PrevObserved           sql.NullTime
	SnapshotID             sql.NullString
	Actor, Importance      string
	Rule, GroupKey         string
	Approximate            bool
}

func (r *rig) events(subject string) []eventView {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT seq, record_type, key, kind, schema_version, scope, coalesce(prev_scope, ''),
		to_jsonb(audience), to_jsonb(prev_audience), fields, changed, scan, notify, observed_at, prev_observed_at,
		snapshot_id::text, actor, importance, coalesce(rule, ''), coalesce(group_key, ''), approximate
		FROM record_event WHERE data_subject = $1 ORDER BY seq`, subject)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []eventView
	for rows.Next() {
		var e eventView
		var aud, prev []byte
		if err := rows.Scan(&e.Seq, &e.Type, &e.Key, &e.Kind, &e.Version, &e.Scope, &e.PrevScope, &aud, &prev,
			&e.Fields, &e.Changed, &e.Scan, &e.Notify, &e.Observed, &e.PrevObserved, &e.SnapshotID, &e.Actor,
			&e.Importance, &e.Rule, &e.GroupKey, &e.Approximate); err != nil {
			r.t.Fatal(err)
		}
		_ = json.Unmarshal(aud, &e.Audience)
		if prev != nil {
			_ = json.Unmarshal(prev, &e.PrevAudience)
		}
		out = append(out, e)
	}
	return out
}

func (r *rig) snapshotStatus(n int) string {
	r.t.Helper()
	var s string
	if err := r.j.db.QueryRow(`SELECT status FROM snapshot WHERE id = $1::uuid`, uid(n)).Scan(&s); err != nil {
		return "absent"
	}
	return s
}

func (r *rig) count(table string) int {
	r.t.Helper()
	var n int
	if err := r.j.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// TestRigHelpers keeps the shared helpers honest until the Apply tests of
// later tasks lean on them.
func TestRigHelpers(t *testing.T) {
	r := newRig(t)
	if uid(7) != "00000000-0000-4000-8000-000000000007" {
		t.Errorf("uid %s", uid(7))
	}
	s := snap(1, "e_child1", "math", T0, true, rec("k", "e_child1", `{"title": "W"}`))
	if s.SnapshotID != uid(1) || s.Scope[3] != (journalapi.ScopePart{"course", "math"}) || len(s.Records) != 1 {
		t.Errorf("snap %+v", s)
	}
	n, rej := record.ValidateSnapshot(r.reg, s, T0.Add(time.Hour))
	if rej != nil {
		t.Fatal(rej)
	}
	if n.ScopeKey != scopeKey("e_child1", "math") {
		t.Errorf("scope key %q vs %q", n.ScopeKey, scopeKey("e_child1", "math"))
	}
	if _, ok := r.current("assignment", "e_child1", "k"); ok {
		t.Error("a row appeared from nowhere")
	}
	if len(r.events("e_child1")) != 0 || r.snapshotStatus(1) != "absent" || r.count("record_event") != 0 {
		t.Error("an empty journal reports content")
	}
	r.clock.Set(T0.Add(time.Minute))
	if !r.j.o.Now().Equal(T0.Add(time.Minute)) {
		t.Error("the journal does not use the rig clock")
	}
	if r.ros.Load() == nil || r.j.o.Roster() == nil {
		t.Error("no roster")
	}
	r.rec.Snapshot("assignment", "applied")
	r.rec.Event("assignment", "created", "low")
	r.rec.ScanWithheld("assignment", 2)
	r.rec.VisibilityMismatch()
	r.j.o.Metrics.ApplyDuration(time.Second)
	if r.rec.snapshots["applied"] != 1 || r.rec.events["created/low"] != 1 || r.rec.withheld != 2 || r.rec.mismatch != 1 {
		t.Errorf("recorder %+v", r.rec)
	}
	if v, err := r.j.SchemaVersion(ctx); err != nil || v < 1 {
		t.Errorf("schema version %d %v", v, err)
	}
}
