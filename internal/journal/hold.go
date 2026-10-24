package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// Errors of the held-snapshot decisions.
var (
	ErrNotFound = errors.New("journal: no such snapshot")
	ErrNotHeld  = errors.New("journal: the snapshot is not held")
)

func (j *Journal) snapshotSubject(ctx context.Context, id string) (string, error) {
	var subject string
	err := j.db.QueryRowContext(ctx, `SELECT data_subject FROM snapshot WHERE id = $1::uuid`, id).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return subject, err
}

// WriteCheck decides, under the current policy, whether writer may still
// write a snapshot of type t in scope sc (policy.Service.MayWrite).
type WriteCheck func(writer string, t record.Type, sc record.Scope) *record.Rejection

// ConfirmHeld applies a held snapshot: 4.4 from step 4 against current
// state, without the removal guard (4.5). The snapshot was authorised when
// it arrived; mayWrite checks its writer again under today's policy, and a
// writer that has lost the right gets nothing applied (discard it instead).
func (j *Journal) ConfirmHeld(ctx context.Context, id string, mayWrite WriteCheck) (Outcome, error) {
	if mayWrite == nil {
		return Outcome{}, errors.New("journal: ConfirmHeld needs a write check")
	}
	subject, err := j.snapshotSubject(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	var out Outcome
	var recordType string
	ef := &effects{} // NOTE (Task 15 review): metrics are emitted after the commit
	err = j.inTx(ctx, func(tx *sql.Tx) error {
		if err := lockSubject(ctx, tx, subject); err != nil {
			return err
		}
		var status, writer, storedScope string
		var payload []byte
		var received time.Time
		err := tx.QueryRowContext(ctx, `SELECT record_type, scope, status, writer, payload, received_at
			FROM snapshot WHERE id = $1::uuid FOR UPDATE`, id).
			Scan(&recordType, &storedScope, &status, &writer, &payload, &received)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != journalapi.StatusHeld {
			return ErrNotHeld
		}
		var s journalapi.Snapshot
		if err := json.Unmarshal(payload, &s); err != nil {
			return err
		}
		n, rej := record.ValidateSnapshot(j.o.Registry, s, received)
		if rej != nil {
			out.Rejection = rej // the configuration changed since; discard it instead
			return nil
		}
		// The payload is only trusted if it still describes the row it is stored in.
		if n.ID != id || n.Type.Name != recordType || n.ScopeKey != storedScope {
			out.Rejection = &record.Rejection{Status: http.StatusUnprocessableEntity, Reason: journalapi.ReasonSchema,
				Detail: "the stored payload does not match the snapshot's id, record type or scope; discard it"}
			return nil
		}
		// The payload is only trusted if it still describes the row it is stored in.
		if n.ID != id || n.Type.Name != recordType || n.ScopeKey != storedScope {
			out.Rejection = &record.Rejection{Status: http.StatusUnprocessableEntity, Reason: journalapi.ReasonSchema,
				Detail: "the stored payload does not match the snapshot's id, record type or scope; discard it"}
			return nil
		}
		if rej := mayWrite(writer, n.Type, n.Scope); rej != nil {
			out.Rejection = rej
			return nil
		}
		// The ban (4.13): an erase deletes the subject's held snapshots, so
		// one can only be here if it was stored some other way; it is
		// refused like any later snapshot. Discard it.
		if rej, err := banned(ctx, tx, n.Subject); err != nil || rej != nil {
			out.Rejection = rej
			return err
		}
		if rej := j.checkSubject(n.Subject); rej != nil {
			out.Rejection = rej
			return nil
		}
		out, err = j.applyFromOrdering(ctx, tx, n, writer, payload, id, ef)
		return err
	})
	if err == nil && out.Rejection == nil {
		ef.emit()
		j.o.Metrics.Snapshot(recordType, out.Status)
	}
	return out, err
}

// DiscardHeld marks a held snapshot discarded; nothing is applied.
func (j *Journal) DiscardHeld(ctx context.Context, id string) error {
	res, err := j.db.ExecContext(ctx, `UPDATE snapshot SET status = 'discarded' WHERE id = $1::uuid AND status = 'held'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	if _, err := j.snapshotSubject(ctx, id); err != nil {
		return err
	}
	return ErrNotHeld
}
