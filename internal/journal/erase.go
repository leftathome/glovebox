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
	"strconv"
	"strings"

	"github.com/leftathome/glovebox/internal/journal/access"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// ErrNoSubject refuses an erase without a subject, or of something that is
// not a member id (the household "", a kept note's pseudonym).
var ErrNoSubject = errors.New("journal: erase needs a member id as data_subject")

// authoredSQL selects, over table alias t, the household notes written by
// $1: a household record's key is "<date>:<on_behalf_of>", its author is the
// second ':'-part, and neither a date nor a roster id contains ':'. $2 is
// record.OwnerHousehold. Records of other types are never matched, whatever
// their key. A kept note keeps its key, so it is matched too.
const authoredSQL = `(t.record_type IN (SELECT name FROM record_type WHERE owner = $2) AND split_part(t.key, ':', 2) = $1)`

// readableSQL selects, over table alias t, the kept notes member $1 is a
// frozen reader of.
const readableSQL = `(t.data_subject IN (SELECT pseudonym FROM erasure_reader WHERE member = $1))`

// Erase removes what the journal holds about one member and bans them (4.13,
// decisions 2026-10-23 and 2026-10-24). The member may still be on the
// roster; one roster snapshot, read at the start, decides the frozen
// readers, and removing the member from the roster through gitops can
// follow at any time. In one transaction:
//
//   - Rule 1: every note the member wrote, about anyone (any subject, the
//     household, themselves, and notes kept from an earlier erasure), with
//     its events.
//   - The member stops being a frozen reader of any kept note; a kept note
//     left with no reader is deleted.
//   - Rule 2: every note someone else wrote about the member is kept if it
//     has a reader left. Its readers are frozen: exactly the roster members
//     who could read it at this moment under the read visibility rule (the
//     subject's guardians, for instance), plus its author if on the roster,
//     never the erased member nor anyone erased before; they are written to
//     erasure_reader. The note moves to its own pseudonymous subject
//     "erased:<pseudonym>:<n>" (one pseudonym per erasure, n per note), its
//     scope becomes the household scope of its type, its events are
//     rewritten in place (same seq, group_key cleared); key, audience,
//     fields and text stay exactly as written. From then on only its frozen
//     readers see it (access.Visible); no roster grant names a pseudonym.
//     A note with no frozen reader is deleted with the subject's data.
//   - Rule 3: everything else of the subject: every snapshot (applied,
//     late, held and discarded, payloads included), record_current row and
//     record_event row.
//
// The pseudonym is "erased:" and 128 random bits, drawn per erasure and
// stored only in the kept rows and their erasure_reader rows: it cannot be
// recomputed from the id (a hash of a roster id could, by trying the
// household's few ids), it links none of the member's other erasures, and
// the erasure row does not record it.
//
// The erasure row is a ban (decision 2026-10-24): from its commit on, every
// snapshot about the member (Apply, ConfirmHeld) and every note about or by
// them (WriteNote) is refused with 410 erased_subject, whatever its time
// (banned). Nothing un-erases: putting an erased person back would need a
// deliberate un-erase, which does not exist yet. Erase is the only
// operation that deletes or rewrites event history.
//
// Locks: first the member's author lock (WriteNote takes the author lock of
// its on_behalf_of before its subject lock), so no note by the member can be
// written or changed while the erase runs; then the subject's, the
// household's and those of every subject the member wrote notes about, in
// sorted order, before any row lock. Author locks always come before subject
// locks and every writer takes at most one author lock, so Erase cannot
// deadlock with Apply, WriteNote or another erase. Every erase takes the
// household's lock, so erases run one at a time; under the author lock the
// set of subjects the member wrote about can then not change (only
// WriteNote by the member adds to it). Kept notes live under pseudonyms
// nobody but an erase writes, so they need no subject lock. Then the rows
// are locked in rescan's order: record_current by primary key, then
// record_event by seq. Deleting or rewriting events does not take the
// events lock: it publishes no sequence numbers (they are never reused), so
// readers' cursors stay valid.
//
// A second erase of the same subject is allowed and writes another erasure
// row; the ban stays. Erasing a subject that holds no data still records the
// erasure, so the ban holds for data that has not arrived yet.
//
// Copies already delivered outside the journal (a reader past the events
// cursor: the openclaw inbox, audiences, memory vaults) are not erased; that
// is a separate, later improvement.
//
// Erase reports nothing to the Recorder: it has no metric (Task 20).
func (j *Journal) Erase(ctx context.Context, actor, subject string) (journalapi.EraseResponse, error) {
	if !roster.ValidID(subject) {
		return journalapi.EraseResponse{}, ErrNoSubject
	}
	ros := j.o.Roster()
	if ros == nil {
		return journalapi.EraseResponse{}, ErrNoRoster
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
			if access.IsErased(s) {
				continue
			}
			if err := lockSubject(ctx, tx, s); err != nil {
				return err
			}
		}
		// Read after the locks, so erased_at is never earlier than the deletion.
		resp.ErasedAt = j.o.Now().UTC()
		return j.eraseLocked(ctx, tx, ros, actor, subject, pseudonym, &resp)
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
	return access.ErasedPrefix + hex.EncodeToString(b), nil
}

func (j *Journal) eraseLocked(ctx context.Context, tx *sql.Tx, ros *roster.Roster, actor, subject, pseudonym string, resp *journalapi.EraseResponse) error {
	// Take the row locks in the order rescan does (record_current by
	// primary key, then events by seq) before changing anything, so Erase
	// and a rescan batch, which holds row locks but no subject lock, cannot
	// deadlock; DELETE and UPDATE alone lock in scan order.
	for _, q := range []string{
		`SELECT 1 FROM record_current t WHERE t.data_subject = $1 OR ` + authoredSQL + ` OR ` + readableSQL +
			` ORDER BY record_type, data_subject, key FOR UPDATE`,
		`SELECT 1 FROM record_event t WHERE t.data_subject = $1 OR ` + authoredSQL + ` OR ` + readableSQL + ` ORDER BY seq FOR UPDATE`,
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
	// The member is no reader of anything any more; a kept note left with
	// no reader is dropped, and reader rows of dropped notes go with them.
	if _, err := exec(`DELETE FROM erasure_reader WHERE member = $1`, subject); err != nil {
		return err
	}
	const orphaned = `starts_with(t.data_subject, '` + access.ErasedPrefix + `')
		AND NOT EXISTS (SELECT 1 FROM erasure_reader r WHERE r.pseudonym = t.data_subject)`
	evD, err := exec(`DELETE FROM record_event t WHERE ` + orphaned)
	if err != nil {
		return err
	}
	curD, err := exec(`DELETE FROM record_current t WHERE ` + orphaned)
	if err != nil {
		return err
	}
	// Kept-note rows of notes that are gone (dropped here, or the member's
	// own, deleted by rule 1); their reader rows go with them (cascade).
	if _, err := exec(`DELETE FROM erasure_kept k WHERE NOT EXISTS (
		SELECT 1 FROM record_current c WHERE c.data_subject = k.pseudonym)`); err != nil {
		return err
	}
	// Rule 2: others' notes about the member, with their frozen readers.
	if resp.NotesPseudonymised, err = keepNotes(ctx, tx, ros, subject, pseudonym); err != nil {
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
	resp.Current += resp.NotesDeletedAsAuthor + curD
	resp.Events += evA + evD
	// Counts only: neither the pseudonym nor any author or reader is recorded.
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

// keptNote is a note about the erased subject that someone else wrote.
type keptNote struct {
	recordType, key string
	audience        []string
}

// frozenReaders is who reads a kept note from now on: every member of ros
// who can read it now, by the same rule reads use (access.Visible over the
// member's roster grants; nobody holds a frozen-reader grant on a subject
// that is not a pseudonym), and its author if on the roster; never the
// erased subject, nor any member in erased (erased earlier, so banned).
// Sorted.
func frozenReaders(ros *roster.Roster, subject string, erased map[string]bool, n keptNote) []string {
	var out []string
	for _, m := range ros.Members() {
		if m == subject || erased[m] {
			continue
		}
		g, _ := ros.Grants(m)
		if access.Visible(g, subject, n.audience) {
			out = append(out, m)
		}
	}
	if author := authorOf(n.key); author != subject && !erased[author] && ros.Has(author) {
		out = append(out, author)
	}
	return sortedSet(out)
}

// authorOf is the on_behalf_of part of a household note key.
// It is the same part authoredSQL matches (split_part(key, ':', 2)).
func authorOf(key string) string {
	if p := strings.SplitN(key, ":", 3); len(p) >= 2 {
		return p[1]
	}
	return ""
}

// keepNotes keeps the subject's household records written by others (rule 1
// has already deleted the subject's own) that have a frozen reader, events
// first, and returns how many it kept. The others stay for rule 3.
func keepNotes(ctx context.Context, tx *sql.Tx, ros *roster.Roster, subject, pseudonym string) (int64, error) {
	rs, err := tx.QueryContext(ctx, `SELECT record_type, key, to_jsonb(audience) FROM record_current
		WHERE data_subject = $1 AND record_type IN (SELECT name FROM record_type WHERE owner = $2)
		ORDER BY record_type, key`, subject, record.OwnerHousehold)
	if err != nil {
		return 0, err
	}
	var notes []keptNote
	for rs.Next() {
		var n keptNote
		var aud []byte
		if err := rs.Scan(&n.recordType, &n.key, &aud); err != nil {
			_ = rs.Close()
			return 0, err
		}
		if err := json.Unmarshal(aud, &n.audience); err != nil {
			_ = rs.Close()
			return 0, err
		}
		notes = append(notes, n)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return 0, err
	}
	_ = rs.Close()
	// Members erased earlier are banned and never read a kept note.
	erased := map[string]bool{}
	ers, err := tx.QueryContext(ctx, `SELECT DISTINCT data_subject FROM erasure`)
	if err != nil {
		return 0, err
	}
	for ers.Next() {
		var e string
		if err := ers.Scan(&e); err != nil {
			_ = ers.Close()
			return 0, err
		}
		erased[e] = true
	}
	if err := ers.Err(); err != nil {
		_ = ers.Close()
		return 0, err
	}
	_ = ers.Close()
	// The trigger admits the rewrite below only with the flag set
	// (migration 0003). set_config(..., true) is transaction-local: the
	// setting reverts at commit or rollback, so it never outlives this
	// transaction on the pooled connection. The flag guards against
	// application mistakes; it is not a permission boundary (anyone who can
	// run SQL here can set it, or drop the trigger).
	flagged := false
	var kept int64
	for _, n := range notes {
		readers := frozenReaders(ros, subject, erased, n)
		if len(readers) == 0 {
			continue // nobody can read it: rule 3 deletes it
		}
		if !flagged {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('glovebox_journal.erase_rewrite', 'on', true)`); err != nil {
				return 0, err
			}
			flagged = true
		}
		kept++
		marker := pseudonym + ":" + strconv.FormatInt(kept, 10)
		// The audience the readers were frozen from, and the readers; the
		// author is marked, and sees the whole history (access.Visible).
		if _, err := tx.ExecContext(ctx, `INSERT INTO erasure_kept (pseudonym, audience) VALUES ($1, $2)`,
			marker, nonNil(n.audience)); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO erasure_reader (pseudonym, member, author)
			SELECT $1, m, m = $3 FROM unnest($2::text[]) AS m`, marker, readers, authorOf(n.key)); err != nil {
			return 0, err
		}
		scope := record.HouseholdScope(n.recordType, "")
		// group_key may embed the subject or the scope and is cleared.
		if _, err := tx.ExecContext(ctx, `UPDATE record_event SET data_subject = $4, scope = $5,
			prev_scope = CASE WHEN prev_scope IS NULL THEN NULL ELSE $5 END, group_key = NULL
			WHERE record_type = $2 AND data_subject = $1 AND key = $3`,
			subject, n.recordType, n.key, marker, scope); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE record_current SET data_subject = $4, scope = $5
			WHERE record_type = $2 AND data_subject = $1 AND key = $3`,
			subject, n.recordType, n.key, marker, scope); err != nil {
			return 0, err
		}
	}
	return kept, nil
}

// authoredSubjects lists the subjects ("" for the household, a pseudonym
// for a kept note) of the notes member wrote.
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
