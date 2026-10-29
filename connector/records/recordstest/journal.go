// Package recordstest is a fake record journal for connector tests. It
// answers POST /v1/records as docs/journal-api.md documents, in the
// journal's order of checks, including every refusal of the "Answers, and
// what the connector's spool does" table:
//
//	404 not_found           any other path or method
//	503 unavailable         SetReady(false) (before authentication)
//	401 unauthenticated     a missing, malformed or wrong bearer token
//	503 unavailable         more than 16 requests in flight, Retry-After: 1
//	503 auth_unavailable    SetAuthUnavailable(true)
//	403 forbidden           Forbid(true)
//	403 policy              NoWrites(true) (before anything else of the endpoint)
//	400 bad_request         a query string; Content-Type not application/json
//	413 too_large           a body over 8 MiB (connection closed), or too many keys
//	422 schema              a body that does not decode strictly
//	200 duplicate / held    a resend of a stored id (same type and scope)
//	422 snapshot_id_reused  a stored id with another type or scope
//	422 / 413 ...           record.ValidateSnapshot, when Registry is set
//	403 policy              Deny(recordType)
//	410 erased_subject      Erase(subject)
//	409 unknown_subject     Unknown(subject, true)
//	200 late                a snapshot of a scope already applied with a later observed_at
//	200 held                HoldNext()
//	200 applied             otherwise
//	500 internal            scripted with Next
//
// Next scripts answers given before the normal path (nothing is stored for
// them); LoseNextAnswer applies the next snapshot and then drops the
// connection without answering.
package recordstest

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// MaxInFlight is the journal's in-flight limit.
const MaxInFlight = 16

const maxSnapshotBytes = 8 << 20

// Journal is the fake. Set Registry and Now before the first request.
type Journal struct {
	*httptest.Server
	Token    string
	Registry *record.Registry // nil: no validation beyond decoding and the scope
	Now      func() time.Time

	slots chan struct{}

	mu              sync.Mutex
	stored          map[string]stored // snapshot_id -> type, scope and outcome
	latest          map[string]time.Time
	received        []journalapi.Snapshot
	answers         []Answer
	erased          map[string]bool
	unknown         map[string]bool
	denied          map[string]bool
	forbidden       bool
	noWrites        bool
	notReady        bool
	authUnavailable bool
	holdNext        bool
	loseNext        bool
	block           chan struct{}
	script          []Answer
}

type stored struct {
	recordType, scope, status string
}

// Answer is one answer the fake gave (or is scripted to give). Drop: close
// the connection without answering.
type Answer struct {
	Code       int
	Reason     string
	Status     string
	RetryAfter int
	Drop       bool
}

// New starts the fake; t.Cleanup closes it.
func New(t testing.TB, token string) *Journal {
	j := &Journal{Token: token, Now: time.Now, slots: make(chan struct{}, MaxInFlight),
		stored: map[string]stored{}, latest: map[string]time.Time{},
		erased: map[string]bool{}, unknown: map[string]bool{}, denied: map[string]bool{}}
	j.Server = httptest.NewServer(http.HandlerFunc(j.serve))
	t.Cleanup(j.Close)
	return j
}

// Erase bans every later snapshot about subject (410).
func (j *Journal) Erase(subject string) { j.mu.Lock(); j.erased[subject] = true; j.mu.Unlock() }

// Unknown makes subject unknown to the roster (409) or known again.
func (j *Journal) Unknown(subject string, unknown bool) {
	j.mu.Lock()
	j.unknown[subject] = unknown
	j.mu.Unlock()
}

// Forbid answers every authenticated request 403 forbidden (no journal
// policy for the caller).
func (j *Journal) Forbid(on bool) { j.mu.Lock(); j.forbidden = on; j.mu.Unlock() }

// NoWrites makes the caller a service that writes no snapshots (403 policy).
func (j *Journal) NoWrites(on bool) { j.mu.Lock(); j.noWrites = on; j.mu.Unlock() }

// Deny refuses snapshots of recordType after validation (403 policy).
func (j *Journal) Deny(recordType string) { j.mu.Lock(); j.denied[recordType] = true; j.mu.Unlock() }

// SetReady(false) answers everything 503 unavailable, before auth.
func (j *Journal) SetReady(ready bool) { j.mu.Lock(); j.notReady = !ready; j.mu.Unlock() }

// SetAuthUnavailable makes TokenReview unreachable (503 auth_unavailable).
func (j *Journal) SetAuthUnavailable(on bool) { j.mu.Lock(); j.authUnavailable = on; j.mu.Unlock() }

// HoldNext makes the removal guard hold the next snapshot that would be
// applied (200 held; a resend is answered held too).
func (j *Journal) HoldNext() { j.mu.Lock(); j.holdNext = true; j.mu.Unlock() }

// LoseNextAnswer applies the next snapshot that would be applied and then
// closes the connection without answering.
func (j *Journal) LoseNextAnswer() { j.mu.Lock(); j.loseNext = true; j.mu.Unlock() }

// Block makes authenticated requests wait (holding their in-flight slot)
// until the returned function is called.
func (j *Journal) Block() (release func()) {
	ch := make(chan struct{})
	j.mu.Lock()
	j.block = ch
	j.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			j.mu.Lock()
			j.block = nil
			j.mu.Unlock()
			close(ch)
		})
	}
}

// InFlight is the number of in-flight slots held.
func (j *Journal) InFlight() int { return len(j.slots) }

// Next scripts answers, given in order before the normal path.
func (j *Journal) Next(a ...Answer) { j.mu.Lock(); j.script = append(j.script, a...); j.mu.Unlock() }

// Received returns the snapshots stored (applied, late or held), in arrival
// order. A duplicate is not received twice.
func (j *Journal) Received() []journalapi.Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]journalapi.Snapshot(nil), j.received...)
}

// Answers returns every answer given, in order.
func (j *Journal) Answers() []Answer {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Answer(nil), j.answers...)
}

func (j *Journal) record(a Answer) {
	j.mu.Lock()
	j.answers = append(j.answers, a)
	j.mu.Unlock()
}

func (j *Journal) refuse(w http.ResponseWriter, code int, reason string) {
	j.write(w, Answer{Code: code, Reason: reason, Status: journalapi.StatusRejected})
}

func (j *Journal) write(w http.ResponseWriter, a Answer) {
	j.record(a)
	if a.Drop {
		drop(w)
		return
	}
	if a.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(a.RetryAfter))
	}
	if a.Code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(a.Code)
	resp := journalapi.Response{Status: a.Status, Reason: a.Reason}
	if resp.Status == "" {
		resp.Status = journalapi.StatusRejected
	}
	if a.Reason != "" {
		resp.Detail = "fake journal"
	}
	if a.Code == http.StatusOK {
		resp.Stats = &journalapi.Stats{}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func bearer(r *http.Request) (string, bool) {
	hs := r.Header.Values("Authorization")
	if len(hs) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(hs[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", false
	}
	return tok, true
}

func (j *Journal) serve(w http.ResponseWriter, r *http.Request) {
	// 1. Route.
	if r.URL.Path != journalapi.PathRecords || r.Method != http.MethodPost {
		j.refuse(w, http.StatusNotFound, journalapi.ReasonNotFound)
		return
	}
	j.mu.Lock()
	notReady := j.notReady
	j.mu.Unlock()
	// 2. Ready.
	if notReady {
		j.refuse(w, http.StatusServiceUnavailable, journalapi.ReasonUnavailable)
		return
	}
	// 3. Bearer token.
	if tok, ok := bearer(r); !ok || tok != j.Token {
		j.refuse(w, http.StatusUnauthorized, journalapi.ReasonUnauthenticated)
		return
	}
	// 4. An in-flight slot.
	select {
	case j.slots <- struct{}{}:
		defer func() { <-j.slots }()
	default:
		j.write(w, Answer{Code: http.StatusServiceUnavailable, Reason: journalapi.ReasonUnavailable, RetryAfter: 1})
		return
	}
	j.mu.Lock()
	block, authUnavailable, forbidden, noWrites := j.block, j.authUnavailable, j.forbidden, j.noWrites
	var scripted *Answer
	if len(j.script) > 0 {
		a := j.script[0]
		j.script = j.script[1:]
		scripted = &a
	}
	j.mu.Unlock()
	if block != nil {
		<-block
	}
	// 5. TokenReview.
	if authUnavailable {
		j.refuse(w, http.StatusServiceUnavailable, journalapi.ReasonAuthUnavailable)
		return
	}
	if forbidden {
		j.refuse(w, http.StatusForbidden, journalapi.ReasonForbidden)
		return
	}
	if scripted != nil {
		j.write(w, *scripted)
		return
	}
	// 6. The endpoint.
	if noWrites {
		j.refuse(w, http.StatusForbidden, journalapi.ReasonPolicy)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		j.refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		j.refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSnapshotBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		w.Header().Set("Connection", "close")
		if errors.As(err, &tooBig) {
			j.refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge)
			return
		}
		j.refuse(w, http.StatusInternalServerError, journalapi.ReasonInternal)
		return
	}
	var snap journalapi.Snapshot
	if err := strictjson.DecodeLimits(body, &snap, j.keyLimits()); err != nil {
		if errors.Is(err, strictjson.ErrTooManyKeys) {
			j.refuse(w, http.StatusRequestEntityTooLarge, journalapi.ReasonTooLarge)
			return
		}
		j.refuse(w, http.StatusUnprocessableEntity, journalapi.ReasonSchema)
		return
	}
	j.apply(w, snap)
}

func (j *Journal) keyLimits() strictjson.Limits {
	if j.Registry == nil {
		return strictjson.Limits{}
	}
	widest := 9
	for _, t := range j.Registry.Types() {
		widest = max(widest, len(t.Fields))
	}
	return strictjson.Limits{MaxObjectKeys: widest + 16, MaxTotalKeys: 9 + 3 + record.MaxRecords*(4+widest) + 1024}
}

func (j *Journal) apply(w http.ResponseWriter, snap journalapi.Snapshot) {
	sc, rej := record.ParseScope(snap.Scope)
	_, idErr := uuid.Parse(snap.SnapshotID)
	j.mu.Lock()
	// 4.4 step 1: a resend is answered from its stored outcome.
	if idErr == nil && rej == nil {
		if st, ok := j.stored[snap.SnapshotID]; ok {
			j.mu.Unlock()
			if st.recordType != snap.RecordType || st.scope != sc.Canonical() {
				j.refuse(w, http.StatusUnprocessableEntity, journalapi.ReasonSnapshotIDReused)
				return
			}
			status := journalapi.StatusDuplicate
			if st.status == journalapi.StatusHeld {
				status = journalapi.StatusHeld
			}
			j.write(w, Answer{Code: http.StatusOK, Status: status})
			return
		}
	}
	j.mu.Unlock()
	now := time.Now()
	if j.Now != nil {
		now = j.Now()
	}
	if j.Registry != nil {
		if _, vrej := record.ValidateSnapshot(j.Registry, snap, now); vrej != nil {
			j.refuse(w, vrej.Status, vrej.Reason)
			return
		}
	} else if idErr != nil || rej != nil {
		j.refuse(w, http.StatusUnprocessableEntity, journalapi.ReasonSchema)
		return
	}
	j.mu.Lock()
	if j.denied[snap.RecordType] {
		j.mu.Unlock()
		j.refuse(w, http.StatusForbidden, journalapi.ReasonPolicy)
		return
	}
	subject := sc.Subject()
	if j.erased[subject] {
		j.mu.Unlock()
		j.refuse(w, http.StatusGone, journalapi.ReasonErasedSubject)
		return
	}
	if j.unknown[subject] {
		j.mu.Unlock()
		j.refuse(w, http.StatusConflict, journalapi.ReasonUnknownSubject)
		return
	}
	key := snap.RecordType + "\x00" + sc.Canonical()
	status := journalapi.StatusApplied
	switch {
	case !j.latest[key].IsZero() && snap.ObservedAt.Before(j.latest[key]):
		status = journalapi.StatusLate
	case j.holdNext:
		status, j.holdNext = journalapi.StatusHeld, false
	default:
		j.latest[key] = snap.ObservedAt
	}
	j.stored[snap.SnapshotID] = stored{recordType: snap.RecordType, scope: sc.Canonical(), status: status}
	j.received = append(j.received, snap)
	lose := j.loseNext
	j.loseNext = false
	j.mu.Unlock()
	j.write(w, Answer{Code: http.StatusOK, Status: status, Drop: lose})
}
