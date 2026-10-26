package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// maxNoteBytes bounds a POST /v1/notes body: 512 KiB, a sixteenth of a
// snapshot's 8 MiB. A note is one record. The widest legitimate one in the
// registry today (attendance_note: one text field of at most 64 KiB, two
// strings of at most 1 KiB, a date) is about 405 KiB even with every byte
// \u-escaped (6 bytes each), the worst a JSON encoder can do with the
// characters validation admits; TestWidestNoteFitsTheByteCap pins that. A
// household type with more text fields must revisit this cap.
const maxNoteBytes = 512 << 10

// noteKeyMargin is the slack of the note key caps. Every key beyond a
// note's shape is refused anyway (strict decoding at the top level,
// validation in fields); the margin only decides where such a body is a
// 400/422 rather than a 413.
const noteKeyMargin = 4

// noteKeyLimits derives the key caps of a note body from the registry. A
// note has 5 top-level keys (record_type, date, data_subject, fields,
// on_behalf_of) and in "fields" at most the widest household type
// version's field count (no field value is an object). Per object: the
// larger of the two plus the margin; per document: both plus the margin.
// With the test fixtures (attendance_note, 4 fields) that is 9 and 13.
func noteKeyLimits(reg *record.Registry) strictjson.Limits {
	widest := 0
	for _, t := range reg.Types() {
		if t.Household() {
			widest = max(widest, len(t.Fields))
		}
	}
	return strictjson.Limits{
		MaxObjectKeys: max(5, widest) + noteKeyMargin,
		MaxTotalKeys:  5 + widest + noteKeyMargin,
	}
}

// handleNotes is POST /v1/notes (4.9a, 5.3). Order: the service writes a
// household type at all (403 policy, before the body is read), Content-Type
// (400), body at most maxNoteBytes (413), key caps (413), strict decoding
// (400: an access field other than on_behalf_of -- audience, grants,
// reader -- is an unknown key), validation (422/413), the service may write
// the type on behalf of that member (403 policy: the type is in
// write_types and on_behalf_of is in the service's readers), then
// WriteNote, which checks the ban (410), roster membership (403 / 409) and
// the member's computed grants (403).
func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request, c call) {
	if !c.svc.WritesNotes(s.d.Registry) {
		refuse(w, http.StatusForbidden, journalapi.ReasonPolicy, "this service writes no household notes")
		return
	}
	if !isJSON(r) {
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNoteBytes))
	if err != nil {
		w.Header().Set("Connection", "close")
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge, "the body exceeds 512 KiB")
			return
		}
		s.d.Log.Warn("journal request body could not be read", "err", err)
		refuse(w, http.StatusInternalServerError, journalapi.ReasonInternal, "")
		return
	}
	var in journalapi.Note
	if err := strictjson.DecodeLimits(body, &in, s.noteKeys); err != nil {
		if errors.Is(err, strictjson.ErrTooManyKeys) {
			refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge, "the body has more keys than any note can")
			return
		}
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest,
			"a note carries record_type, date, data_subject, fields and on_behalf_of, and nothing else")
		return
	}
	n, rej := record.ValidateNote(s.d.Registry, in)
	if rej != nil {
		reject(w, rej)
		return
	}
	if rej := c.svc.MayWriteNote(n.Type, n.OnBehalfOf); rej != nil {
		reject(w, rej)
		return
	}
	resp, rej, err := s.d.Journal.WriteNote(r.Context(), c.caller, n)
	if err != nil {
		s.internal(w, "note", err)
		return
	}
	if rej != nil {
		reject(w, rej)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
