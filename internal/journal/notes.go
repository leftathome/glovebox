package journal

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
)

// WriteNote writes one household record (4.9a, 5.3). writer is the
// authenticated service; the note's on_behalf_of is a roster member whose
// computed grants must cover the subject for the type's fixed audience.
func (j *Journal) WriteNote(ctx context.Context, writer string, n *record.NormNote) (journalapi.NoteResponse, *record.Rejection, error) {
	r := j.o.Roster()
	if r == nil {
		return journalapi.NoteResponse{}, &record.Rejection{Status: http.StatusConflict, Reason: journalapi.ReasonUnknownSubject,
			Detail: "no roster is loaded"}, nil
	}
	// A non-member is refused inside the transaction, after the ban: an
	// erased author off the roster gets 410, not 403.
	grants, member := r.Grants(n.OnBehalfOf)
	now := j.o.Now().UTC()
	resp := journalapi.NoteResponse{Status: "unchanged", Key: n.Key}
	var refused *record.Rejection
	ef := &effects{} // NOTE (Task 15 review): metrics are emitted after the commit
	err := j.inTx(ctx, func(tx *sql.Tx) error {
		// The author lock first, then the subject lock (lockAuthor): an erase
		// of the author, which deletes every note they wrote, never runs
		// beside a write of one.
		if err := lockAuthor(ctx, tx, n.OnBehalfOf); err != nil {
			return err
		}
		if err := lockSubject(ctx, tx, n.Subject); err != nil {
			return err
		}
		// The ban is checked first (410 before 403 and 409): no note about an
		// erased member, and none by one, whatever the time (decision
		// 2026-10-24). The author and subject locks order it against an erase
		// of either.
		if rej, err := banned(ctx, tx, n.Subject, n.OnBehalfOf); err != nil || rej != nil {
			refused = rej
			return err
		}
		if !member {
			refused = policyRefusal("on_behalf_of is not in the roster")
			return nil
		}
		// The household ("", only for a type that allows no subject, which
		// ValidateNote checks) is not a roster member; the grant check
		// below still needs a household grant ("", t), as in Apply.
		if n.Subject != "" && !r.Has(n.Subject) {
			refused = &record.Rejection{Status: http.StatusConflict, Reason: journalapi.ReasonUnknownSubject,
				Detail: "data_subject is not in the current roster"}
			return nil
		}
		if !coversAudience(grants, n.Subject, n.Audience) {
			refused = policyRefusal("on_behalf_of holds no grant on this subject for the type's audience")
			return nil
		}
		cur, err := j.loadCurrent(ctx, tx, n.Type, n.Subject, n.Key)
		if err != nil {
			return err
		}
		plan := diff.Diff(diff.Input{Scope: n.Scope, ObservedAt: now, Current: cur,
			Records: []record.NormRecord{{Key: n.Key, DataSubject: n.Subject, Audience: n.Audience, Fields: n.Fields, Hash: n.Hash}}})
		if len(plan.Stale) > 0 {
			// The stored note was confirmed later than now (the core clock
			// stepped back). A byte-identical resend is already stored:
			// unchanged. A changed note cannot be ordered after it, and
			// reporting "unchanged" would drop the correction: retryable 409.
			if c := cur[n.Key]; !c.Removed && bytes.Equal(c.Hash, n.Hash) && c.Scope == n.Scope {
				return nil
			}
			refused = &record.Rejection{Status: http.StatusConflict, Reason: journalapi.ReasonConflict,
				Detail: "the stored note is newer than the journal clock; retry later"}
			return nil
		}
		if len(plan.Events) == 0 {
			return nil
		}
		src := eventSource{actor: writer + ";on_behalf_of=" + n.OnBehalfOf, observedAt: now}
		if err := j.writePlan(ctx, tx, n.Type, n.Subject, src, cur, plan, ef); err != nil {
			return err
		}
		resp.Status = string(plan.Events[0].Kind)
		return tx.QueryRowContext(ctx, `SELECT last_event_seq FROM record_current
			WHERE record_type = $1 AND data_subject = $2 AND key = $3`, n.Type.Name, n.Subject, n.Key).Scan(&resp.Seq)
	})
	if err != nil {
		return journalapi.NoteResponse{}, nil, err
	}
	if refused != nil {
		return journalapi.NoteResponse{}, refused, nil
	}
	ef.emit()
	return resp, nil, nil
}

func coversAudience(grants []roster.Grant, subject string, audience []string) bool {
	for _, g := range grants {
		if g.Subject != subject {
			continue
		}
		for _, t := range audience {
			if g.Token == t {
				return true
			}
		}
	}
	return false
}

func policyRefusal(detail string) *record.Rejection {
	return &record.Rejection{Status: http.StatusForbidden, Reason: journalapi.ReasonPolicy, Detail: detail}
}
