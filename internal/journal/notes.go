package journal

import (
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
	grants, ok := r.Grants(n.OnBehalfOf)
	if !ok {
		return journalapi.NoteResponse{}, policyRefusal("on_behalf_of is not in the roster"), nil
	}
	if rej := j.checkSubject(n.Subject); rej != nil {
		return journalapi.NoteResponse{}, rej, nil
	}
	if !coversAudience(grants, n.Subject, n.Audience) {
		return journalapi.NoteResponse{}, policyRefusal("on_behalf_of holds no grant on this subject for the type's audience"), nil
	}
	now := j.o.Now().UTC()
	resp := journalapi.NoteResponse{Status: "unchanged", Key: n.Key}
	var refused *record.Rejection
	ef := &effects{} // NOTE (Task 15 review): metrics are emitted after the commit
	err := j.inTx(ctx, func(tx *sql.Tx) error {
		if err := lockSubject(ctx, tx, n.Subject); err != nil {
			return err
		}
		// A note is observed now: it is refused while the subject's erasure
		// is within the clock-skew allowance (see Apply, 4.13).
		erased, err := erasedAfter(ctx, tx, n.Subject, now)
		if err != nil {
			return err
		}
		if erased {
			refused = &record.Rejection{Status: http.StatusGone, Reason: journalapi.ReasonErasedSubject,
				Detail: "the subject was erased"}
			return nil
		}
		cur, err := j.loadCurrent(ctx, tx, n.Type, n.Subject, n.Key)
		if err != nil {
			return err
		}
		plan := diff.Diff(diff.Input{Scope: n.Scope, ObservedAt: now, Current: cur,
			Records: []record.NormRecord{{Key: n.Key, DataSubject: n.Subject, Audience: n.Audience, Fields: n.Fields, Hash: n.Hash}}})
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
		return resp, nil, err
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
