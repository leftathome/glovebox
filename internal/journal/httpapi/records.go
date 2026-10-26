package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

const maxSnapshotBytes = 8 << 20 // 4.3

// handleRecords is POST /v1/records. Order: the service writes snapshots at
// all (403 policy), Content-Type (400), body at most 8 MiB (413), strict
// decoding (422 schema), 4.4 step 1 (a resend is answered from its stored
// outcome; a reused id is 422), validation (4xx), policy (403), Apply (200
// statuses, 409, 410, 422 reuse).
func (s *Server) handleRecords(w http.ResponseWriter, r *http.Request, c call) {
	// Before anything else, the step-1 lookup included: a service that
	// writes no snapshots learns nothing here, not even whether an id exists.
	if !c.svc.WritesSnapshots(s.d.Registry) {
		refuse(w, http.StatusForbidden, journalapi.ReasonPolicy, "this service writes no snapshots")
		return
	}
	if !isJSON(r) {
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSnapshotBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge, "the body exceeds 8 MiB")
			return
		}
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, "the body could not be read")
		return
	}
	// Strict: unknown, duplicate and case-variant keys and trailing data are
	// refused, so the stored payload means exactly what was validated.
	var snap journalapi.Snapshot
	if err := strictjson.Decode(body, &snap); err != nil {
		refuse(w, http.StatusUnprocessableEntity, journalapi.ReasonSchema, "the body is not a snapshot")
		return
	}
	// 4.4 step 1 before anything else: a resend is answered as it was,
	// even if the configuration changed since it was first sent.
	// NOTE (Task 15 review): an id is a duplicate only for the same caller,
	// record type and canonical scope; without a parseable scope there is
	// nothing to compare, so the lookup is skipped and validation refuses it.
	if id, err := uuid.Parse(snap.SnapshotID); err == nil {
		if sc, rej := record.ParseScope(snap.Scope); rej == nil {
			out, found, err := s.d.Journal.Lookup(r.Context(), id.String(), c.caller, snap.RecordType, sc.Canonical())
			if err != nil {
				s.internal(w, "lookup", err)
				return
			}
			if found {
				if out.Rejection != nil { // snapshot_id_reused: 422
					reject(w, out.Rejection)
					return
				}
				// A resend, held or not, is a duplicate: "held" counts once.
				// The label comes from the registry, never from the body.
				if t, ok := s.d.Registry.Active(snap.RecordType); ok {
					s.d.Metrics.Snapshot(t.Name, journalapi.StatusDuplicate)
				}
				writeJSON(w, http.StatusOK, journalapi.Response{Status: out.Status, Stats: &out.Stats})
				return
			}
		}
	}
	n, rej := record.ValidateSnapshot(s.d.Registry, snap, s.d.Now())
	if rej != nil {
		reject(w, rej)
		return
	}
	if rej := c.svc.MayWrite(n.Type, n.Scope); rej != nil {
		reject(w, rej)
		return
	}
	out, err := s.d.Journal.Apply(r.Context(), c.caller, n, body)
	if err != nil {
		s.internal(w, "apply", err)
		return
	}
	if out.Rejection != nil {
		reject(w, out.Rejection)
		return
	}
	writeJSON(w, http.StatusOK, journalapi.Response{Status: out.Status, Stats: &out.Stats})
}
