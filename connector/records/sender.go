package records

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Sender drains the spool: one snapshot at a time (the journal allows 16
// requests in flight for everyone, A14), oldest first within a scope, and
// acts on every answer by its reason (Classify):
//
//	ActDelete      delete the file; continue
//	ActReject      move it to rejected/; continue with the scope's next file
//	ActErased      delete it and purge the subject's other files; leave the scope
//	ActRetryScope  keep it; block this scope until its own backoff ends
//	ActRetry       keep it; stop the pass; global backoff (at least Retry-After)
//	ActRetryAlert  as ActRetry, and log an error (alerted through answers_total)
//
// A token file that cannot be read or is empty sends nothing and is
// ActRetryAlert with code "token": a deployment fault, as a 401 is.
//
// Backoff is exponential from min_backoff to max_backoff with +/-20%
// jitter, reset by any accepted or rejected file. It lives in memory only:
// a restart sends at once, which is safe because every send is idempotent
// (the file is resent byte for byte, A9).
//
// Log lines carry the record type, the scope prefix (source and surface),
// the code and the reason; never a scope value, subject id, key, field
// value or file name.
type Sender struct {
	spool  *Spool
	client *Client
	m      *Metrics
	cfg    SpoolConfig
	now    func() time.Time
	jitter func() float64 // in [-0.2, 0.2]
	log    *slog.Logger

	globalUntil time.Time
	globalStep  int
	scopes      map[string]scopeBackoff
}

type scopeBackoff struct {
	until time.Time
	step  int
}

// PassResult says what a pass did and when the next one is due (zero:
// whenever there is new work).
type PassResult struct {
	Sent      int
	NextDue   time.Time
	Remaining int
}

// NewSender builds a sender. now and log may be nil.
func NewSender(spool *Spool, client *Client, m *Metrics, cfg SpoolConfig, log *slog.Logger, now func() time.Time) *Sender {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sender{spool: spool, client: client, m: m, cfg: cfg, now: now, log: log,
		jitter: func() float64 { return rand.Float64()*0.4 - 0.2 }, scopes: map[string]scopeBackoff{}}
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// Drain makes one pass over the spool. The bounds are applied first, even
// during a global backoff, so max_age holds while the journal is away.
// A backoff runs from the moment of its answer, not the pass's start.
func (s *Sender) Drain(ctx context.Context) PassResult {
	if err := s.spool.Enforce(); err != nil {
		s.log.Warn("records spool bounds could not be applied", "error", errText(err))
	}
	now := s.now()
	if now.Before(s.globalUntil) {
		return PassResult{NextDue: s.globalUntil, Remaining: s.remaining()}
	}
	pending, err := s.spool.Pending()
	if err != nil {
		s.log.Warn("records spool could not be listed", "error", errText(err))
		return PassResult{NextDue: now.Add(s.cfg.MinBackoff())}
	}
	order, groups := groupByScope(pending)
	res := PassResult{}
	erased := map[string]bool{}
scopes:
	for _, key := range order {
		if b, ok := s.scopes[key]; ok && now.Before(b.until) {
			res.NextDue = earliest(res.NextDue, b.until)
			continue
		}
		for _, e := range groups[key] {
			if ctx.Err() != nil {
				break scopes
			}
			if e.DataSubject != "" && erased[e.DataSubject] {
				continue scopes
			}
			body, err := s.spool.Body(e)
			if err != nil {
				continue // the file went away (bounds, purge); nothing to do
			}
			a, err := s.client.Send(ctx, body)
			code := codeLabel(a.Code)
			if err != nil {
				if ctx.Err() != nil {
					break scopes // shutting down: not the journal's answer
				}
				a, code = Answer{}, codeLabel(0) // transport
				if errors.Is(err, ErrNoToken) {
					code = codeToken
				}
			}
			act := Classify(a.Code, a.Status, a.Reason)
			if code == codeToken {
				act = ActRetryAlert
			}
			s.m.answer(code, a.Reason, act)
			switch act {
			case ActDelete:
				if err := s.spool.Delete(e); err != nil {
					s.log.Warn("records spool file could not be deleted after the journal accepted it",
						"record_type", e.RecordType, "scope", e.ScopePrefix, "error", errText(err))
				}
				res.Sent++
				s.globalStep = 0
				delete(s.scopes, key)
				s.spool.MarkUnknown(key, false)
				continue
			case ActReject:
				if err := s.spool.Reject(e, ReasonLabel(a.Reason)); err != nil {
					s.log.Warn("records spool file could not be moved to rejected/",
						"record_type", e.RecordType, "scope", e.ScopePrefix, "error", errText(err))
				}
				s.globalStep = 0
				delete(s.scopes, key)
				s.spool.MarkUnknown(key, false)
				s.log.Error("records snapshot refused by the journal; moved to rejected/",
					"record_type", e.RecordType, "scope", e.ScopePrefix, "code", code, "reason", ReasonLabel(a.Reason))
				continue
			case ActErased:
				// The purge deletes this file too (it is about the subject).
				n, perr := s.spool.PurgeSubject(e.DataSubject)
				if e.DataSubject == "" {
					if perr = s.spool.Delete(e); perr == nil {
						n = 1
						s.m.droppedN(DropErasedSubject, 1)
					}
				}
				if perr != nil {
					s.log.Warn("records spool files of an erased subject could not all be deleted", "error", errText(perr))
				}
				erased[e.DataSubject] = true
				s.log.Warn("records subject erased in the journal; its spooled snapshots were deleted",
					"record_type", e.RecordType, "scope", e.ScopePrefix, "reason", ReasonLabel(a.Reason), "files", n)
			case ActRetryScope:
				b := s.scopes[key]
				b.step++
				b.until = s.now().Add(s.backoff(b.step, a.RetryAfter))
				s.scopes[key] = b
				s.spool.MarkUnknown(key, true)
				res.NextDue = earliest(res.NextDue, b.until)
				s.log.Info("records scope blocked: its subject is not in the journal's roster",
					"record_type", e.RecordType, "scope", e.ScopePrefix, "reason", ReasonLabel(a.Reason))
			default: // ActRetry, ActRetryAlert
				s.globalStep++
				s.globalUntil = s.now().Add(s.backoff(s.globalStep, a.RetryAfter))
				if act == ActRetryAlert {
					s.log.Error("records journal refused the connector for a reason that is not the data's; keeping the spool",
						"record_type", e.RecordType, "scope", e.ScopePrefix, "code", code, "reason", ReasonLabel(a.Reason))
				} else {
					s.log.Warn("records journal unavailable; keeping the spool",
						"code", code, "reason", ReasonLabel(a.Reason))
				}
				res.NextDue = s.globalUntil
				res.Remaining = s.remaining()
				return res
			}
			continue scopes // ActErased, ActRetryScope: leave this scope
		}
	}
	res.Remaining = s.remaining()
	return res
}

// FailPass backs off as after a transport failure (the loop calls it when
// a pass panicked).
func (s *Sender) FailPass() {
	s.globalStep++
	s.globalUntil = s.now().Add(s.backoff(s.globalStep, 0))
}

func (s *Sender) remaining() int {
	es, err := s.spool.Pending()
	if err != nil {
		return 0
	}
	return len(es)
}

// backoff is min_backoff * 2^(step-1), capped at max_backoff, with jitter,
// and never shorter than retryAfter.
func (s *Sender) backoff(step int, retryAfter time.Duration) time.Duration {
	d := s.cfg.MinBackoff() << min(max(step-1, 0), 20)
	if d > s.cfg.MaxBackoff() || d <= 0 {
		d = s.cfg.MaxBackoff()
	}
	d = time.Duration(float64(d) * (1 + s.jitter()))
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// groupByScope groups entries (oldest first) by scope (ScopeKey: record
// type and scope parts); scopes are ordered by their oldest file.
func groupByScope(es []Entry) ([]string, map[string][]Entry) {
	var order []string
	groups := map[string][]Entry{}
	for _, e := range es {
		if _, ok := groups[e.ScopeKey]; !ok {
			order = append(order, e.ScopeKey)
		}
		groups[e.ScopeKey] = append(groups[e.ScopeKey], e)
	}
	return order, groups
}
