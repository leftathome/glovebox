package records

import (
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Action is what the spool does with a file after an answer (spec 4.3 as
// built; docs/journal-api.md "Answers, and what the connector's spool
// does").
type Action int

const (
	ActDelete     Action = iota // accepted: delete the file
	ActErased                   // 410 erased_subject: delete, and purge the subject's other files (A10)
	ActReject                   // permanent refusal: move to rejected/
	ActRetryScope               // 409 unknown_subject: keep; block this scope only, retry with backoff
	ActRetry                    // the journal is unavailable: keep; stop the pass, retry with backoff
	ActRetryAlert               // a deployment fault or an answer the table does not name: keep, retry, alert
)

// String is the action's metric label.
func (a Action) String() string {
	switch a {
	case ActDelete:
		return "delete"
	case ActErased:
		return "erased"
	case ActReject:
		return "reject"
	case ActRetryScope:
		return "retry_scope"
	case ActRetry:
		return "retry"
	case ActRetryAlert:
		return "retry_alert"
	}
	return "unknown"
}

// accepted are the 200 statuses; the file is deleted on any of them.
var accepted = map[string]bool{
	journalapi.StatusApplied: true, journalapi.StatusDuplicate: true,
	journalapi.StatusLate: true, journalapi.StatusHeld: true,
	// Not answered by /v1/records today; journalapi documents it as delete.
	journalapi.StatusDiscarded: true,
}

// permanent are the only (code, reason) pairs that move a file to
// rejected/.
var permanent = map[int]map[string]bool{
	http.StatusForbidden:             {journalapi.ReasonPolicy: true},
	http.StatusRequestEntityTooLarge: {journalapi.ReasonTooLarge: true},
	http.StatusUnprocessableEntity: {journalapi.ReasonSchema: true, journalapi.ReasonFutureObservedAt: true,
		journalapi.ReasonDuplicateKey: true, journalapi.ReasonSnapshotIDReused: true},
}

// unavailable are the 503 reasons: the journal is starting, busy, or cannot
// ask TokenReview. "" covers a 503 from something in front of it.
var unavailable = map[string]bool{
	journalapi.ReasonUnavailable: true, journalapi.ReasonAuthUnavailable: true, "": true,
}

// Classify maps an answer to an action. code 0 is a transport error. The
// spool keys on reason: only the reasons the journal documents as
// permanent move a file to rejected/, only 410 erased_subject deletes
// unsent, and anything the table does not name is kept and alerted on
// (ActRetryAlert) -- never dropped. The documented alert rows (400
// bad_request, 401 unauthenticated, 403 forbidden, 404 not_found) are
// ActRetryAlert too, so they need no row of their own.
func Classify(code int, status, reason string) Action {
	switch {
	case code == 0:
		return ActRetry
	case code == http.StatusOK:
		if accepted[status] {
			return ActDelete
		}
		return ActRetryAlert
	case code == http.StatusGone && reason == journalapi.ReasonErasedSubject:
		return ActErased
	case permanent[code][reason]:
		return ActReject
	case code == http.StatusConflict && reason == journalapi.ReasonUnknownSubject:
		return ActRetryScope
	case code == http.StatusServiceUnavailable:
		if unavailable[reason] {
			return ActRetry
		}
		return ActRetryAlert
	case code >= 500 && code <= 599:
		return ActRetry
	}
	return ActRetryAlert
}

var knownReasons = map[string]bool{}

func init() {
	for _, r := range []string{journalapi.ReasonErasedSubject, journalapi.ReasonPolicy, journalapi.ReasonUnknownSubject,
		journalapi.ReasonTooLarge, journalapi.ReasonSchema, journalapi.ReasonFutureObservedAt, journalapi.ReasonDuplicateKey,
		journalapi.ReasonBadRequest, journalapi.ReasonUnauthenticated, journalapi.ReasonForbidden, journalapi.ReasonAuthUnavailable,
		journalapi.ReasonUnavailable, journalapi.ReasonNotFound, journalapi.ReasonConflict, journalapi.ReasonSnapshotIDReused,
		journalapi.ReasonInternal} {
		knownReasons[r] = true
	}
}

// ReasonLabel bounds a reason for a metric label or a log line: a known
// reason, "none" or "other".
func ReasonLabel(r string) string {
	if knownReasons[r] {
		return r
	}
	if r == "" {
		return "none"
	}
	return "other"
}
