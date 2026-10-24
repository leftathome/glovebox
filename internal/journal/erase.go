package journal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// ErrNoSubject refuses an erase without a subject.
var ErrNoSubject = errors.New("journal: erase needs a data_subject")

// erasedPrefix starts every pseudonym. Roster ids cannot contain ':', so a
// pseudonym never equals or collides with a member id or a note key.
const erasedPrefix = "erased:"

// guardiansToken is the audience part a kept note keeps (4.13).
const guardiansToken = "guardians"

// authoredSQL selects, over table alias t, the household notes written by
// $1: a household record's key is "<date>:<on_behalf_of>" (plus
// ":erased:<token>" once kept for an erased subject), its author is the
// second ':'-part, and neither a date nor a roster id contains ':'. $2 is
// record.OwnerHousehold. Records of other types are never matched, whatever
// their key.
const authoredSQL = `(t.record_type IN (SELECT name FROM record_type WHERE owner = $2) AND split_part(t.key, ':', 2) = $1)`

// Erase removes what the journal holds about one member (4.13, decision
// 2026-10-23), in one transaction:
//
//   - Every note the member wrote, about anyone (any subject, the household,
//     themselves, and notes kept from an earlier erasure), with its events.
//   - Every note someone else wrote about the member whose audience has a
//     guardians part is kept: moved to the household (data_subject ""),
//     its key suffixed with ":<pseudonym>", its scope the household's, its
//     audience narrowed to [guardians], its events rewritten in place (same
//     seq, group_key cleared). Its text is kept as written. A note with no
//     guardians part has no reader left once the subject part is gone, and
//     is deleted with the subject's data.
//   - Everything else of the subject: every snapshot (applied, late, held
//     and discarded, payloads included), record_current row and
//     record_event row.
//
// The pseudonym is "erased:" and 128 random bits, drawn per erasure and
// stored nowhere else: it cannot be recomputed from the id (a hash of a
// roster id could, by trying the household's few ids), it links none of
// the member's other erasures, and the erasure row does not record it.
//
// The erasure is recorded (erased_at is the journal clock) so that
// snapshots observed before it, or within the clock-skew allowance after
// it, are refused with 410 (Apply), and notes from before it too
// (WriteNote). It is the only operation that deletes or rewrites event
// history.
//
// Locks: first the member's author lock (WriteNote takes the author lock of
// its on_behalf_of before its subject lock), so no note by the member can be
// written or changed while the erase runs; then the subject's, the
// household's and those of every subject the member wrote notes about, in
// sorted order, before any row lock. Author locks always come before subject
// locks and every writer takes at most one author lock, so Erase cannot
// deadlock with Apply, WriteNote or another erase. Under the author lock the
// set of subjects the member wrote about cannot grow: only WriteNote by the
// member adds to it, and another erase can only move such a note to the
// household, whose lock is always taken. Then the rows are locked in
// rescan's order: record_current by primary key, then record_event by seq.
// Deleting or rewriting events does not take the events lock: it publishes
// no sequence numbers (they are never reused), so readers' cursors stay
// valid. The household (the empty data_subject) is not a subject and is
// refused.
//
// A second erase of the same subject is allowed: it deletes whatever was
// written since, with a new pseudonym for what it keeps, and writes another
// erasure row. Apply refuses against the latest erased_at of any row, so
// the refusal window never moves backward. Erasing a subject that holds no
// data still records the erasure, so a spool replay that has not arrived
// yet is refused.
//
// After the erase, new notes by or about the member are accepted as new
// data (a note about them observed before erased_at is refused), and copies
// a reader already took from the events feed are outside the journal: erase
// does not reach them.
//
// Erase reports nothing to the Recorder: it has no metric (Task 20).
func (j *Journal) Erase(ctx context.Context, actor, subject string) (journalapi.EraseResponse, error) {
	if subject == "" {
		return journalapi.EraseResponse{}, ErrNoSubject
	}
	pseudonym, err := newPseudonym()
	if err != nil {
		return journalapi.EraseResponse{}, err
	}
	resp := journalapi.EraseResponse{DataSubject: subject}
	err = j.inTx(ctx, func(tx *sql.Tx) error {
		if err := lockAuthor(ctx, tx, subject); err != nil {
			return err
		}
		authored, err := authoredSubjects(ctx, tx, subject)
		if err != nil {
			return err
		}
		if j.beforeEraseLocks != nil {
			j.beforeEraseLocks()
		}
		for _, s := range sortedSet(append([]string{subject, ""}, authored...)) {
			if err := lockSubject(ctx, tx, s); err != nil {
				return err
			}
		}
		// Read after the locks, so erased_at is never earlier than the deletion.
		resp.ErasedAt = j.o.Now().UTC()
		return j.eraseLocked(ctx, tx, actor, subject, pseudonym, &resp)
	})
	if err != nil {
		return journalapi.EraseResponse{}, err
	}
	return resp, nil
}

func newPseudonym() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("journal: erase: pseudonym: %w", err)
	}
	return erasedPrefix + hex.EncodeToString(b), nil
}

func (j *Journal) eraseLocked(ctx context.Context, tx *sql.Tx, actor, subject, pseudonym string, resp *journalapi.EraseResponse) error {
	// Take the row locks in the order rescan does (record_current by
	// primary key, then events by seq) before changing anything, so Erase
	// and a rescan batch, which holds row locks but no subject lock, cannot
	// deadlock; DELETE and UPDATE alone lock in scan order.
	for _, q := range []string{
		`SELECT 1 FROM record_current t WHERE t.data_subject = $1 OR ` + authoredSQL + ` ORDER BY record_type, data_subject, key FOR UPDATE`,
		`SELECT 1 FROM record_event t WHERE t.data_subject = $1 OR ` + authoredSQL + ` ORDER BY seq FOR UPDATE`,
	} {
		rs, err := tx.QueryContext(ctx, q, subject, record.OwnerHousehold)
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
	exec := func(q string, args ...any) (int64, error) {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	// Rule 1: the member's own notes, about anyone.
	evA, err := exec(`DELETE FROM record_event t WHERE `+authoredSQL, subject, record.OwnerHousehold)
	if err != nil {
		return err
	}
	if resp.NotesDeletedAsAuthor, err = exec(`DELETE FROM record_current t WHERE `+authoredSQL, subject, record.OwnerHousehold); err != nil {
		return err
	}
	// Rule 2: others' notes about the member that guardians can read.
	var evK int64
	if resp.NotesPseudonymised, evK, err = j.keepNotes(ctx, tx, subject, pseudonym); err != nil {
		return err
	}
	// Rule 3: everything else of the subject.
	if resp.Snapshots, err = exec(`DELETE FROM snapshot WHERE data_subject = $1`, subject); err != nil {
		return err
	}
	if resp.Current, err = exec(`DELETE FROM record_current WHERE data_subject = $1`, subject); err != nil {
		return err
	}
	if resp.Events, err = exec(`DELETE FROM record_event WHERE data_subject = $1`, subject); err != nil {
		return err
	}
	resp.Current += resp.NotesDeletedAsAuthor
	resp.Events += evA + evK
	// Counts only: neither the pseudonym nor any author is recorded.
	rows, err := json.Marshal(map[string]int64{"snapshot": resp.Snapshots, "record_current": resp.Current,
		"record_event": resp.Events, "notes_deleted_as_author": resp.NotesDeletedAsAuthor,
		"notes_pseudonymised": resp.NotesPseudonymised})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ($1, $2, $3, $4)`,
		subject, resp.ErasedAt, actor, rows)
	return err
}

// keepNotes moves the subject's household records written by others (rule 1
// has already deleted the subject's own) that have a guardians part to the
// household under the pseudonym, events first, and returns how many records
// it kept and how many of their events it deleted. Visibility stays the existing rule: a row of the household with
// audience [guardians] is read by holders of ("", guardians), the
// household's guardians (spec 11 3.4); the subject part, which named the
// erased member, is gone, and no grant names a pseudonym.
func (j *Journal) keepNotes(ctx context.Context, tx *sql.Tx, subject, pseudonym string) (int64, int64, error) {
	const kept = `data_subject = $1 AND $2 = ANY(audience)
		AND record_type IN (SELECT name FROM record_type WHERE owner = $3)`
	rs, err := tx.QueryContext(ctx, `SELECT DISTINCT record_type FROM record_current WHERE `+kept+` ORDER BY 1`,
		subject, guardiansToken, record.OwnerHousehold)
	if err != nil {
		return 0, 0, err
	}
	var types []string
	for rs.Next() {
		var t string
		if err := rs.Scan(&t); err != nil {
			_ = rs.Close()
			return 0, 0, err
		}
		types = append(types, t)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return 0, 0, err
	}
	_ = rs.Close()
	if len(types) == 0 {
		return 0, 0, nil
	}
	// The trigger admits this rewrite only with the flag set (migration
	// 0002). set_config(..., true) is transaction-local: the setting reverts
	// at commit or rollback, so it never outlives this transaction on the
	// pooled connection. The rest of the transaction only deletes and
	// inserts the erasure row, which the trigger does not see. The flag
	// guards against application mistakes; it is not a permission boundary
	// (anyone who can run SQL here can set it, or drop the trigger).
	if _, err := tx.ExecContext(ctx, `SELECT set_config('glovebox_journal.erase_rewrite', 'on', true)`); err != nil {
		return 0, 0, err
	}
	var n, gone int64
	for _, t := range types {
		scope := record.HouseholdScope(t, "")
		const keys = `record_type = $6 AND data_subject = $1 AND key IN (
				SELECT key FROM record_current WHERE record_type = $6 AND ` + kept + `)`
		// An event whose audience has no guardians part (written under a
		// type version with another fixed audience) would be read by
		// nobody: it is deleted rather than kept as unreadable text. The
		// newest event of a kept note has the row's audience, so it stays,
		// and last_event_seq still names it.
		res, err := tx.ExecContext(ctx, `DELETE FROM record_event WHERE NOT ($2 = ANY(audience))
			AND record_type = $4 AND data_subject = $1 AND key IN (
				SELECT key FROM record_current WHERE record_type = $4 AND `+kept+`)`,
			subject, guardiansToken, record.OwnerHousehold, t)
		if err != nil {
			return 0, 0, err
		}
		d, err := res.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		gone += d
		// The others keep the guardians part of their audience and nothing
		// else, their prev_audience likewise (empty when it had none: an old
		// value across that change matches nobody); group_key may embed the
		// subject or the scope and is cleared.
		if _, err := tx.ExecContext(ctx, `UPDATE record_event SET data_subject = '', key = key || ':' || $4, scope = $5,
			prev_scope = CASE WHEN prev_scope IS NULL THEN NULL ELSE $5 END,
			audience = ARRAY[$2]::text[],
			prev_audience = CASE WHEN prev_audience IS NULL THEN NULL
				WHEN $2 = ANY(prev_audience) THEN ARRAY[$2]::text[] ELSE '{}'::text[] END,
			group_key = NULL
			WHERE `+keys,
			subject, guardiansToken, record.OwnerHousehold, pseudonym, scope, t); err != nil {
			return 0, 0, err
		}
		res, err = tx.ExecContext(ctx, `UPDATE record_current SET data_subject = '', key = key || ':' || $4, scope = $5,
			audience = ARRAY[$2]::text[]
			WHERE record_type = $6 AND `+kept,
			subject, guardiansToken, record.OwnerHousehold, pseudonym, scope, t)
		if err != nil {
			return 0, 0, err
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		n += k
	}
	return n, gone, nil
}

// authoredSubjects lists the subjects ("" for the household) of the notes
// member wrote.
func authoredSubjects(ctx context.Context, tx *sql.Tx, member string) ([]string, error) {
	rs, err := tx.QueryContext(ctx, `SELECT DISTINCT t.data_subject FROM record_current t WHERE `+authoredSQL,
		member, record.OwnerHousehold)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rs.Err()
}

// sortedSet sorts and deduplicates.
func sortedSet(xs []string) []string {
	sort.Strings(xs)
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
