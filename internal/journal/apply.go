package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// Outcome is what a write did: a status with stats, or a refusal.
type Outcome struct {
	Status    string
	Stats     journalapi.Stats
	Rejection *record.Rejection
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// errSnapshotIDReused is the unique violation on snapshot_pkey: the id
// belongs to a snapshot another writer, type or scope stored first.
var errSnapshotIDReused = errors.New("journal: snapshot_id reused")

// reusedRejection answers a snapshot id that is already taken by a snapshot
// of another writer, record type or scope. It is a 422 (a permanent sender
// bug: retrying cannot help, the sender must use a new id), not the
// retryable 409 of an unknown subject. It reveals nothing about the stored
// snapshot (no stats, no status).
func reusedRejection() *record.Rejection {
	return &record.Rejection{Status: http.StatusUnprocessableEntity, Reason: journalapi.ReasonSnapshotIDReused,
		Detail: "snapshot_id reused: it identifies a snapshot of another writer, record type or scope"}
}

// Lookup is step 1 (idempotency): the stored outcome of a snapshot id,
// reported as "duplicate" -- or "held" while it is still held. An id is a
// duplicate only when writer, record type and canonical scope all match the
// stored snapshot; otherwise the Outcome is the 422 refusal of an id reuse
// (found is true, and no stored detail is returned).
func (j *Journal) Lookup(ctx context.Context, id, writer, recordType, scope string) (Outcome, bool, error) {
	return lookup(ctx, j.db, id, writer, recordType, scope)
}

func lookup(ctx context.Context, q queryRower, id, writer, recordType, scope string) (Outcome, bool, error) {
	var status, w, typ, sc string
	var stats []byte
	err := q.QueryRowContext(ctx, `SELECT status, stats, writer, record_type, scope FROM snapshot WHERE id = $1::uuid`, id).
		Scan(&status, &stats, &w, &typ, &sc)
	if errors.Is(err, sql.ErrNoRows) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, err
	}
	if w != writer || typ != recordType || sc != scope {
		return Outcome{Rejection: reusedRejection()}, true, nil
	}
	out := Outcome{Status: journalapi.StatusDuplicate}
	if status == journalapi.StatusHeld {
		out.Status = journalapi.StatusHeld
	}
	_ = json.Unmarshal(stats, &out.Stats)
	return out, true, nil
}

// effects collects what a write reports to the Recorder. It is emitted only
// after the transaction commits: a rolled-back write reports nothing.
type effects struct{ fns []func() }

func (e *effects) add(f func()) { e.fns = append(e.fns, f) }
func (e *effects) emit() {
	for _, f := range e.fns {
		f()
	}
}

// Apply runs 4.4 for a snapshot that passed validation and policy.
// payload is the request body, stored as received.
//
// Erasure (4.13): a snapshot is refused with 410 when its observed_at is
// less than record.MaxFutureSkew after an erasure of its subject, not only
// when it is before it. The writer's clock may run ahead of the journal's by
// up to the skew validation allows, so a snapshot stamped inside that window
// after the erase may in fact have been observed before it; it is treated
// as pre-erasure. One observed exactly erased_at+MaxFutureSkew or later is
// accepted.
//
// A snapshot id is a duplicate only if writer, record type and scope match
// the stored snapshot; any other reuse is refused with 422 (also when two
// writers race for the id).
func (j *Journal) Apply(ctx context.Context, writer string, n *record.Normalized, payload []byte) (Outcome, error) {
	start := time.Now()
	var out Outcome
	resend := false
	ef := &effects{}
	err := j.inTx(ctx, func(tx *sql.Tx) error {
		if err := lockSubject(ctx, tx, n.Subject); err != nil {
			return err
		}
		o, found, err := lookup(ctx, tx, n.ID, writer, n.Type.Name, n.ScopeKey)
		if err != nil || found {
			out = o
			resend = o.Rejection == nil
			return err
		}
		erased, err := erasedAfter(ctx, tx, n.Subject, n.ObservedAt)
		if err != nil {
			return err
		}
		if erased {
			out.Rejection = &record.Rejection{Status: http.StatusGone, Reason: journalapi.ReasonErasedSubject,
				Detail: "observed before, or within the clock-skew allowance after, the subject's erasure"}
			return nil
		}
		if rej := j.checkSubject(n.Subject); rej != nil {
			out.Rejection = rej
			return nil
		}
		out, err = j.applyFromOrdering(ctx, tx, n, writer, payload, "", ef)
		return err
	})
	if errors.Is(err, errSnapshotIDReused) {
		out, err = Outcome{Rejection: reusedRejection()}, nil
	}
	j.o.Metrics.ApplyDuration(time.Since(start))
	if err == nil && out.Rejection == nil {
		ef.emit()
		status := out.Status
		if resend {
			status = journalapi.StatusDuplicate // a resend of a held snapshot is not a second hold
		}
		j.o.Metrics.Snapshot(n.Type.Name, status)
	}
	return out, err
}

// erasedAfter reports whether the subject was erased after the observation,
// allowing for the writer's clock skew (see Apply).
func erasedAfter(ctx context.Context, tx *sql.Tx, subject string, observed time.Time) (bool, error) {
	if subject == "" {
		return false, nil
	}
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM erasure WHERE data_subject = $1 AND erased_at > $2 LIMIT 1`,
		subject, observed.Add(-record.MaxFutureSkew)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// checkSubject: a subject not in the current roster is refused with 409, a
// retryable answer -- a removal or a reload may be in progress (4.9b).
func (j *Journal) checkSubject(subject string) *record.Rejection {
	if subject == "" {
		return nil
	}
	if r := j.o.Roster(); r == nil || !r.Has(subject) {
		return &record.Rejection{Status: http.StatusConflict, Reason: journalapi.ReasonUnknownSubject,
			Detail: "data_subject is not in the current roster"}
	}
	return nil
}

// applyFromOrdering is 4.4 from step 4 on. confirmID is set when an
// operator confirms a held snapshot: the guard is skipped and the
// snapshot's existing row is updated instead of inserted.
func (j *Journal) applyFromOrdering(ctx context.Context, tx *sql.Tx, n *record.Normalized, writer string, payload []byte, confirmID string, ef *effects) (Outcome, error) {
	var later int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM snapshot
		WHERE record_type = $1 AND scope = $2 AND status = 'applied' AND observed_at > $3`,
		n.Type.Name, n.ScopeKey, n.ObservedAt).Scan(&later); err != nil {
		return Outcome{}, err
	}
	if later > 0 {
		out := Outcome{Status: journalapi.StatusLate}
		return out, j.storeSnapshot(ctx, tx, n, writer, payload, confirmID, out)
	}
	cur, err := j.loadCurrent(ctx, tx, n.Type, n.Subject, "")
	if err != nil {
		return Outcome{}, err
	}
	plan := diff.Diff(diff.Input{Scope: n.ScopeKey, Complete: n.Complete, Range: n.Range, ObservedAt: n.ObservedAt,
		Current: cur, Records: n.Records})
	out := Outcome{Status: journalapi.StatusApplied, Stats: plan.Stats()}
	if confirmID == "" && j.o.Guard.Hold(plan.LiveInRange, len(plan.Removals)) {
		out.Status = journalapi.StatusHeld
		return out, j.storeSnapshot(ctx, tx, n, writer, payload, "", out)
	}
	src := eventSource{snapshotID: sql.NullString{String: n.ID, Valid: true}, actor: writer,
		observedAt: n.ObservedAt, approximate: n.Approximate}
	if err := j.writePlan(ctx, tx, n.Type, n.Subject, src, cur, plan, ef); err != nil {
		return Outcome{}, err
	}
	if err := j.storeSnapshot(ctx, tx, n, writer, payload, confirmID, out); err != nil {
		return Outcome{}, err
	}
	// Older held snapshots of the scope would now be late (4.5).
	_, err = tx.ExecContext(ctx, `UPDATE snapshot SET status = 'discarded'
		WHERE record_type = $1 AND scope = $2 AND status = 'held' AND observed_at < $3`,
		n.Type.Name, n.ScopeKey, n.ObservedAt)
	return out, err
}

func (j *Journal) storeSnapshot(ctx context.Context, tx *sql.Tx, n *record.Normalized, writer string, payload []byte, confirmID string, out Outcome) error {
	stats, _ := json.Marshal(out.Stats)
	if confirmID != "" {
		_, err := tx.ExecContext(ctx, `UPDATE snapshot SET status = $2, stats = $3 WHERE id = $1::uuid`, confirmID, out.Status, stats)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO snapshot (id, record_type, schema_version, scope, data_subject, complete,
		approximate, observed_at, received_at, writer, status, payload, stats)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		n.ID, n.Type.Name, n.Version, n.ScopeKey, n.Subject, n.Complete, n.Approximate, n.ObservedAt,
		j.o.Now().UTC(), writer, out.Status, payload, stats)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "snapshot_pkey" {
		return errSnapshotIDReused
	}
	return err
}

// loadCurrent reads the rows of one subject and type (one key when key is
// set), with values returned to their canonical form. The rows are locked
// (FOR UPDATE): a rescan batch (Task 21) also locks the rows it rewrites,
// so neither can write verdicts computed from what the other replaced.
// Lock order is safe: rows are locked in primary-key order, as the rescan
// batches lock them, and a rescan never takes the subject or event advisory
// locks an Apply holds.
func (j *Journal) loadCurrent(ctx context.Context, tx *sql.Tx, t record.Type, subject, key string) (map[string]diff.Current, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, scope, to_jsonb(audience), fields, removed, content_hash,
		last_seen_at, last_confirmed_at, scan FROM record_current
		WHERE record_type = $1 AND data_subject = $2 AND ($3 = '' OR key = $3)
		ORDER BY record_type, data_subject, key FOR UPDATE`, t.Name, subject, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]diff.Current{}
	for rows.Next() {
		var c diff.Current
		var aud, fields, scan []byte
		if err := rows.Scan(&c.Key, &c.Scope, &aud, &fields, &c.Removed, &c.Hash, &c.LastSeenAt, &c.LastConfirmedAt, &scan); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(aud, &c.Audience); err != nil {
			return nil, err
		}
		if c.Fields, err = record.Reload(t, fields); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(scan, &c.Scan); err != nil {
			return nil, err
		}
		out[c.Key] = c
	}
	return out, rows.Err()
}

// eventSource is what every event of one write shares.
type eventSource struct {
	snapshotID  sql.NullString // NULL for household writes
	actor       string
	observedAt  time.Time
	approximate bool
}

// writePlan is 4.4 step 9.
func (j *Journal) writePlan(ctx context.Context, tx *sql.Tx, t record.Type, subject string, src eventSource, cur map[string]diff.Current, plan diff.Plan, ef *effects) error {
	all := append(append([]diff.Event(nil), plan.Events...), plan.Removals...)
	// Scanning and rule evaluation happen first, under the subject lock only:
	// a scan can take seconds, and the events lock below is global.
	type built struct {
		ev       diff.Event
		row      eventRow
		next     currentRow
		withheld int
	}
	rows := make([]built, 0, len(all))
	for _, ev := range all {
		row, next, withheld := j.buildEvent(t, subject, src, cur[ev.Key], ev)
		rows = append(rows, built{ev, row, next, withheld})
	}
	if len(rows) > 0 {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
	}
	for _, b := range rows {
		ev, row, next := b.ev, b.row, b.next
		var seq int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO record_event (record_type, data_subject, key, kind,
			schema_version, scope, prev_scope, audience, prev_audience, fields, changed, scan, observed_at,
			prev_observed_at, approximate, snapshot_id, actor, importance, rule, notify, group_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::uuid, $17, $18, $19, $20, $21)
			RETURNING seq`,
			t.Name, subject, ev.Key, string(ev.Kind), t.SchemaVersion, ev.Scope, row.prevScope, nonNil(ev.Audience),
			ev.PrevAudience, row.fields, row.changed, row.scan, src.observedAt, row.prevObserved, src.approximate,
			src.snapshotID, src.actor, row.importance, row.rule, row.notify, row.groupKey).Scan(&seq); err != nil {
			return fmt.Errorf("journal: insert event: %w", err)
		}
		next.lastEventSeq = seq
		if err := upsertCurrent(ctx, tx, t.Name, subject, src, next); err != nil {
			return err
		}
		kind, importance, withheld := string(ev.Kind), row.importance, b.withheld
		ef.add(func() {
			j.o.Metrics.Event(t.Name, kind, importance)
			if withheld > 0 {
				j.o.Metrics.ScanWithheld(t.Name, withheld)
			}
		})
	}
	if len(plan.Confirm) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE record_current
			SET last_seen_at = GREATEST(last_seen_at, $4), last_confirmed_snapshot = $5::uuid, last_confirmed_at = $4
			WHERE record_type = $1 AND data_subject = $2 AND key = ANY($3)`,
			t.Name, subject, plan.Confirm, src.observedAt, src.snapshotID); err != nil {
			return err
		}
	}
	return nil
}

// eventRow holds the columns of one record_event row that need building.
type eventRow struct {
	prevScope    sql.NullString
	fields       []byte // NULL unless created/restored
	changed      []byte // NULL unless updated/removed
	scan         []byte
	prevObserved sql.NullTime
	importance   string
	rule         sql.NullString
	notify       []byte
	groupKey     sql.NullString
}

// currentRow is the record_current row after an event.
type currentRow struct {
	key, scope   string
	audience     []string
	fields       record.Fields
	removed      bool
	lastSeenAt   time.Time
	scan         record.ScanMap
	lastEventSeq int64
}

func (j *Journal) buildEvent(t record.Type, subject string, src eventSource, prev diff.Current, ev diff.Event) (eventRow, currentRow, int) {
	sv := j.o.Scanner.Version()
	withheld := 0
	scanNew := func(v json.RawMessage) string {
		var s string
		_ = json.Unmarshal(v, &s)
		verdict := j.o.Scanner.Verdict(s)
		if verdict != record.VerdictPass {
			withheld++
		}
		return verdict
	}
	row := eventRow{}
	if ev.PrevScope != "" {
		row.prevScope = sql.NullString{String: ev.PrevScope, Valid: true}
	}
	if ev.PrevObservedAt != nil {
		row.prevObserved = sql.NullTime{Time: *ev.PrevObservedAt, Valid: true}
	}
	next := currentRow{key: ev.Key, scope: ev.Scope, audience: ev.Audience, lastSeenAt: src.observedAt}
	evScan := map[string]record.EventScan{}
	scanMap := record.ScanMap{}
	switch ev.Kind {
	case diff.Created, diff.Restored:
		row.fields = ev.After.JSON()
		for f, v := range ev.After {
			if t.IsText(f) {
				verdict := scanNew(v)
				evScan[f] = record.EventScan{New: verdict, ScannerVersion: sv}
				scanMap[f] = record.ScanEntry{Verdict: verdict, ScannerVersion: sv}
			}
		}
		next.fields = ev.After
	case diff.Updated:
		changed := make(map[string][2]json.RawMessage, len(ev.Changed))
		for f, c := range ev.Changed {
			changed[f] = [2]json.RawMessage{c.Old, c.New}
		}
		row.changed, _ = json.Marshal(changed)
		for f, e := range prev.Scan {
			if _, ok := ev.After[f]; ok {
				scanMap[f] = e // unchanged values keep their verdict
			}
		}
		for f, c := range ev.Changed {
			if !t.IsText(f) {
				continue
			}
			es := record.EventScan{ScannerVersion: sv}
			if c.Old != nil {
				// The old value keeps the verdict of the scan that judged
				// it. The event carries one version for both sides: when
				// the old value's came from another scanner (or it has
				// none, and stays "": withheld), the sides were judged by
				// different scanners, and the event says "" (mixed). A
				// rescan judges "" again, both sides, instead of taking
				// the old verdict for current. Naming the old version
				// would not do: in a deployment running two scanner
				// versions, the replica holding the older one would label
				// a newer New verdict with the older version, and the
				// replica holding the newer one would take the old pass
				// for its own.
				old := prev.Scan[f]
				es.Old = old.Verdict
				if old.ScannerVersion != sv {
					es.ScannerVersion = ""
				}
			}
			if c.New != nil {
				es.New = scanNew(c.New)
				scanMap[f] = record.ScanEntry{Verdict: es.New, ScannerVersion: sv}
			} else {
				delete(scanMap, f)
			}
			evScan[f] = es
		}
		next.fields = ev.After
	case diff.Removed:
		row.changed = ev.Before.JSON()
		for f := range ev.Before {
			if t.IsText(f) {
				evScan[f] = record.EventScan{Old: prev.Scan[f].Verdict, ScannerVersion: prev.Scan[f].ScannerVersion}
			}
		}
		scanMap = prev.Scan
		next.fields = prev.Fields
		next.removed = true
		next.lastSeenAt = prev.LastSeenAt
	}
	next.scan = scanMap
	row.scan, _ = json.Marshal(evScan)

	view := rules.EventView{RecordType: t.Name, DataSubject: subject, Key: ev.Key, Scope: ev.Scope, Kind: string(ev.Kind), After: ev.After}
	switch ev.Kind {
	case diff.Updated:
		view.Before = ev.Before
	case diff.Removed:
		view.Before, view.After = ev.Before, nil
	}
	o := j.o.Rules.Evaluate(view)
	row.importance = o.Importance
	if o.Rule != "" {
		row.rule = sql.NullString{String: o.Rule, Valid: true}
	}
	if o.GroupKey != "" {
		row.groupKey = sql.NullString{String: o.GroupKey, Valid: true}
	}
	if len(o.Notify) > 0 {
		row.notify, _ = json.Marshal(o.Notify)
	}
	return row, next, withheld
}

func upsertCurrent(ctx context.Context, tx *sql.Tx, recordType, subject string, src eventSource, c currentRow) error {
	scan, _ := json.Marshal(c.scan)
	if c.scan == nil {
		scan = []byte(`{}`)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
		removed, first_seen_at, last_seen_at, last_confirmed_snapshot, last_confirmed_at, content_hash, last_event_seq, scan)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid, $8, $11, $12, $13)
		ON CONFLICT (record_type, data_subject, key) DO UPDATE SET
			scope = EXCLUDED.scope, audience = EXCLUDED.audience, fields = EXCLUDED.fields, removed = EXCLUDED.removed,
			last_seen_at = EXCLUDED.last_seen_at, last_confirmed_snapshot = EXCLUDED.last_confirmed_snapshot,
			last_confirmed_at = EXCLUDED.last_confirmed_at, content_hash = EXCLUDED.content_hash,
			last_event_seq = EXCLUDED.last_event_seq, scan = EXCLUDED.scan`,
		recordType, subject, c.key, c.scope, nonNil(c.audience), c.fields.JSON(), c.removed, src.observedAt, c.lastSeenAt,
		src.snapshotID, c.fields.Hash(), c.lastEventSeq, scan)
	return err
}

// nonNil turns a nil list into an empty one: pgx sends a nil slice as NULL,
// and audience columns are NOT NULL (lists are [], never null).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
