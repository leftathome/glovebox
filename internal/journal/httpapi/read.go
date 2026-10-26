package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// The reads (4.12). Each names exactly one reader and its own filters as
// query parameters, nothing else (queryParams). Authorization, in order:
//
//   - the service's policy (readerOK): reader is required (400); the service
//     must have read: true (state, events) or counts: true (counts), and the
//     reader must be in its readers -- "*" admits any id here, a list only
//     its members (policy.Service.MayRead / MayCount; 403 policy);
//   - the journal: the reader must be a member of the roster in use and not
//     erased (ErrUnknownReader, 403 policy), with a roster loaded
//     (ErrNoRoster, 503); it computes the reader's grants and filters (SQL
//     and Go) and masks every row. readErr maps its errors.
//
// The handlers pass the journal's results through as they are: never a
// row, value or event the journal did not return.

const (
	defaultEventLimit = 200
	maxEventLimit     = 1000 // below journal.MaxEventsLimit
	defaultCountsDays = 7
)

// optional is a filter that may be absent; present and empty is "" (the
// household's rows, for subject).
func optional(v url.Values, name string) *string {
	if vals, ok := v[name]; ok {
		return &vals[0]
	}
	return nil
}

// nonNil keeps a list [] on the wire, never null. The journal already
// returns non-nil lists; this holds the API's promise on its own.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// nextCursor is EventsResponse.Next: the last returned seq, or the
// request's after for an empty page. An empty page never moves the cursor,
// in either direction. NOTE (Task 19 review): a page whose every row the
// Go re-check drops comes back empty although later events exist; the
// cursor then stays where it is (fail closed: it never pages past events
// it did not deliver), and the stall is not silent -- every dropped row
// counts VisibilityMismatch, which raises GloveboxJournalVisibilityMismatch.
func nextCursor(after int64, events []journalapi.Event) int64 {
	if len(events) == 0 {
		return after
	}
	return events[len(events)-1].Seq
}

func badRequest(w http.ResponseWriter, detail string) {
	refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, detail)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, c call) {
	q, err := queryParams(r, "reader", "subject", "type", "scope", "include_removed")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	reader, ok := s.readerOK(w, q.Get("reader"), c.svc.MayRead(q.Get("reader")))
	if !ok {
		return
	}
	sq := journal.StateQuery{Reader: reader, Subject: optional(q, "subject"), Type: q.Get("type"), Scope: q.Get("scope")}
	if v, ok := q["include_removed"]; ok {
		if sq.IncludeRemoved, err = strconv.ParseBool(v[0]); err != nil {
			badRequest(w, "include_removed is true or false")
			return
		}
	}
	// NOTE (Task 19 review): the roster hash comes from the journal's
	// result, so it is that of the roster which filtered the rows.
	rows, sha, err := s.d.Journal.StateWithRoster(r.Context(), sq)
	if err != nil {
		s.readErr(w, "state", err)
		return
	}
	writeJSON(w, http.StatusOK, journalapi.StateResponse{Rows: nonNil(rows), RosterSHA256: sha})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, c call) {
	q, err := queryParams(r, "reader", "after", "subject", "type", "min_importance", "limit")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	reader, ok := s.readerOK(w, q.Get("reader"), c.svc.MayRead(q.Get("reader")))
	if !ok {
		return
	}
	eq := journal.EventsQuery{Reader: reader, Subject: optional(q, "subject"), Type: q.Get("type"),
		MinImportance: rules.Low, Limit: defaultEventLimit}
	if v, ok := q["after"]; ok {
		if eq.After, err = strconv.ParseInt(v[0], 10, 64); err != nil || eq.After < 0 {
			badRequest(w, "after is a sequence number >= 0")
			return
		}
	}
	if v, ok := q["min_importance"]; ok {
		if rules.ImportanceRank(v[0]) == 0 {
			badRequest(w, "min_importance is high, normal or low")
			return
		}
		eq.MinImportance = v[0]
	}
	if v, ok := q["limit"]; ok {
		if eq.Limit, err = strconv.Atoi(v[0]); err != nil || eq.Limit < 1 || eq.Limit > maxEventLimit {
			badRequest(w, "limit is 1 to 1000")
			return
		}
	}
	events, err := s.d.Journal.Events(r.Context(), eq)
	if err != nil {
		s.readErr(w, "events", err)
		return
	}
	writeJSON(w, http.StatusOK, journalapi.EventsResponse{Events: nonNil(events), Next: nextCursor(eq.After, events)})
}

func (s *Server) handleCounts(w http.ResponseWriter, r *http.Request, c call) {
	q, err := queryParams(r, "reader", "subject", "since")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	reader, ok := s.readerOK(w, q.Get("reader"), c.svc.MayCount(q.Get("reader")))
	if !ok {
		return
	}
	since := s.d.Now().Add(-defaultCountsDays * 24 * time.Hour)
	if v, ok := q["since"]; ok {
		if since, err = time.Parse(time.RFC3339, v[0]); err != nil {
			badRequest(w, "since is an RFC 3339 time")
			return
		}
	}
	counts, err := s.d.Journal.Counts(r.Context(), reader, optional(q, "subject"), since)
	if err != nil {
		s.readErr(w, "counts", err)
		return
	}
	writeJSON(w, http.StatusOK, journalapi.CountsResponse{Since: since, Counts: nonNil(counts)})
}

// handleHealthz is GET /v1/healthz: any authenticated caller; the roster
// hash (for the consumer's pre-write comparison, 4.9b) and the schema
// version. No roster loaded is an empty hash.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request, _ call) {
	if _, err := queryParams(r); err != nil {
		badRequest(w, err.Error())
		return
	}
	v, err := s.d.Journal.SchemaVersion(r.Context())
	if err != nil {
		s.internal(w, "healthz", err)
		return
	}
	sha := ""
	if ros := s.d.Roster(); ros != nil {
		sha = ros.SHA256()
	}
	writeJSON(w, http.StatusOK, journalapi.Healthz{Status: journalapi.HealthOK, RosterSHA256: sha, SchemaVersion: v})
}
