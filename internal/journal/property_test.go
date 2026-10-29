package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// rosterOf makes a roster whose members are the given subjects, all in one
// household, plus e_parent1 as everyone's guardian.
func rosterOf(t *testing.T, subjects []string) *roster.Roster {
	t.Helper()
	people := map[string]any{"e_parent1": map[string]any{"agent": "agent-parent1", "guardian_of": subjects}}
	for _, s := range subjects {
		people[s] = map[string]any{"agent": "agent-" + s}
	}
	raw, _ := json.Marshal(map[string]any{"people": people, "household_members": append([]string{"e_parent1"}, subjects...)})
	r, err := roster.Parse(roster.FormatOpenclawHouseholdV1, raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func genSnapshots(r *rand.Rand, subject string, base time.Time, firstID, version int) []journalapi.Snapshot {
	titleField, commentField := "title", "comment"
	if version == 2 {
		titleField, commentField = "name", "teacher_comment"
	}
	var out []journalapi.Snapshot
	n := 4 + r.Intn(8)
	for i := 0; i < n; i++ {
		obs := base.Add(time.Duration(i) * time.Hour)
		if i > 0 && r.Intn(5) == 0 {
			obs = base.Add(time.Duration(r.Intn(i))*time.Hour + 30*time.Minute) // arrives late
		}
		if i > 0 && r.Intn(8) == 0 {
			obs = out[len(out)-1].ObservedAt // the same observation time as the one before: both apply
		}
		s := journalapi.Snapshot{SnapshotID: uid(firstID + i), RecordType: "assignment", SchemaVersion: version,
			Scope: course(subject, []string{"c1", "c2"}[r.Intn(2)]), Complete: r.Intn(3) != 0, ObservedAt: obs}
		if s.Complete && r.Intn(3) == 0 {
			s.CompleteRange = &journalapi.CompleteRange{Field: "due", From: "2026-09-01", To: "2026-10-15"}
		}
		for k := 0; k < 6; k++ {
			if r.Intn(3) == 0 {
				continue
			}
			f := map[string]json.RawMessage{
				titleField: record.Encode([]string{"a", "b", "c"}[r.Intn(3)]),
				"due":      record.Encode([]string{"2026-09-10", "2026-10-01", "2026-10-20"}[r.Intn(3)]),
			}
			if r.Intn(4) == 0 {
				f[commentField] = record.Encode([]string{"ok", "INJECT"}[r.Intn(2)])
			}
			if r.Intn(4) == 0 {
				f["score"] = record.Encode(float64(r.Intn(10)))
			}
			rc := journalapi.Record{Key: fmt.Sprintf("k%d", k), DataSubject: subject, Fields: f}
			if r.Intn(4) == 0 {
				rc.Audience = []string{"guardians"}
			}
			s.Records = append(s.Records, rc)
		}
		out = append(out, s)
		if r.Intn(6) == 0 {
			out = append(out, s) // delivered twice
		}
	}
	return out
}

// digest is every column of the subject's current rows and its event count.
func (r *rig) digest(subject string) string {
	r.t.Helper()
	var d string
	err := r.j.db.QueryRow(`SELECT coalesce(string_agg(t::text, '|' ORDER BY t.record_type, t.key), '') ||
		'#' || (SELECT count(*) FROM record_event WHERE data_subject = $1)
		FROM record_current t WHERE t.data_subject = $1`, subject).Scan(&d)
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

// expectPlan is an oracle for the guard, written in SQL against current
// state before the snapshot is applied: the live keys in range and how many
// of them a complete snapshot would remove.
func (r *rig) expectPlan(s journalapi.Snapshot) (live, removals int) {
	r.t.Helper()
	if !s.Complete {
		return 0, 0
	}
	keys := []string{} // never nil: a nil array is SQL NULL and would hide every removal
	for _, rc := range s.Records {
		keys = append(keys, rc.Key)
	}
	from, to := "0000-01-01", "9999-12-31"
	ranged := s.CompleteRange != nil
	if ranged {
		from, to = s.CompleteRange.From, s.CompleteRange.To
	}
	err := r.j.db.QueryRow(`SELECT count(*),
		count(*) FILTER (WHERE NOT (key = ANY($3)) AND last_confirmed_at <= $7)
		FROM record_current
		WHERE record_type = 'assignment' AND data_subject = $1 AND scope = $2 AND NOT removed
		  AND (NOT $4 OR (fields->>'due' >= $5 AND fields->>'due' <= $6))`,
		subjectOf(s), scopeOf(s), keys, ranged, from, to, s.ObservedAt).Scan(&live, &removals)
	if err != nil {
		r.t.Fatal(err)
	}
	return live, removals
}

// expectHold is the guard's rule (spec 4.5) written out here, not called:
// hold when something would be removed, more than MinKeys keys are live, and
// either all of them or more than MaxFraction of them go.
func expectHold(live, removals int) bool {
	g := diff.DefaultGuard
	return removals > 0 && live > g.MinKeys && (removals == live || float64(removals) > g.MaxFraction*float64(live))
}

func subjectOf(s journalapi.Snapshot) string {
	for _, p := range s.Scope {
		if p[0] == "data_subject" {
			return p[1]
		}
	}
	return ""
}

func scopeOf(s journalapi.Snapshot) string {
	sc, rej := record.ParseScope(s.Scope)
	if rej != nil {
		panic(rej)
	}
	return sc.Canonical()
}

func (r *rig) foldMatchesCurrent(subject string) {
	r.t.Helper()
	rows, err := r.j.db.Query(`SELECT record_type, key, kind, schema_version, scope, to_jsonb(audience), fields, changed
		FROM record_event WHERE data_subject = $1 ORDER BY seq`, subject)
	if err != nil {
		r.t.Fatal(err)
	}
	var events []diff.FoldEvent
	for rows.Next() {
		var e diff.FoldEvent
		var kind string
		var aud, fields, changed []byte
		if err := rows.Scan(&e.ID.RecordType, &e.ID.Key, &kind, &e.SchemaVersion, &e.Scope, &aud, &fields, &changed); err != nil {
			r.t.Fatal(err)
		}
		e.ID.DataSubject, e.Kind = subject, diff.Kind(kind)
		_ = json.Unmarshal(aud, &e.Audience)
		if fields != nil {
			_ = json.Unmarshal(fields, &e.Fields)
		}
		if kind == "updated" {
			var ch map[string][2]json.RawMessage
			_ = json.Unmarshal(changed, &ch)
			e.Changed = map[string]diff.Change{}
			for k, p := range ch {
				e.Changed[k] = diff.Change{Old: nilIfNull(p[0]), New: nilIfNull(p[1])}
			}
		}
		events = append(events, e)
	}
	rows.Close()
	folded := diff.Fold(events, r.reg.ForwardName)
	cur, err := r.j.db.Query(`SELECT key, scope, to_jsonb(audience), fields, removed FROM record_current
		WHERE record_type = 'assignment' AND data_subject = $1`, subject)
	if err != nil {
		r.t.Fatal(err)
	}
	defer cur.Close()
	t, _ := r.reg.Active("assignment")
	seen := 0
	for cur.Next() {
		var key, scope string
		var aud, fields []byte
		var removed bool
		if err := cur.Scan(&key, &scope, &aud, &fields, &removed); err != nil {
			r.t.Fatal(err)
		}
		seen++
		var audience []string
		_ = json.Unmarshal(aud, &audience)
		f, _ := record.Reload(t, fields)
		got := folded[diff.Identity{RecordType: "assignment", DataSubject: subject, Key: key}]
		gotFields, _ := record.Reload(t, got.Fields.JSON())
		if got.Scope != scope || got.Removed != removed || !reflect.DeepEqual(got.Audience, audience) || !bytes.Equal(gotFields.JSON(), f.JSON()) {
			r.t.Fatalf("%s/%s: fold %+v, current scope=%s removed=%v audience=%v fields=%s", subject, key, got, scope, removed, audience, f.JSON())
		}
	}
	if seen != len(folded) {
		r.t.Fatalf("%s: %d current rows, %d folded records", subject, seen, len(folded))
	}
	var orphans, stale int
	if err := r.j.db.QueryRow(`SELECT
		(SELECT count(*) FROM record_event e WHERE e.data_subject = $1 AND NOT EXISTS (SELECT 1 FROM record_current c
			WHERE c.record_type = e.record_type AND c.data_subject = e.data_subject AND c.key = e.key)),
		(SELECT count(*) FROM record_current c WHERE c.data_subject = $1 AND c.last_event_seq IS DISTINCT FROM
			(SELECT max(e.seq) FROM record_event e WHERE e.record_type = c.record_type AND e.data_subject = c.data_subject AND e.key = c.key))`,
		subject).Scan(&orphans, &stale); err != nil {
		r.t.Fatal(err)
	}
	if orphans != 0 || stale != 0 {
		r.t.Fatalf("%s: %d orphan events, %d current rows whose last_event_seq is not their newest event", subject, orphans, stale)
	}
}

// exercised counts what the generated sequences exercised, so a test can insist
// that every branch it claims to cover was reached.
type exercised map[string]int

// seqModel is a reference model of one subject's records, written from the
// rules of spec 4.4 and 4.5 and not from internal/journal/diff: snapshots
// are fed in arrival order; a snapshot is late when a snapshot of its scope
// observed later was applied; a record named by a snapshot is left alone
// when it was confirmed later (by anything, a removal included); a complete
// snapshot removes the live records of its scope (and window) that it does
// not name and that nothing confirmed later, unless the guard holds it.
type seqModel struct {
	rows       map[string]*mrow
	appliedObs map[string][]time.Time // by scope
	first      map[string]string      // first status of each snapshot id
}

type mrow struct {
	scope     string
	aud       []string
	fields    record.Fields
	removed   bool
	confirmed time.Time
}

func newSeqModel() *seqModel {
	return &seqModel{rows: map[string]*mrow{}, appliedObs: map[string][]time.Time{}, first: map[string]string{}}
}

// activate renames the stored fields forward to the registry's active version.
func (m *seqModel) activate(reg *record.Registry, from int) {
	for _, row := range m.rows {
		row.fields = reg.Forward("assignment", from, row.fields)
	}
}

// step feeds one first delivery and returns the status the journal should answer.
func (m *seqModel) step(n *record.Normalized) string {
	obs := n.ObservedAt
	for _, o := range m.appliedObs[n.ScopeKey] {
		if o.After(obs) {
			m.first[n.ID] = journalapi.StatusLate
			return journalapi.StatusLate
		}
	}
	named := map[string]bool{}
	for _, nr := range n.Records {
		named[nr.Key] = true
	}
	live, removals := 0, []string{}
	if n.Complete {
		for key, row := range m.rows {
			if row.removed || row.scope != n.ScopeKey {
				continue
			}
			if n.Range != nil {
				var due json.RawMessage = row.fields[n.Range.Field]
				if !n.Range.Contains(due) {
					continue
				}
			}
			live++
			if !named[key] && !row.confirmed.After(obs) {
				removals = append(removals, key)
			}
		}
	}
	if expectHold(live, len(removals)) {
		m.first[n.ID] = journalapi.StatusHeld
		return journalapi.StatusHeld
	}
	for _, nr := range n.Records {
		if row, ok := m.rows[nr.Key]; ok && row.confirmed.After(obs) {
			continue // stale
		}
		m.rows[nr.Key] = &mrow{scope: n.ScopeKey, aud: nr.Audience, fields: nr.Fields, confirmed: obs}
	}
	for _, key := range removals {
		m.rows[key].removed, m.rows[key].confirmed = true, obs
	}
	m.appliedObs[n.ScopeKey] = append(m.appliedObs[n.ScopeKey], obs)
	m.first[n.ID] = journalapi.StatusApplied
	return journalapi.StatusApplied
}

// matches fails when the journal's current rows differ from the model's.
func (m *seqModel) matches(r *rig, subject string, seed int64) {
	r.t.Helper()
	t, _ := r.reg.Active("assignment")
	cur, err := r.j.db.Query(`SELECT key, scope, to_jsonb(audience), fields, removed, last_confirmed_at FROM record_current
		WHERE record_type = 'assignment' AND data_subject = $1`, subject)
	if err != nil {
		r.t.Fatal(err)
	}
	defer cur.Close()
	n := 0
	for cur.Next() {
		var key, scope string
		var aud, fields []byte
		var removed bool
		var confirmed time.Time
		if err := cur.Scan(&key, &scope, &aud, &fields, &removed, &confirmed); err != nil {
			r.t.Fatal(err)
		}
		n++
		w, ok := m.rows[key]
		if !ok {
			r.t.Fatalf("seed %d: %s/%s exists and the model has no such record", seed, subject, key)
		}
		var audience []string
		_ = json.Unmarshal(aud, &audience)
		f, _ := record.Reload(t, fields)
		if scope != w.scope || removed != w.removed || !reflect.DeepEqual(audience, w.aud) || !bytes.Equal(f.JSON(), w.fields.JSON()) ||
			!confirmed.Equal(w.confirmed) {
			r.t.Fatalf("seed %d: %s/%s is scope=%s removed=%v audience=%v fields=%s confirmed=%v;\n the model says scope=%s removed=%v audience=%v fields=%s confirmed=%v",
				seed, subject, key, scope, removed, audience, f.JSON(), confirmed, w.scope, w.removed, w.aud, w.fields.JSON(), w.confirmed)
		}
	}
	if n != len(m.rows) {
		r.t.Fatalf("seed %d: %s has %d current rows, the model %d", seed, subject, n, len(m.rows))
	}
}

func runSequence(t *testing.T, r *rig, subject string, snaps []journalapi.Snapshot, seed int64, tl exercised, model *seqModel) {
	t.Helper()
	seen := map[string]bool{}
	for i, s := range snaps {
		before := r.digest(subject)
		live, removals := r.expectPlan(s)
		wantHold := !seen[s.SnapshotID] && expectHold(live, removals)
		out := r.apply(s)
		if !seen[s.SnapshotID] {
			n, rej := record.ValidateSnapshot(r.reg, s, T0.Add(time.Hour))
			if rej != nil {
				t.Fatalf("seed %d step %d: %v", seed, i, rej)
			}
			if want := model.step(n); want != out.Status {
				t.Fatalf("seed %d step %d (%s): the journal answered %s, the model %s", seed, i, s.SnapshotID, out.Status, want)
			}
		}
		if seen[s.SnapshotID] {
			tl["redelivered"]++
		} else {
			tl["first:"+out.Status]++
			if s.CompleteRange != nil {
				tl["windowed"]++
			}
			if !s.Complete {
				tl["incomplete"]++
			}
		}
		where := fmt.Sprintf("seed %d step %d (%s)", seed, i, s.SnapshotID)
		switch out.Status {
		case journalapi.StatusLate, journalapi.StatusDuplicate, journalapi.StatusHeld:
			if after := r.digest(subject); after != before {
				t.Fatalf("%s: a %s answer changed state", where, out.Status)
			}
		}
		if !seen[s.SnapshotID] && out.Status != journalapi.StatusLate && (out.Status == journalapi.StatusHeld) != wantHold {
			t.Fatalf("%s: status %s, guard oracle says hold=%v", where, out.Status, wantHold)
		}
		if !seen[s.SnapshotID] && out.Status == journalapi.StatusApplied && s.Complete {
			// An applied complete snapshot removed exactly the keys the oracle counted.
			var removed int
			_ = r.j.db.QueryRow(`SELECT count(*) FROM record_event WHERE snapshot_id = $1::uuid AND kind = 'removed'`, s.SnapshotID).Scan(&removed)
			if removed != removals {
				t.Fatalf("%s: %d removed events, the oracle counted %d", where, removed, removals)
			}
			tl["removed-events"] += removed
		}
		if !seen[s.SnapshotID] && (out.Status == journalapi.StatusHeld || out.Status == journalapi.StatusLate) {
			var n int
			_ = r.j.db.QueryRow(`SELECT count(*) FROM record_event WHERE snapshot_id = $1::uuid`, s.SnapshotID).Scan(&n)
			if n != 0 {
				t.Fatalf("%s: a %s snapshot wrote %d events", where, out.Status, n)
			}
		}
		if !s.Complete {
			var removed int
			_ = r.j.db.QueryRow(`SELECT count(*) FROM record_event WHERE snapshot_id = $1::uuid AND kind = 'removed'`, s.SnapshotID).Scan(&removed)
			if removed > 0 {
				t.Fatalf("%s: an incomplete snapshot removed %d records", where, removed)
			}
		}
		if !seen[s.SnapshotID] && out.Status == journalapi.StatusApplied {
			// Every record the snapshot names was confirmed by it, or by something later.
			for _, rc := range s.Records {
				var confirmed time.Time
				if err := r.j.db.QueryRow(`SELECT last_confirmed_at FROM record_current
					WHERE record_type = 'assignment' AND data_subject = $1 AND key = $2`, subject, rc.Key).Scan(&confirmed); err != nil {
					t.Fatalf("%s: %s: %v", where, rc.Key, err)
				}
				if confirmed.Before(s.ObservedAt) {
					t.Fatalf("%s: %s was confirmed at %v, before the snapshot that names it (%v)", where, rc.Key, confirmed, s.ObservedAt)
				}
			}
		}
		seen[s.SnapshotID] = true
	}
	model.matches(r, subject, seed)
	r.foldMatchesCurrent(subject)
	before := r.digest(subject)
	for _, s := range snaps {
		if out := r.apply(s); out.Status != journalapi.StatusDuplicate && out.Status != journalapi.StatusHeld {
			t.Fatalf("seed %d: replaying %s answered %s", seed, s.SnapshotID, out.Status)
		}
	}
	if r.digest(subject) != before {
		t.Fatalf("seed %d: replaying every snapshot changed state", seed)
	}
}

// requireTally fails the test when a branch it claims to cover never ran.
func requireTally(t *testing.T, tl exercised, keys ...string) {
	t.Helper()
	t.Logf("exercised: %v", map[string]int(tl))
	for _, k := range keys {
		if tl[k] == 0 {
			t.Errorf("the generators never produced %q", k)
		}
	}
}

func TestPropertyApplyFoldIdempotenceLateAndGuard(t *testing.T) {
	const sequences = 40
	var subjects []string
	for i := 0; i < sequences; i++ {
		subjects = append(subjects, fmt.Sprintf("e_s%02d", i))
	}
	r := newRig(t)
	r.ros.Store(rosterOf(t, subjects))
	tl := exercised{}
	for i, subject := range subjects {
		seed := int64(i)
		snaps := genSnapshots(rand.New(rand.NewSource(seed)), subject, T0.Add(-48*time.Hour), i*100+1, 1)
		runSequence(t, r, subject, snaps, seed, tl, newSeqModel())
	}
	requireTally(t, tl, "first:applied", "first:late", "first:held", "redelivered", "windowed", "incomplete", "removed-events")
}

func TestPropertyFoldAcrossASchemaActivation(t *testing.T) {
	const sequences = 15
	var subjects []string
	for i := 0; i < sequences; i++ {
		subjects = append(subjects, fmt.Sprintf("e_v%02d", i))
	}
	r := newRig(t)
	r.ros.Store(rosterOf(t, subjects))
	tl := exercised{}
	models := map[string]*seqModel{}
	for i, subject := range subjects {
		snaps := genSnapshots(rand.New(rand.NewSource(int64(1000+i))), subject, T0.Add(-96*time.Hour), i*100+1, 1)
		models[subject] = newSeqModel()
		runSequence(t, r, subject, snaps, int64(1000+i), tl, models[subject])
	}
	r.withVersion(t, assignmentV2())
	for i, subject := range subjects {
		models[subject].activate(r.reg, 1)
		rnd := rand.New(rand.NewSource(int64(2000 + i)))
		var snaps []journalapi.Snapshot
		for _, s := range genSnapshots(rnd, subject, T0.Add(-24*time.Hour), 50000+i*100+1, 2) {
			if rnd.Intn(2) == 0 { // an old connector still sends version 1
				// Copy: a duplicated delivery shares its records with the original.
				s.SchemaVersion = 1
				recs := make([]journalapi.Record, len(s.Records))
				for j, rc := range s.Records {
					f := map[string]json.RawMessage{}
					for k, v := range rc.Fields {
						switch k {
						case "name":
							k = "title"
						case "teacher_comment":
							k = "comment"
						}
						f[k] = v
					}
					rc.Fields = f
					recs[j] = rc
				}
				s.Records = recs
			}
			snaps = append(snaps, s)
		}
		runSequence(t, r, subject, snaps, int64(2000+i), tl, models[subject])
	}
	requireTally(t, tl, "first:applied", "first:held", "redelivered")
	var versions []string
	rows, _ := r.j.db.Query(`SELECT DISTINCT schema_version::text FROM record_event ORDER BY 1`)
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		versions = append(versions, v)
	}
	rows.Close()
	sort.Strings(versions)
	if strings.Join(versions, ",") != "1,2" {
		t.Errorf("events were written at versions %v; the test needs both", versions)
	}
}

// The guard's edges (spec 4.5) through Apply, with the numbers written out:
// scopes of three keys or fewer are never held; above that a complete
// snapshot is held when it removes every key or more than 30 percent of them,
// and applied when it removes exactly 30 percent or less.
func TestPropertyGuardBoundaries(t *testing.T) {
	r := newRig(t)
	r.ros.Store(rosterOf(t, []string{"e_g1"}))
	cases := []struct {
		live, removals int
		held           bool
	}{
		{1, 1, false}, {3, 3, false}, {3, 2, false}, {4, 1, false}, {4, 2, true}, {4, 4, true},
		{7, 2, false}, {7, 3, true}, {10, 3, false}, {10, 4, true}, {20, 6, false}, {20, 7, true},
		{10, 0, false},
	}
	id := 1
	for i, c := range cases {
		scope := fmt.Sprintf("g%d", i)
		var all, keep []journalapi.Record
		for k := 0; k < c.live; k++ {
			rc := rec(fmt.Sprintf("c%d-k%d", i, k), "e_g1", `{"title": "a", "due": "2026-10-01"}`)
			all = append(all, rc)
			if k >= c.removals {
				keep = append(keep, rc)
			}
		}
		if out := r.apply(snap(id, "e_g1", scope, T0.Add(-10*time.Hour), true, all...)); out.Status != journalapi.StatusApplied {
			t.Fatalf("case %d: the first snapshot answered %s", i, out.Status)
		}
		id++
		out := r.apply(snap(id, "e_g1", scope, T0.Add(-9*time.Hour), true, keep...))
		id++
		if got := out.Status == journalapi.StatusHeld; got != c.held {
			t.Errorf("%d live, %d removed: status %s, held should be %v", c.live, c.removals, out.Status, c.held)
		}
		var removed int
		_ = r.j.db.QueryRow(`SELECT count(*) FROM record_event WHERE snapshot_id = $1::uuid AND kind = 'removed'`, uid(id-1)).Scan(&removed)
		if want := c.removals; !c.held && removed != want || c.held && removed != 0 {
			t.Errorf("%d live, %d removed: %d removed events, held=%v", c.live, c.removals, removed, c.held)
		}
	}
}
