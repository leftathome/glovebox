// Package journalapi is the wire contract of the record journal (spec
// 2026-10-16 sections 4.3, 4.9, 4.12, 5.3): the paths, the token audience,
// the request and response bodies, and the status and reason codes a
// connector's spool acts on. The journal's server, connectors (Part B) and
// readers (Part C) all build against these types.
//
// Conventions: every time.Time is RFC 3339 in UTC. The server rejects unknown
// fields in request bodies (strict decoding happens in its handlers, not in
// these types). List and map fields of responses are always encoded as [] or
// {}, never null.
package journalapi

import (
	"encoding/json"
	"time"
)

// Audience is the audience a caller's projected ServiceAccount token must
// be minted for (4.9).
const Audience = "glovebox-journal"

// Paths.
const (
	PathRecords     = "/v1/records"
	PathNotes       = "/v1/notes"
	PathState       = "/v1/state"
	PathEvents      = "/v1/events"
	PathCounts      = "/v1/counts"
	PathHealthz     = "/v1/healthz"
	PathAdminHeld   = "/v1/admin/held/" // + "{snapshot_id}:confirm" or ":discard"
	PathAdminErase  = "/v1/admin/erase"
	PathAdminRescan = "/v1/admin/rescan"
)

// Held-snapshot actions.
const (
	ActionConfirm = "confirm"
	ActionDiscard = "discard"
)

// HeldPath is the admin path that confirms or discards a held snapshot.
func HeldPath(snapshotID, action string) string {
	return PathAdminHeld + snapshotID + ":" + action
}

// Statuses. The first five answer POST /v1/records with HTTP 200; a
// connector deletes its spool file on any of them (4.3).
const (
	StatusApplied   = "applied"
	StatusDuplicate = "duplicate"
	StatusLate      = "late"
	StatusHeld      = "held"
	StatusDiscarded = "discarded"
	StatusRejected  = "rejected"
)

// StatusStarted answers POST /v1/admin/rescan (HTTP 202).
const StatusStarted = "started"

// Event kinds (Event.Kind).
const (
	KindCreated  = "created"
	KindUpdated  = "updated"
	KindRemoved  = "removed"
	KindRestored = "restored"
)

// Event importance (Event.Importance).
const (
	ImportanceHigh   = "high"
	ImportanceNormal = "normal"
	ImportanceLow    = "low"
)

// Notice modes (Notice.Mode).
const (
	ModePush   = "push"
	ModeDigest = "digest"
	ModePrompt = "prompt"
)

// NoteResponse statuses.
const (
	NoteCreated   = "created"
	NoteUpdated   = "updated"
	NoteUnchanged = "unchanged"
)

// HealthOK is Healthz.Status when the journal is ready.
const HealthOK = "ok"

// WithheldPrefix starts every string that replaces a value the reader may
// not see (4.8, 4.11).
const WithheldPrefix = "withheld: "

// Reasons accompany StatusRejected. The HTTP status says what the spool
// does; see the table in docs/journal-api.md.
const (
	ReasonErasedSubject    = "erased_subject"     // 410: delete the file
	ReasonPolicy           = "policy"             // 403: move to spool/rejected/
	ReasonUnknownSubject   = "unknown_subject"    // 409: keep, retry with backoff
	ReasonTooLarge         = "too_large"          // 413: move to spool/rejected/
	ReasonSchema           = "schema"             // 422: move to spool/rejected/
	ReasonFutureObservedAt = "future_observed_at" // 422
	ReasonDuplicateKey     = "duplicate_key"      // 422
	ReasonBadRequest       = "bad_request"        // 400
	ReasonUnauthenticated  = "unauthenticated"    // 401
	ReasonForbidden        = "forbidden"          // 403: not an allow-listed ServiceAccount
	ReasonAuthUnavailable  = "auth_unavailable"   // 503: TokenReview could not be asked
	ReasonUnavailable      = "unavailable"        // 503: the journal is not ready
	ReasonNotFound         = "not_found"          // 404
	ReasonConflict         = "conflict"           // 409 on admin endpoints
	ReasonSnapshotIDReused = "snapshot_id_reused" // 422: the id belongs to another writer, type or scope; permanent, move to spool/rejected/
	ReasonInternal         = "internal"           // 500
)

// ScopePart is one ["name", "value"] pair of a scope.
type ScopePart [2]string

// Snapshot is the body of POST /v1/records.
type Snapshot struct {
	SnapshotID    string         `json:"snapshot_id"`
	RecordType    string         `json:"record_type"`
	SchemaVersion int            `json:"schema_version"`
	Scope         []ScopePart    `json:"scope"`
	Complete      bool           `json:"complete"`
	CompleteRange *CompleteRange `json:"complete_range"`
	ObservedAt    time.Time      `json:"observed_at"`
	Approximate   bool           `json:"approximate"`
	Records       []Record       `json:"records"`
}

// CompleteRange limits a complete snapshot's removals to records whose
// date field lies in [From, To] (4.3). From and To are YYYY-MM-DD dates.
type CompleteRange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Record is one record of a snapshot.
type Record struct {
	Key         string                     `json:"key"`
	DataSubject string                     `json:"data_subject"`
	Audience    []string                   `json:"audience,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields"`
}

// Stats counts what a snapshot did (or, when held, would do).
type Stats struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Removed   int `json:"removed"`
	Restored  int `json:"restored"`
	Unchanged int `json:"unchanged"`
	Stale     int `json:"stale"`
}

// Response answers POST /v1/records, POST /v1/admin/held/..., and every
// refusal of any endpoint.
type Response struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	Stats  *Stats `json:"stats,omitempty"`
}

// Note is the body of POST /v1/notes (5.3). OnBehalfOf is the request's
// reader: the only access field a caller may send.
type Note struct {
	RecordType  string                     `json:"record_type"`
	Date        string                     `json:"date"`
	DataSubject string                     `json:"data_subject"`
	Fields      map[string]json.RawMessage `json:"fields"`
	OnBehalfOf  string                     `json:"on_behalf_of"`
}

// NoteResponse answers POST /v1/notes. Status is NoteCreated, NoteUpdated or
// NoteUnchanged; Seq is omitted when it is NoteUnchanged.
type NoteResponse struct {
	Status string `json:"status"`
	Key    string `json:"key"`
	Seq    int64  `json:"seq,omitempty"`
}

// StateRow is one record as GET /v1/state returns it (4.12).
type StateRow struct {
	RecordType  string                     `json:"record_type"`
	DataSubject string                     `json:"data_subject"`
	Key         string                     `json:"key"`
	Scope       string                     `json:"scope"`
	Audience    []string                   `json:"audience"`
	Fields      map[string]json.RawMessage `json:"fields"`
	Removed     bool                       `json:"removed"`
	FirstSeenAt time.Time                  `json:"first_seen_at"`
	LastSeenAt  time.Time                  `json:"last_seen_at"`
	ConfirmedAt time.Time                  `json:"confirmed_at"`
	// InLatestSnapshot: whether the newest applied snapshot of the
	// record's scope contained it. Absent for household notes.
	InLatestSnapshot *bool `json:"in_latest_snapshot,omitempty"`
	// Approximate: the current value came from seeding.
	Approximate bool `json:"approximate"`
}

// StateResponse answers GET /v1/state. Rows is always [], never null.
type StateResponse struct {
	Rows         []StateRow `json:"rows"`
	RosterSHA256 string     `json:"roster_sha256"`
}

// Change is one field of an "updated" event. A value the reader may not
// see is the string "withheld: ..." (WithheldPrefix; 4.8, 4.11).
type Change struct {
	Old json.RawMessage `json:"old"`
	New json.RawMessage `json:"new"`
}

// Notice is notification advice from the event's rule (4.10), filtered to
// the tokens the reader holds for the event's subject.
type Notice struct {
	Token   string `json:"token"`
	Mode    string `json:"mode"`
	Purpose string `json:"purpose,omitempty"`
}

// Event is one change as GET /v1/events returns it. There is deliberately no
// prev_audience on the wire (4.8): old values are masked for readers outside
// the old audience, but the audience change itself is not exposed.
type Event struct {
	Seq            int64                      `json:"seq"`
	RecordType     string                     `json:"record_type"`
	DataSubject    string                     `json:"data_subject"`
	Key            string                     `json:"key"`
	Kind           string                     `json:"kind"`
	SchemaVersion  int                        `json:"schema_version"`
	Scope          string                     `json:"scope"`
	PrevScope      string                     `json:"prev_scope,omitempty"`
	Audience       []string                   `json:"audience"`
	Fields         map[string]json.RawMessage `json:"fields,omitempty"`
	Changed        map[string]Change          `json:"changed,omitempty"`
	LastFields     map[string]json.RawMessage `json:"last_fields,omitempty"`
	ObservedAt     time.Time                  `json:"observed_at"`
	PrevObservedAt *time.Time                 `json:"prev_observed_at,omitempty"`
	Approximate    bool                       `json:"approximate"`
	Actor          string                     `json:"actor"`
	Importance     string                     `json:"importance"`
	Rule           string                     `json:"rule,omitempty"`
	GroupKey       string                     `json:"group_key,omitempty"`
	Notify         []Notice                   `json:"notify,omitempty"`
}

// EventsResponse answers GET /v1/events. Events is always [], never null. Next is the cursor to send as
// "after" next time: the last returned seq, or the request's "after".
type EventsResponse struct {
	Events []Event `json:"events"`
	Next   int64   `json:"next"`
}

// Count is one subject and record type in GET /v1/counts: no field values.
// Events is always an object ({} when empty), never null.
type Count struct {
	DataSubject string         `json:"data_subject"`
	RecordType  string         `json:"record_type"`
	Live        int            `json:"live"`
	Events      map[string]int `json:"events"` // by importance, since Since
}

// CountsResponse answers GET /v1/counts. Counts is always [], never null.
type CountsResponse struct {
	Since  time.Time `json:"since"`
	Counts []Count   `json:"counts"`
}

// Healthz answers GET /v1/healthz.
type Healthz struct {
	Status        string `json:"status"`
	RosterSHA256  string `json:"roster_sha256"`
	SchemaVersion int    `json:"schema_version"`
}

// EraseRequest is the body of POST /v1/admin/erase.
type EraseRequest struct {
	DataSubject string `json:"data_subject"`
}

// EraseResponse counts what an erase deleted.
type EraseResponse struct {
	DataSubject string    `json:"data_subject"`
	ErasedAt    time.Time `json:"erased_at"`
	Snapshots   int64     `json:"snapshots"`
	Current     int64     `json:"record_current"`
	Events      int64     `json:"record_events"`
}

// RescanRequest is the optional body of POST /v1/admin/rescan. All rescans
// every stored text value, not only those scanned by another scanner
// version.
type RescanRequest struct {
	All bool `json:"all"`
}

// RescanResult is logged when a rescan finishes.
type RescanResult struct {
	CurrentRows int `json:"current_rows"`
	EventRows   int `json:"event_rows"`
	Released    int `json:"released"`
	Withheld    int `json:"withheld"`
}
