package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// ErrNoSubject refuses an erase without a subject.
var ErrNoSubject = errors.New("journal: erase needs a data_subject")

// Erase deletes everything the journal holds about one subject (4.13): every
// snapshot (applied, late, held and discarded, payloads included), every
// record_current row and every record_event row, and records the erasure
// (erased_at is the journal clock) so that snapshots observed before it, or
// within the clock-skew allowance after it, are refused with 410 (Apply).
// It is the only operation that deletes event history.
//
// Everything happens in one transaction under the subject lock, so it is
// ordered against Apply, WriteNote and ConfirmHeld of the same subject.
// Deleting events does not take the events lock: it publishes no sequence
// numbers (they are never reused), so readers' cursors stay valid. The
// household (the empty data_subject) is not a subject and is refused.
//
// A second erase of the same subject is allowed: it deletes whatever was
// written since, and writes another erasure row. Apply refuses against
// the latest erased_at of any row, so the refusal window never moves
// backward: a second erase made after the clock stepped back is a no-op for
// the guard. Erasing a subject that holds no data still records the
// erasure, so a spool replay that has not arrived yet is refused.
//
// Erase reports nothing to the Recorder: it has no metric (Task 20).
func (j *Journal) Erase(ctx context.Context, actor, subject string) (journalapi.EraseResponse, error) {
	if subject == "" {
		return journalapi.EraseResponse{}, ErrNoSubject
	}
	resp := journalapi.EraseResponse{DataSubject: subject}
	err := j.inTx(ctx, func(tx *sql.Tx) error {
		if err := lockSubject(ctx, tx, subject); err != nil {
			return err
		}
		// Read after the lock, so erased_at is never earlier than the deletion.
		resp.ErasedAt = j.o.Now().UTC()
		// Take the row locks in the order rescan does (record_current by
		// primary key, then events by seq) before deleting, so Erase and a
		// rescan batch, which holds row locks but no subject lock, cannot
		// deadlock; DELETE alone locks in scan order.
		for _, q := range []string{
			`SELECT 1 FROM record_current WHERE data_subject = $1 ORDER BY record_type, data_subject, key FOR UPDATE`,
			`SELECT 1 FROM record_event WHERE data_subject = $1 ORDER BY seq FOR UPDATE`,
		} {
			rs, err := tx.QueryContext(ctx, q, subject)
			if err != nil {
				return err
			}
			for rs.Next() {
			}
			if err := rs.Err(); err != nil {
				_ = rs.Close()
				return err
			}
			_ = rs.Close()
		}
		del := func(q string) (int64, error) {
			res, err := tx.ExecContext(ctx, q, subject)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		}
		var err error
		if resp.Snapshots, err = del(`DELETE FROM snapshot WHERE data_subject = $1`); err != nil {
			return err
		}
		if resp.Current, err = del(`DELETE FROM record_current WHERE data_subject = $1`); err != nil {
			return err
		}
		if resp.Events, err = del(`DELETE FROM record_event WHERE data_subject = $1`); err != nil {
			return err
		}
		rows, err := json.Marshal(map[string]int64{"snapshot": resp.Snapshots, "record_current": resp.Current, "record_event": resp.Events})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ($1, $2, $3, $4)`,
			subject, resp.ErasedAt, actor, rows)
		return err
	})
	if err != nil {
		return journalapi.EraseResponse{}, err
	}
	return resp, nil
}
