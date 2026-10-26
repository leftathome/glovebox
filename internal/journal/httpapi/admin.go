package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// The operator endpoints (4.12, 4.13, 4.5, 4.11): confirm or discard a held
// snapshot, erase a member, start a rescan. Only a service whose policy has
// admin: true reaches them; nothing else in a policy grants it, and admin
// grants nothing else. Order on each: operator (403 policy, logged as a
// warning), no query string (400), the request's own checks, then the
// journal. Every request past the operator check logs one audit line
// (auditAdmin).

// adminEnvelopeBytes is the slack of an admin body cap for everything but
// its values: braces, colons, commas and whitespace. A held decision takes
// no body ({} at most), so this is its whole cap.
const adminEnvelopeBytes = 256

// adminKeyMargin is the slack of the admin key caps. A key the request does
// not declare is refused anyway (strict decoding); the margin only decides
// where such a body is a 400 rather than a 413.
const adminKeyMargin = 2

// bodyCap caps one admin request's body.
type bodyCap struct {
	keys  strictjson.Limits
	bytes int64
}

// adminBodyCap derives a body cap from the request type v (a struct of
// string and bool fields with json names): per object and per document,
// its field count plus adminKeyMargin; in bytes, every key escaped, every
// string at the widest scope value (a member id the journal can hold) with
// every byte escaped, every bool as "false", plus adminEnvelopeBytes. The
// erase body (two ids) is 3,454 bytes, the rescan body 283. A field of any
// other type panics: give it a width here first.
func adminBodyCap(v any) bodyCap {
	t := reflect.TypeOf(v)
	n, size := 0, adminEnvelopeBytes
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		n++
		size += escapedBytes*len(name) + 4
		switch f.Type.Kind() {
		case reflect.String:
			size += escapedBytes*record.MaxScopeValueBytes + 2
		case reflect.Bool:
			size += len("false")
		default:
			panic("httpapi: no admin body width for a field of type " + f.Type.String())
		}
	}
	return bodyCap{keys: strictjson.Limits{MaxObjectKeys: n + adminKeyMargin, MaxTotalKeys: n + adminKeyMargin}, bytes: int64(size)}
}

var (
	heldCap   = adminBodyCap(struct{}{})
	eraseCap  = adminBodyCap(journalapi.EraseRequest{})
	rescanCap = adminBodyCap(journalapi.RescanRequest{})
)

// admin is the operator check: the caller's policy has admin: true. A
// refusal is logged with the caller and the route, nothing from the request.
func (s *Server) admin(w http.ResponseWriter, c call, endpoint string) bool {
	if !c.svc.Admin {
		s.d.Log.Warn("journal admin refused", "caller", c.caller, "endpoint", endpoint)
		refuse(w, http.StatusForbidden, journalapi.ReasonPolicy, "this service is not an operator")
		return false
	}
	return true
}

// auditAdmin logs one info line per operator request, when the answer is
// written: the caller, the route name, what the request named (attrs) and
// the HTTP status. attrs carry only parsed, well-formed ids or fixed
// placeholders, never caller text.
func (s *Server) auditAdmin(w http.ResponseWriter, c call, endpoint string, attrs ...any) {
	code := 0
	if sw, ok := w.(*statusWriter); ok {
		code = sw.status
	}
	args := append([]any{"caller", c.caller, "endpoint", endpoint}, attrs...)
	s.d.Log.Info("journal admin", append(args, "code", code)...)
}

// adminBody reads and strictly decodes an admin body into into, under cap.
// An empty body (whitespace only) leaves into as it is, or is refused when
// required. A non-empty body must be application/json. Over the byte cap or
// a key cap is 413; anything else that does not decode is 400 with shape as
// the detail (a fixed text: no key or value is echoed). It writes the
// refusal itself.
func (s *Server) adminBody(w http.ResponseWriter, r *http.Request, cap bodyCap, into any, required bool, shape string) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cap.bytes))
	if err != nil {
		w.Header().Set("Connection", "close")
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge, "the body exceeds what this request takes")
			return false
		}
		s.d.Log.Warn("journal request body could not be read", "err", err)
		refuse(w, http.StatusInternalServerError, journalapi.ReasonInternal, "")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if required {
			badRequest(w, shape)
			return false
		}
		return true
	}
	if !isJSON(r) {
		badRequest(w, "Content-Type must be application/json")
		return false
	}
	if err := strictjson.DecodeLimits(body, into, cap.keys); err != nil {
		if errors.Is(err, strictjson.ErrTooManyKeys) {
			refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge, "the body has more keys than this request takes")
			return false
		}
		badRequest(w, shape)
		return false
	}
	return true
}

// handleHeld is POST /v1/admin/held/<snapshot_id>:confirm|discard (4.5).
// The id must be a UUID and the action one of the two, checked before the
// database. The body is empty (or {}). Confirm re-checks the snapshot's
// writer under today's policy (writerMayWrite) and re-runs 4.4 from step 4
// (ConfirmHeld); its refusals (403 writer, 410 ban, 409 roster, 422 schema)
// come back as they are, and the snapshot stays held. Unknown is 404, no
// longer held is 409 conflict.
func (s *Server) handleHeld(w http.ResponseWriter, r *http.Request, c call) {
	const endpoint = "admin_held"
	if !s.admin(w, c, endpoint) {
		return
	}
	snapshot, action := "(not a snapshot id)", "(not an action)"
	defer func() { s.auditAdmin(w, c, endpoint, "snapshot_id", snapshot, "action", action) }()
	if _, err := queryParams(r); err != nil {
		badRequest(w, "POST /v1/admin/held takes no query parameters")
		return
	}
	idText, act, _ := strings.Cut(r.PathValue("target"), ":")
	id, err := uuid.Parse(idText)
	if err != nil || (act != journalapi.ActionConfirm && act != journalapi.ActionDiscard) {
		badRequest(w, "the path is /v1/admin/held/<snapshot_id>:confirm or :discard")
		return
	}
	snapshot, action = id.String(), act
	if !s.adminBody(w, r, heldCap, &struct{}{}, false, "a held decision takes no body") {
		return
	}
	var out journal.Outcome
	if action == journalapi.ActionConfirm {
		out, err = s.d.Journal.ConfirmHeld(r.Context(), snapshot, s.writerMayWrite)
	} else {
		err = s.d.Journal.DiscardHeld(r.Context(), snapshot)
	}
	switch {
	case errors.Is(err, journal.ErrNotFound):
		refuse(w, http.StatusNotFound, journalapi.ReasonNotFound, "no such snapshot")
	case errors.Is(err, journal.ErrNotHeld):
		refuse(w, http.StatusConflict, journalapi.ReasonConflict, "the snapshot is not held")
	case err != nil:
		s.internal(w, "held", err)
	case out.Rejection != nil:
		reject(w, out.Rejection)
	case action == journalapi.ActionDiscard:
		writeJSON(w, http.StatusOK, journalapi.Response{Status: journalapi.StatusDiscarded})
	default:
		writeJSON(w, http.StatusOK, journalapi.Response{Status: out.Status, Stats: &out.Stats})
	}
}

// writerMayWrite re-checks a held snapshot's writer under the current
// policy before an operator's confirmation applies it (Task 17's
// journal.WriteCheck). The details name no type, scope or writer.
func (s *Server) writerMayWrite(writer string, t record.Type, sc record.Scope) *record.Rejection {
	svc, ok := s.d.Policy.Service(writer)
	if !ok {
		return &record.Rejection{Status: http.StatusForbidden, Reason: journalapi.ReasonPolicy,
			Detail: "the snapshot's writer no longer has a journal policy; discard the snapshot"}
	}
	if rej := svc.MayWrite(t, sc); rej != nil {
		return &record.Rejection{Status: http.StatusForbidden, Reason: journalapi.ReasonPolicy,
			Detail: "the snapshot's writer may no longer write it (" + rej.Detail + "); discard the snapshot"}
	}
	return nil
}

// handleErase is POST /v1/admin/erase (4.13) with
// {"data_subject": "<member id>", "confirm": "<the same id>"}. An erase
// cannot be undone and is a ban, so the id must be a member id
// (roster.ValidID: never "" or a kept note's pseudonym) and confirm must
// repeat it exactly; otherwise 400 and nothing is erased. The member may
// still be on the roster. No roster loaded is 503. The audit line names the
// erased member's id: the operator needs it, and the erasure row keeps it
// anyway. Erase reports no metric (Task 20).
func (s *Server) handleErase(w http.ResponseWriter, r *http.Request, c call) {
	const endpoint = "admin_erase"
	if !s.admin(w, c, endpoint) {
		return
	}
	subject := ""
	defer func() { s.auditAdmin(w, c, endpoint, "data_subject", loggedMember(subject)) }()
	if _, err := queryParams(r); err != nil {
		badRequest(w, "POST /v1/admin/erase takes no query parameters")
		return
	}
	const shape = `the body is {"data_subject": "<member id>", "confirm": "<the same member id>"}`
	var req journalapi.EraseRequest
	if !s.adminBody(w, r, eraseCap, &req, true, shape) {
		return
	}
	subject = req.DataSubject
	if !roster.ValidID(req.DataSubject) {
		badRequest(w, "data_subject is not a member id")
		return
	}
	if req.Confirm != req.DataSubject {
		badRequest(w, "an erase cannot be undone: confirm must repeat data_subject exactly")
		return
	}
	resp, err := s.d.Journal.Erase(r.Context(), c.caller, req.DataSubject)
	if err != nil {
		s.eraseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// loggedMember is what the audit log names for a requested member: the id
// if it is shaped like one, else a placeholder.
func loggedMember(id string) string {
	if !roster.ValidID(id) {
		return "(not a member id)"
	}
	return id
}

// eraseErr maps an Erase error: ErrNoSubject is 400 (the handler refuses
// such an id first), ErrNoRoster 503, anything else 500.
func (s *Server) eraseErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, journal.ErrNoSubject):
		badRequest(w, "data_subject is not a member id")
	case errors.Is(err, journal.ErrNoRoster):
		refuse(w, http.StatusServiceUnavailable, journalapi.ReasonUnavailable, "no roster is loaded")
	default:
		s.internal(w, "erase", err)
	}
}

// handleRescan is POST /v1/admin/rescan (4.11) with an optional
// {"all": true}. A rescan can take minutes, so it runs in the background
// and the answer is 202 started; its progress and result are in the log.
// One at a time: while one runs (s.rescanning), another is 409 conflict and
// does not start. The rescan runs under Deps.Context (the process's
// lifetime), never the request's: the request timeout, which ends the
// request's context as soon as the 202 is written, and a client that goes
// away do not cancel it, and it holds no in-flight slot once answered (the
// guard bounds it to one). Shutting down cancels Deps.Context, which ends it
// at the next batch; committed batches stay, and a later run continues.
// Rescan reports no metric (Task 21).
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request, c call) {
	const endpoint = "admin_rescan"
	if !s.admin(w, c, endpoint) {
		return
	}
	var req journalapi.RescanRequest
	defer func() { s.auditAdmin(w, c, endpoint, "all", req.All) }()
	if _, err := queryParams(r); err != nil {
		badRequest(w, "POST /v1/admin/rescan takes no query parameters")
		return
	}
	if !s.adminBody(w, r, rescanCap, &req, false, `the body is empty or {"all": true}`) {
		return
	}
	if !s.rescanning.CompareAndSwap(false, true) {
		refuse(w, http.StatusConflict, journalapi.ReasonConflict, "a rescan is already running")
		return
	}
	s.d.Log.Info("journal rescan started", "caller", c.caller, "all", req.All)
	go s.runRescan(s.d.Context, c.caller, req.All)
	writeJSON(w, http.StatusAccepted, journalapi.Response{Status: journalapi.StatusStarted})
}

// runRescan runs one rescan under ctx (Deps.Context, never a request's)
// and frees the guard after.
func (s *Server) runRescan(ctx context.Context, caller string, all bool) {
	defer s.rescanning.Store(false)
	res, err := s.d.Journal.Rescan(ctx, all)
	if err != nil {
		s.d.Log.Error("journal rescan failed", "caller", caller, "err", err, "current_rows", res.CurrentRows,
			"event_rows", res.EventRows, "released", res.Released, "withheld", res.Withheld)
		return
	}
	s.d.Log.Info("journal rescan finished", "caller", caller, "current_rows", res.CurrentRows,
		"event_rows", res.EventRows, "released", res.Released, "withheld", res.Withheld)
}
