package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/leftathome/glovebox/internal/journal/access"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// ErrUnknownReader is returned by a read for a reader who is not a member of
// the roster in use (or when no roster is loaded). Such a reader has no
// grants, and "public" rows are visible only to roster members, so the read
// returns no rows at all. The request layer answers it with 403.
var ErrUnknownReader = errors.New("journal: reader is not in the roster")

// ErrBadQuery is returned for a query the journal cannot run: a limit below
// 1, a negative cursor or an unknown importance. The request layer validates
// these first; the journal refuses them anyway.
var ErrBadQuery = errors.New("journal: bad query")

// grantsOf computes a reader's grants from the roster in use. The roster is
// the only source: the journal confirms membership here, before any query.
func (j *Journal) grantsOf(reader string) ([]roster.Grant, error) {
	g, ok := j.o.Roster().Grants(reader) // safe on a nil roster
	if !ok {
		return nil, ErrUnknownReader
	}
	return g, nil
}

// rowVisible is the Go half of the visibility rule, run on every row the SQL
// half returned. The journal field exists so a test can make the two
// disagree.
func (j *Journal) rowVisible(grants []roster.Grant, subject string, audience []string) bool {
	if j.visible != nil {
		return j.visible(grants, subject, audience)
	}
	return access.Visible(grants, subject, audience)
}

// visibleSQL is the visibility rule over table alias a, with the grants as
// $1 (subjects) and $2 (tokens). The GIN index on audience serves the
// overlap test; the EXISTS is the rule itself.
func visibleSQL(a string) string {
	return fmt.Sprintf(`('public' = ANY(%[1]s.audience) OR EXISTS (
		SELECT 1 FROM unnest($1::text[], $2::text[]) AS g(s, t)
		WHERE g.s = %[1]s.data_subject AND g.t = ANY(%[1]s.audience)))`, a)
}

func nullable(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// kinds tells which fields of a record type hold untrusted text. A type the
// registry no longer knows has no field kinds, so every field counts as text
// and only a passing verdict releases it (fail closed).
type kinds struct {
	t     record.Type
	known bool
}

func (j *Journal) kindsOf(recordType string) kinds {
	t, ok := j.o.Registry.Active(recordType)
	return kinds{t: t, known: ok}
}

func (k kinds) isText(field string) bool { return !k.known || k.t.IsText(field) }

// StateQuery selects GET /v1/state rows. A nil Subject means every subject;
// empty Type and Scope mean all. Reader is a roster member id; the grants
// are computed from the roster, never supplied.
type StateQuery struct {
	Reader         string
	Subject        *string
	Type, Scope    string
	IncludeRemoved bool
}

// State returns the visible record_current rows. The result is never nil.
func (j *Journal) State(ctx context.Context, q StateQuery) ([]journalapi.StateRow, error) {
	out := []journalapi.StateRow{}
	grants, err := j.grantsOf(q.Reader)
	if err != nil {
		return out, err
	}
	subs, toks := access.Pairs(grants)
	rows, err := j.db.QueryContext(ctx, `SELECT c.record_type, c.data_subject, c.key, c.scope, to_jsonb(c.audience),
		c.fields, c.removed, c.first_seen_at, c.last_seen_at, c.last_confirmed_at, c.scan,
		CASE WHEN c.last_confirmed_snapshot IS NULL THEN NULL
		     ELSE NOT c.removed AND c.last_confirmed_snapshot = (
		          SELECT s.id FROM snapshot s
		          WHERE s.record_type = c.record_type AND s.scope = c.scope AND s.status = 'applied'
		          ORDER BY s.observed_at DESC, s.received_at DESC LIMIT 1) END,
		coalesce((SELECT e.approximate FROM record_event e WHERE e.seq = c.last_event_seq), false)
		FROM record_current c
		WHERE `+visibleSQL("c")+`
		  AND ($3::text IS NULL OR c.data_subject = $3) AND ($4 = '' OR c.record_type = $4)
		  AND ($5 = '' OR c.scope = $5) AND ($6 OR NOT c.removed)
		ORDER BY c.record_type, c.data_subject, c.key`,
		subs, toks, nullable(q.Subject), q.Type, q.Scope, q.IncludeRemoved)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var s journalapi.StateRow
		var aud, fields, scan []byte
		var inLatest sql.NullBool
		if err := rows.Scan(&s.RecordType, &s.DataSubject, &s.Key, &s.Scope, &aud, &fields, &s.Removed,
			&s.FirstSeenAt, &s.LastSeenAt, &s.ConfirmedAt, &scan, &inLatest, &s.Approximate); err != nil {
			return []journalapi.StateRow{}, err
		}
		if err := json.Unmarshal(aud, &s.Audience); err != nil {
			return []journalapi.StateRow{}, err
		}
		if !j.rowVisible(grants, s.DataSubject, s.Audience) {
			j.o.Metrics.VisibilityMismatch()
			continue
		}
		k := j.kindsOf(s.RecordType)
		var f map[string]json.RawMessage
		var sm record.ScanMap
		if err := json.Unmarshal(fields, &f); err != nil {
			return []journalapi.StateRow{}, err
		}
		if err := json.Unmarshal(scan, &sm); err != nil {
			return []journalapi.StateRow{}, fmt.Errorf("record %s/%s: scan: %w", s.DataSubject, s.Key, err)
		}
		s.Fields = map[string]json.RawMessage{}
		for name, v := range f {
			// Fail closed (4.11): a value with a verdict is masked by it,
			// whatever the active type now says; a text value with none is
			// withheld.
			if _, scanned := sm[name]; scanned || k.isText(name) {
				v = access.Text(v, sm[name].Verdict)
			}
			s.Fields[name] = v
		}
		if inLatest.Valid {
			b := inLatest.Bool
			s.InLatestSnapshot = &b
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return []journalapi.StateRow{}, err
	}
	return out, nil
}

// EventsQuery selects GET /v1/events.
type EventsQuery struct {
	Reader        string
	After         int64
	Subject       *string
	Type          string
	MinImportance string // "" means low
	Limit         int    // at least 1
}

// Events returns visible events with seq > After, ascending, at most Limit.
// The result is never nil.
func (j *Journal) Events(ctx context.Context, q EventsQuery) ([]journalapi.Event, error) {
	out := []journalapi.Event{}
	grants, err := j.grantsOf(q.Reader)
	if err != nil {
		return out, err
	}
	min := q.MinImportance
	if min == "" {
		min = rules.Low
	}
	if q.Limit < 1 || q.After < 0 || rules.ImportanceRank(min) == 0 {
		return out, ErrBadQuery
	}
	subs, toks := access.Pairs(grants)
	rows, err := j.db.QueryContext(ctx, `SELECT e.seq, e.record_type, e.data_subject, e.key, e.kind, e.schema_version,
		e.scope, coalesce(e.prev_scope, ''), to_jsonb(e.audience), to_jsonb(e.prev_audience), e.fields, e.changed,
		e.scan, e.observed_at, e.prev_observed_at, e.approximate, e.actor, e.importance, coalesce(e.rule, ''),
		e.notify, coalesce(e.group_key, '')
		FROM record_event e
		WHERE e.seq > $3 AND `+visibleSQL("e")+`
		  AND ($4::text IS NULL OR e.data_subject = $4) AND ($5 = '' OR e.record_type = $5)
		  AND e.importance = ANY($6)
		ORDER BY e.seq LIMIT $7`,
		subs, toks, q.After, nullable(q.Subject), q.Type, rules.AtLeast(min), q.Limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e journalapi.Event
		var aud, prevAud, fields, changed, scan, notify []byte
		var prevObs sql.NullTime
		if err := rows.Scan(&e.Seq, &e.RecordType, &e.DataSubject, &e.Key, &e.Kind, &e.SchemaVersion, &e.Scope,
			&e.PrevScope, &aud, &prevAud, &fields, &changed, &scan, &e.ObservedAt, &prevObs, &e.Approximate,
			&e.Actor, &e.Importance, &e.Rule, &notify, &e.GroupKey); err != nil {
			return []journalapi.Event{}, err
		}
		if err := json.Unmarshal(aud, &e.Audience); err != nil {
			return []journalapi.Event{}, err
		}
		if !j.rowVisible(grants, e.DataSubject, e.Audience) {
			j.o.Metrics.VisibilityMismatch()
			continue
		}
		// prev_audience NULL means the audience did not change. A value that is
		// present but does not decode must never release an old value, so it
		// is an error, not "unchanged".
		audienceChanged := prevAud != nil
		var prevAudience []string
		if audienceChanged {
			if err := json.Unmarshal(prevAud, &prevAudience); err != nil {
				return []journalapi.Event{}, fmt.Errorf("event %d: prev_audience: %w", e.Seq, err)
			}
		}
		if prevObs.Valid {
			t := prevObs.Time
			e.PrevObservedAt = &t
		}
		if err := j.fillEventValues(&e, grants, prevAudience, audienceChanged, fields, changed, scan); err != nil {
			return []journalapi.Event{}, fmt.Errorf("event %d: %w", e.Seq, err)
		}
		if notify != nil {
			var ns []rules.Notice
			if err := json.Unmarshal(notify, &ns); err != nil {
				return []journalapi.Event{}, fmt.Errorf("event %d: notify: %w", e.Seq, err)
			}
			// rules.Notice is journalapi.Notice, so the filtered list is the wire form.
			e.Notify = access.Notices(grants, e.DataSubject, ns)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return []journalapi.Event{}, err
	}
	return out, nil
}

// fillEventValues decodes, renames forward and masks an event's values.
func (j *Journal) fillEventValues(e *journalapi.Event, grants []roster.Grant, prevAudience []string, audienceChanged bool, fields, changed, scan []byte) error {
	k := j.kindsOf(e.RecordType)
	name := func(f string) string { return j.o.Registry.ForwardName(e.RecordType, e.SchemaVersion, f) }
	var es map[string]record.EventScan
	if err := json.Unmarshal(scan, &es); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	verdicts := map[string]record.EventScan{}
	for f, v := range es {
		verdicts[name(f)] = v
	}
	mask := func(f string, v json.RawMessage, verdict string) json.RawMessage {
		if _, scanned := verdicts[f]; scanned || k.isText(f) {
			return access.Text(v, verdict)
		}
		return v
	}
	switch e.Kind {
	case journalapi.KindCreated, journalapi.KindRestored:
		var f map[string]json.RawMessage
		if err := json.Unmarshal(fields, &f); err != nil {
			return err
		}
		e.Fields = map[string]json.RawMessage{}
		for fn, v := range f {
			n := name(fn)
			e.Fields[n] = mask(n, v, verdicts[n].New)
		}
	case journalapi.KindUpdated:
		var ch map[string][2]json.RawMessage
		if err := json.Unmarshal(changed, &ch); err != nil {
			return err
		}
		e.Changed = map[string]journalapi.Change{}
		for fn, pair := range ch {
			n := name(fn)
			old, nw := nilIfNull(pair[0]), nilIfNull(pair[1])
			old = mask(n, old, verdicts[n].Old)
			old = access.OldValue(grants, e.DataSubject, prevAudience, audienceChanged, old)
			e.Changed[n] = journalapi.Change{Old: old, New: mask(n, nw, verdicts[n].New)}
		}
	case journalapi.KindRemoved:
		var f map[string]json.RawMessage
		if err := json.Unmarshal(changed, &f); err != nil {
			return err
		}
		e.LastFields = map[string]json.RawMessage{}
		for fn, v := range f {
			n := name(fn)
			e.LastFields[n] = mask(n, v, verdicts[n].Old)
		}
	default:
		return fmt.Errorf("unknown event kind %q", e.Kind)
	}
	return nil
}

func nilIfNull(v json.RawMessage) json.RawMessage {
	if v == nil || string(v) == "null" {
		return nil
	}
	return v
}

// Counts returns live records and events by importance since since, per
// subject and record type, for the visible rows only (4.12). No values. The
// result is never nil.
func (j *Journal) Counts(ctx context.Context, reader string, subject *string, since time.Time) ([]journalapi.Count, error) {
	grants, err := j.grantsOf(reader)
	if err != nil {
		return []journalapi.Count{}, err
	}
	subs, toks := access.Pairs(grants)
	type key struct{ s, t string }
	acc := map[key]*journalapi.Count{}
	get := func(s, t string) *journalapi.Count {
		k := key{s, t}
		if acc[k] == nil {
			acc[k] = &journalapi.Count{DataSubject: s, RecordType: t, Events: map[string]int{}}
		}
		return acc[k]
	}
	live, err := j.db.QueryContext(ctx, `SELECT c.data_subject, c.record_type, count(*) FROM record_current c
		WHERE NOT c.removed AND `+visibleSQL("c")+` AND ($3::text IS NULL OR c.data_subject = $3)
		GROUP BY 1, 2`, subs, toks, nullable(subject))
	if err != nil {
		return []journalapi.Count{}, err
	}
	for live.Next() {
		var s, t string
		var n int
		if err := live.Scan(&s, &t, &n); err != nil {
			live.Close()
			return []journalapi.Count{}, err
		}
		get(s, t).Live = n
	}
	err = live.Err()
	live.Close()
	if err != nil {
		return []journalapi.Count{}, err
	}
	evs, err := j.db.QueryContext(ctx, `SELECT e.data_subject, e.record_type, e.importance, count(*) FROM record_event e
		WHERE e.observed_at >= $4 AND `+visibleSQL("e")+` AND ($3::text IS NULL OR e.data_subject = $3)
		GROUP BY 1, 2, 3`, subs, toks, nullable(subject), since)
	if err != nil {
		return []journalapi.Count{}, err
	}
	defer evs.Close()
	for evs.Next() {
		var s, t, imp string
		var n int
		if err := evs.Scan(&s, &t, &imp, &n); err != nil {
			return []journalapi.Count{}, err
		}
		get(s, t).Events[imp] = n
	}
	if err := evs.Err(); err != nil {
		return []journalapi.Count{}, err
	}
	out := make([]journalapi.Count, 0, len(acc))
	for _, c := range acc {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].DataSubject != out[k].DataSubject {
			return out[i].DataSubject < out[k].DataSubject
		}
		return out[i].RecordType < out[k].RecordType
	})
	return out, nil
}
