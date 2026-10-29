package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// escapedBytes is the most bytes one byte of a string value can take in
// JSON: the \u-escaped form. Validation admits no control characters, so
// every legitimate value fits at this width whatever encoder sent it.
const escapedBytes = 6

// noteEnvelopeBytes is the slack of the note byte cap for everything but
// the fields' values: the top-level keys and record_type, date, data_subject
// and on_behalf_of (each at most a scope value, 256 bytes, escaped: about
// 5 KiB), the fields' keys, and whitespace.
const noteEnvelopeBytes = 16 << 10

// fieldValueBytes is the widest JSON encoding of one valid value of a field
// type, with every string byte escaped.
func fieldValueBytes(t record.FieldType) int {
	switch t {
	case record.String:
		return escapedBytes*record.MaxStringBytes + 2
	case record.Text:
		return escapedBytes*record.MaxTextBytes + 2
	case record.StringList:
		return record.MaxListItems*(escapedBytes*record.MaxStringBytes+3) + 2
	default: // int, number, bool, date, timestamp
		return 256
	}
}

// noteByteLimit derives the POST /v1/notes body cap from the registry, as
// noteKeyLimits derives the key caps: the widest household type version's
// fields at their widest encoding (fieldValueBytes, plus the escaped key),
// plus noteEnvelopeBytes; never more than a snapshot's 8 MiB. With the test
// fixtures (attendance_note: a text, two strings, a date) it is 422,442
// bytes, about 413 KiB. TestWidestNoteFitsTheByteCap sends the widest
// attendance_note.
func noteByteLimit(reg *record.Registry) int64 {
	widest := 0
	for _, t := range reg.Types() {
		if !t.Household() {
			continue
		}
		n := 0
		for name, def := range t.Fields {
			n += escapedBytes*len(name) + 4 + fieldValueBytes(def.Type)
		}
		widest = max(widest, n)
	}
	return min(int64(widest+noteEnvelopeBytes), maxSnapshotBytes)
}

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
// household type at all (403 policy, before the body is read), no query
// parameters (400), Content-Type
// (400), body at most noteByteLimit (413), key caps (413), strict decoding
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
	// A note names its member in the body (on_behalf_of) and nothing in the
	// query: any query parameter, an access field above all, is refused
	// before the body is read (4.9).
	if _, err := queryParams(r); err != nil {
		badRequest(w, "POST /v1/notes takes no query parameters")
		return
	}
	if !isJSON(r) {
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.noteBytes))
	if err != nil {
		w.Header().Set("Connection", "close")
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge, "the body exceeds the largest note")
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
