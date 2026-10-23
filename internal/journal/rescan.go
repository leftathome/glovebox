package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// rescanBatch is the number of rows one rescan batch reads, and writes in
// one transaction.
const rescanBatch = 100

// rescanRetries bounds how often a batch re-reads rows that changed under
// it before the rescan gives up with an error.
const rescanRetries = 5

// Rescan re-judges stored text values (4.11) after a scanner or ruleset
// upgrade and updates their verdicts in record_current and record_event,
// releasing values the scanner now passes and withholding those it now
// fails. Nothing else in either table changes. With all false, a value whose
// verdict already came from the current scanner version is skipped.
//
// Work goes in batches of rescanBatch rows, current rows first (by primary
// key), then events (by seq); a failed or interrupted rescan keeps the
// batches it committed, and running it again continues with what is left.
// The counts in the result cover only what was written.
//
// A batch has three steps. The rows are read without a lock, every value is
// scanned with no lock held (a scan can take seconds, and a held row lock
// would stall Apply and Erase of that subject for as long), and only then are
// the batch's rows locked FOR UPDATE and the verdicts written. Rows are
// locked in primary-key order (events: seq order), the order Erase uses
// after its subject lock, and no transaction holds current and event rows
// together, so a rescan cannot deadlock with Erase or Apply (which locks the
// subject, then rows in primary-key order, then the events lock; a rescan
// takes neither advisory lock).
//
// A row is written only if, once locked, it is exactly what was read: the
// same content_hash, last_event_seq and scan for a current row, the same
// scan for an event. A row Erase deleted stays deleted (the write is an
// UPDATE, never an insert). A row that still exists but changed (an Apply
// rewrote it, another rescan judged it) is not written from the stale read:
// it is read, scanned and written again, up to rescanRetries times, because
// an Apply copies the verdicts of the fields it did not change together with
// the scanner version that judged them, so such a row may still hold an old
// scanner's pass. The version filter keeps what the Apply already judged
// from being scanned again. A row still changing after the last retry makes
// Rescan fail: it never returns nil while a row it could not judge remains.
// (A writer that changes the same rows faster than a batch can be re-judged
// can starve the rescan this way; the error says so and a later run
// continues.)
func (j *Journal) Rescan(ctx context.Context, all bool) (journalapi.RescanResult, error) {
	var res journalapi.RescanResult
	if j.o.Scanner == nil {
		return res, errors.New("journal: rescan needs a scanner")
	}
	sv := j.o.Scanner.Version()
	after := [3]string{}
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n, last, err := j.rescanCurrent(ctx, after, all, sv, &res)
		if err != nil {
			return res, err
		}
		if n < rescanBatch {
			break
		}
		after = last
	}
	var seq int64
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n, last, err := j.rescanEvents(ctx, seq, all, sv, &res)
		if err != nil {
			return res, err
		}
		if n < rescanBatch {
			break
		}
		seq = last
	}
	return res, nil
}

// textVerdict judges one stored value. Only an explicit pass from the
// scanner is a pass: an empty answer, a panic and a value that is not a
// string (it cannot be scanned) are errors.
func (j *Journal) textVerdict(v json.RawMessage) (verdict string) {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return record.VerdictError
	}
	defer func() {
		if recover() != nil {
			verdict = record.VerdictError
		}
	}()
	verdict = j.o.Scanner.Verdict(s)
	if verdict == "" {
		verdict = record.VerdictError
	}
	return verdict
}

// tally counts a release (a value that did not pass now does) or a
// withholding (a value that passed now does not).
func tally(t *journalapi.RescanResult, before, after string) {
	switch {
	case before != record.VerdictPass && after == record.VerdictPass:
		t.Released++
	case before == record.VerdictPass && after != record.VerdictPass:
		t.Withheld++
	}
}

func addResult(res *journalapi.RescanResult, b journalapi.RescanResult) {
	res.CurrentRows += b.CurrentRows
	res.EventRows += b.EventRows
	res.Released += b.Released
	res.Withheld += b.Withheld
}

// rescanUpdate is one row's new verdicts and what it must still look like
// when it is locked.
type rescanUpdate struct {
	pk     [3]string
	seq    int64 // current: last_event_seq; events: seq
	hash   []byte
	was    []byte // scan as read
	scan   []byte // scan to write
	counts journalapi.RescanResult
}

// ---- current rows ----------------------------------------------------

type curRead struct {
	pk                 [3]string
	fields, scan, hash []byte
	seq                int64
}

func (j *Journal) readCurrent(ctx context.Context, query string, args ...any) ([]curRead, error) {
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []curRead
	for rows.Next() {
		var c curRead
		if err := rows.Scan(&c.pk[0], &c.pk[1], &c.pk[2], &c.fields, &c.scan, &c.hash, &c.seq); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const curColumns = `SELECT record_type, data_subject, key, fields, scan, content_hash, last_event_seq FROM record_current `

func (j *Journal) rescanCurrent(ctx context.Context, after [3]string, all bool, sv string, res *journalapi.RescanResult) (int, [3]string, error) {
	rows, err := j.readCurrent(ctx, curColumns+`WHERE (record_type, data_subject, key) > ($1, $2, $3)
		ORDER BY record_type, data_subject, key LIMIT $4`, after[0], after[1], after[2], rescanBatch)
	if err != nil {
		return 0, after, err
	}
	n := len(rows)
	last := after
	if n > 0 {
		last = rows[n-1].pk
	}
	for attempt := 0; len(rows) > 0; attempt++ {
		stale, err := j.judgeCurrent(ctx, rows, all, sv, res)
		if err != nil {
			return 0, after, err
		}
		if len(stale) == 0 {
			break
		}
		if attempt == rescanRetries {
			return 0, after, fmt.Errorf("journal: rescan: %d record_current rows kept changing under the rescan (first: %v); run it again", len(stale), stale[0])
		}
		var rt, ds, key []string
		for _, pk := range stale {
			rt, ds, key = append(rt, pk[0]), append(ds, pk[1]), append(key, pk[2])
		}
		rows, err = j.readCurrent(ctx, curColumns+`WHERE (record_type, data_subject, key) IN
			(SELECT * FROM unnest($1::text[], $2::text[], $3::text[])) ORDER BY record_type, data_subject, key`, rt, ds, key)
		if err != nil {
			return 0, after, err
		}
	}
	return n, last, nil
}

// judgeCurrent scans the rows and writes their verdicts. It returns the keys
// of rows that exist but changed since they were read.
func (j *Journal) judgeCurrent(ctx context.Context, rows []curRead, all bool, sv string, res *journalapi.RescanResult) ([][3]string, error) {
	var updates []rescanUpdate
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pk := r.pk
		var fields map[string]json.RawMessage
		var scan record.ScanMap
		if err := json.Unmarshal(r.fields, &fields); err != nil {
			return nil, fmt.Errorf("journal: rescan: %s/%s/%s: fields: %w", pk[0], pk[1], pk[2], err)
		}
		if err := json.Unmarshal(r.scan, &scan); err != nil {
			return nil, fmt.Errorf("journal: rescan: %s/%s/%s: scan: %w", pk[0], pk[1], pk[2], err)
		}
		k := j.kindsOf(pk[0])
		next := maps.Clone(scan)
		if next == nil {
			next = record.ScanMap{}
		}
		u := rescanUpdate{pk: pk, seq: r.seq, hash: r.hash, was: r.scan}
		changed := false
		for _, f := range slices.Sorted(maps.Keys(fields)) {
			cur, scanned := scan[f]
			if !(k.isText(f) || scanned) || (!all && cur.ScannerVersion == sv) {
				continue
			}
			v := j.textVerdict(fields[f])
			tally(&u.counts, cur.Verdict, v)
			next[f] = record.ScanEntry{Verdict: v, ScannerVersion: sv}
			changed = true
		}
		if !changed {
			continue
		}
		u.scan, _ = json.Marshal(next)
		updates = append(updates, u)
	}
	if len(updates) == 0 {
		return nil, nil
	}
	var batch journalapi.RescanResult
	var stale [][3]string
	err := j.inTx(ctx, func(tx *sql.Tx) error {
		batch, stale = journalapi.RescanResult{}, nil
		var rt, ds, key []string
		for _, u := range updates {
			rt, ds, key = append(rt, u.pk[0]), append(ds, u.pk[1]), append(key, u.pk[2])
		}
		locked, err := tx.QueryContext(ctx, `SELECT record_type, data_subject, key, content_hash, last_event_seq, scan
			FROM record_current
			WHERE (record_type, data_subject, key) IN (SELECT * FROM unnest($1::text[], $2::text[], $3::text[]))
			ORDER BY record_type, data_subject, key FOR UPDATE`, rt, ds, key)
		if err != nil {
			return err
		}
		type state struct {
			hash, scan []byte
			seq        int64
		}
		now := map[[3]string]state{}
		for locked.Next() {
			var pk [3]string
			var s state
			if err := locked.Scan(&pk[0], &pk[1], &pk[2], &s.hash, &s.seq, &s.scan); err != nil {
				_ = locked.Close()
				return err
			}
			now[pk] = s
		}
		err = locked.Err()
		_ = locked.Close()
		if err != nil {
			return err
		}
		for _, u := range updates {
			s, ok := now[u.pk]
			if !ok {
				continue // erased: nothing to judge, nothing to bring back
			}
			if s.seq != u.seq || !bytes.Equal(s.hash, u.hash) || !bytes.Equal(s.scan, u.was) {
				stale = append(stale, u.pk) // changed since it was read: judge it again
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE record_current SET scan = $4
				WHERE record_type = $1 AND data_subject = $2 AND key = $3`, u.pk[0], u.pk[1], u.pk[2], u.scan); err != nil {
				return err
			}
			batch.CurrentRows++
			batch.Released += u.counts.Released
			batch.Withheld += u.counts.Withheld
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	addResult(res, batch)
	return stale, nil
}

// ---- events ----------------------------------------------------------

type evRead struct {
	seq                   int64
	rt, kind              string
	version               int
	fields, changed, scan []byte
}

func (j *Journal) readEvents(ctx context.Context, query string, args ...any) ([]evRead, error) {
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []evRead
	for rows.Next() {
		var e evRead
		if err := rows.Scan(&e.seq, &e.rt, &e.version, &e.kind, &e.fields, &e.changed, &e.scan); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const evColumns = `SELECT seq, record_type, schema_version, kind, fields, changed, scan FROM record_event `

func (j *Journal) rescanEvents(ctx context.Context, after int64, all bool, sv string, res *journalapi.RescanResult) (int, int64, error) {
	rows, err := j.readEvents(ctx, evColumns+`WHERE seq > $1 ORDER BY seq LIMIT $2`, after, rescanBatch)
	if err != nil {
		return 0, after, err
	}
	n := len(rows)
	last := after
	if n > 0 {
		last = rows[n-1].seq
	}
	for attempt := 0; len(rows) > 0; attempt++ {
		stale, err := j.judgeEvents(ctx, rows, all, sv, res)
		if err != nil {
			return 0, after, err
		}
		if len(stale) == 0 {
			break
		}
		if attempt == rescanRetries {
			return 0, after, fmt.Errorf("journal: rescan: %d record_event rows kept changing under the rescan (first: seq %d); run it again", len(stale), stale[0])
		}
		rows, err = j.readEvents(ctx, evColumns+`WHERE seq = ANY($1) ORDER BY seq`, stale)
		if err != nil {
			return 0, after, err
		}
	}
	return n, last, nil
}

// judgeEvents scans the events and writes their verdicts. It returns the
// seq of events that exist but whose verdicts changed since they were read.
func (j *Journal) judgeEvents(ctx context.Context, rows []evRead, all bool, sv string, res *journalapi.RescanResult) ([]int64, error) {
	var updates []rescanUpdate
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seq, rt, version := r.seq, r.rt, r.version
		var scan map[string]record.EventScan
		if err := json.Unmarshal(r.scan, &scan); err != nil {
			return nil, fmt.Errorf("journal: rescan: event %d: scan: %w", seq, err)
		}
		// Old and new value of every field the event carries; nil is absent.
		type pair struct{ old, new json.RawMessage }
		values := map[string]pair{}
		switch r.kind {
		case "created", "restored":
			var f map[string]json.RawMessage
			if err := json.Unmarshal(r.fields, &f); err != nil {
				return nil, fmt.Errorf("journal: rescan: event %d: fields: %w", seq, err)
			}
			for name, v := range f {
				values[name] = pair{new: v}
			}
		case "updated":
			var ch map[string][2]json.RawMessage
			if err := json.Unmarshal(r.changed, &ch); err != nil {
				return nil, fmt.Errorf("journal: rescan: event %d: changed: %w", seq, err)
			}
			for name, p := range ch {
				values[name] = pair{old: nilIfNull(p[0]), new: nilIfNull(p[1])}
			}
		case "removed":
			var f map[string]json.RawMessage
			if err := json.Unmarshal(r.changed, &f); err != nil {
				return nil, fmt.Errorf("journal: rescan: event %d: changed: %w", seq, err)
			}
			for name, v := range f {
				values[name] = pair{old: v}
			}
		default:
			continue
		}
		// Text is decided as on read: by the active type, under the field's
		// current name; a type the registry does not know has only text.
		k := j.kindsOf(rt)
		next := maps.Clone(scan)
		if next == nil {
			next = map[string]record.EventScan{}
		}
		u := rescanUpdate{seq: seq, was: r.scan}
		changed := false
		for _, f := range slices.Sorted(maps.Keys(values)) {
			es, scanned := scan[f]
			if !(k.isText(j.o.Registry.ForwardName(rt, version, f)) || scanned) || (!all && es.ScannerVersion == sv) {
				continue
			}
			if v := values[f].old; v != nil {
				verdict := j.textVerdict(v)
				tally(&u.counts, es.Old, verdict)
				es.Old = verdict
			}
			if v := values[f].new; v != nil {
				verdict := j.textVerdict(v)
				tally(&u.counts, es.New, verdict)
				es.New = verdict
			}
			es.ScannerVersion = sv
			next[f] = es
			changed = true
		}
		if !changed {
			continue
		}
		u.scan, _ = json.Marshal(next)
		updates = append(updates, u)
	}
	if len(updates) == 0 {
		return nil, nil
	}
	var batch journalapi.RescanResult
	var stale []int64
	err := j.inTx(ctx, func(tx *sql.Tx) error {
		batch, stale = journalapi.RescanResult{}, nil
		seqs := make([]int64, 0, len(updates))
		for _, u := range updates {
			seqs = append(seqs, u.seq)
		}
		locked, err := tx.QueryContext(ctx, `SELECT seq, scan FROM record_event WHERE seq = ANY($1) ORDER BY seq FOR UPDATE`, seqs)
		if err != nil {
			return err
		}
		now := map[int64][]byte{}
		for locked.Next() {
			var seq int64
			var s []byte
			if err := locked.Scan(&seq, &s); err != nil {
				_ = locked.Close()
				return err
			}
			now[seq] = s
		}
		err = locked.Err()
		_ = locked.Close()
		if err != nil {
			return err
		}
		for _, u := range updates {
			s, ok := now[u.seq]
			if !ok {
				continue // erased
			}
			if !bytes.Equal(s, u.was) {
				stale = append(stale, u.seq) // judged by someone else since it was read
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE record_event SET scan = $2 WHERE seq = $1`, u.seq, u.scan); err != nil {
				return err
			}
			batch.EventRows++
			batch.Released += u.counts.Released
			batch.Withheld += u.counts.Withheld
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	addResult(res, batch)
	return stale, nil
}
