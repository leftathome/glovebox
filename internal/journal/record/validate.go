package record

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Snapshot limits (4.3, 4.6).
const (
	MaxRecords    = 5000
	MaxKeyBytes   = 512
	MaxFutureSkew = 10 * time.Minute
)

// Range is a validated complete_range, its field under the active name.
type Range struct {
	Field, From, To string
}

// Contains reports whether a stored date value lies in [From, To]. A null
// or non-date value is outside: such a record is never removed by a
// windowed snapshot.
func (r *Range) Contains(v json.RawMessage) bool {
	if r == nil || v == nil {
		return false
	}
	var s string
	if json.Unmarshal(v, &s) != nil || !ValidDate(s) {
		return false
	}
	return s >= r.From && s <= r.To
}

// NormRecord is one validated record.
type NormRecord struct {
	Key         string
	DataSubject string
	Audience    []string // sorted, distinct
	Fields      Fields   // canonical, active names
	Hash        []byte
}

// Normalized is a validated snapshot, ready for Apply.
type Normalized struct {
	ID          string // canonical UUID
	Type        Type   // the active version
	Version     int    // the version it was written in
	Scope       Scope
	ScopeKey    string // Scope.Canonical()
	Subject     string // the scope's data_subject part, "" if none
	Complete    bool
	Range       *Range
	ObservedAt  time.Time // UTC
	Approximate bool
	Records     []NormRecord
}

// ValidateSnapshot runs the database-free checks of 4.3 and 4.4 step 2
// (except policy) and 4.4 step 5 (renames).
func ValidateSnapshot(reg *Registry, s journalapi.Snapshot, now time.Time) (*Normalized, *Rejection) {
	id, err := uuid.Parse(s.SnapshotID)
	if err != nil {
		return nil, schemaErr("snapshot_id must be a UUID")
	}
	if !ValidName(s.RecordType) {
		return nil, schemaErr("invalid record_type")
	}
	t, ok := reg.Version(s.RecordType, s.SchemaVersion)
	if !ok {
		return nil, schemaErr("unknown record type %q version %d", s.RecordType, s.SchemaVersion)
	}
	if t.Household() {
		return nil, schemaErr("household record types are written through %s", journalapi.PathNotes)
	}
	active, _ := reg.Active(s.RecordType)
	sc, rej := ParseScope(s.Scope)
	if rej != nil {
		return nil, rej
	}
	if s.ObservedAt.IsZero() {
		return nil, schemaErr("observed_at is required")
	}
	if y := s.ObservedAt.UTC().Year(); y < 0 || y > 9999 {
		return nil, schemaErr("observed_at is outside years 0000-9999 in UTC")
	}
	if s.ObservedAt.After(now.Add(MaxFutureSkew)) {
		return nil, &Rejection{Status: http.StatusUnprocessableEntity, Reason: journalapi.ReasonFutureObservedAt,
			Detail: "observed_at is more than 10 minutes ahead of the journal's clock"}
	}
	if len(s.Records) > MaxRecords {
		return nil, tooLarge("%d records exceed %d per snapshot", len(s.Records), MaxRecords)
	}
	n := &Normalized{
		ID: id.String(), Type: active, Version: s.SchemaVersion, Scope: sc, ScopeKey: sc.Canonical(),
		Subject: sc.Subject(), Complete: s.Complete, ObservedAt: s.ObservedAt.UTC(), Approximate: s.Approximate,
	}
	if sc.Source() != active.Owner {
		return nil, schemaErr("the scope's source part is not the owner of %s records", t.Name)
	}
	if n.Subject == "" && !active.AllowNoSubject {
		return nil, schemaErr("the scope has no data_subject part and %s records are always about someone", t.Name)
	}
	if cr := s.CompleteRange; cr != nil {
		if !s.Complete {
			return nil, schemaErr("complete_range requires complete=true")
		}
		if !ValidName(cr.Field) {
			return nil, schemaErr("invalid complete_range.field")
		}
		if t.Fields[cr.Field].Type != Date {
			return nil, schemaErr("complete_range.field %q is not a date field of %s v%d", cr.Field, t.Name, t.SchemaVersion)
		}
		if af := reg.ForwardName(t.Name, t.SchemaVersion, cr.Field); active.Fields[af].Type != Date {
			return nil, schemaErr("complete_range.field %q is not a date field of the active version of %s", cr.Field, t.Name)
		}
		if !ValidDate(cr.From) || !ValidDate(cr.To) || cr.From > cr.To {
			return nil, schemaErr("complete_range needs from <= to, both YYYY-MM-DD")
		}
		n.Range = &Range{Field: reg.ForwardName(t.Name, t.SchemaVersion, cr.Field), From: cr.From, To: cr.To}
	}
	seen := make(map[string]bool, len(s.Records))
	n.Records = make([]NormRecord, 0, len(s.Records))
	// An absent or empty audience takes the type's default: a record
	// visible to nobody is never intended.
	defAud, drej := NormalizeAudience(active.DefaultAudience)
	if drej != nil {
		defAud = nil // a type without a default: records must name one
	}
	for i, rec := range s.Records {
		if strings.TrimSpace(rec.Key) == "" || len(rec.Key) > MaxKeyBytes || badText(rec.Key) {
			return nil, schemaErr("record %d: key must be 1 to %d bytes without control characters", i, MaxKeyBytes)
		}
		if rec.DataSubject != n.Subject {
			return nil, schemaErr("record %d: data_subject must equal the scope's data_subject part", i)
		}
		if seen[rec.Key] {
			return nil, &Rejection{Status: http.StatusUnprocessableEntity, Reason: journalapi.ReasonDuplicateKey,
				Detail: "record " + strconv.Itoa(i) + ": (data_subject, key) repeats an earlier record"}
		}
		seen[rec.Key] = true
		var aud []string
		if len(rec.Audience) == 0 && defAud != nil {
			aud = defAud
		} else {
			var arej *Rejection
			aud, arej = NormalizeAudience(rec.Audience)
			if arej != nil {
				return nil, arej.prefixed("record " + strconv.Itoa(i) + ": ")
			}
		}
		f, rej := NormalizeFields(t, rec.Fields)
		if rej != nil {
			return nil, rej.prefixed("record " + strconv.Itoa(i) + ": ")
		}
		f = reg.Forward(t.Name, t.SchemaVersion, f)
		for name := range f {
			if _, ok := active.Fields[name]; !ok {
				return nil, schemaErr("record %d: field %q of v%d has no place in v%d", i, name, t.SchemaVersion, active.SchemaVersion)
			}
		}
		n.Records = append(n.Records, NormRecord{Key: rec.Key, DataSubject: rec.DataSubject, Audience: aud, Fields: f, Hash: f.Hash()})
	}
	return n, nil
}

// NormalizeAudience validates tokens and returns them sorted and distinct.
func NormalizeAudience(in []string) ([]string, *Rejection) {
	if len(in) == 0 {
		return nil, schemaErr("no audience: the record names none and its type has no default")
	}
	if len(in) > MaxAudienceTokens {
		return nil, tooLarge("an audience of %d tokens exceeds %d", len(in), MaxAudienceTokens)
	}
	set := map[string]bool{}
	for _, tok := range in {
		if !ValidToken(tok) {
			return nil, schemaErr("invalid audience token")
		}
		set[tok] = true
	}
	out := make([]string, 0, len(set))
	for tok := range set {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out, nil
}

// NormNote is a validated household write.
type NormNote struct {
	Type       Type
	Key        string // "<date>:<on_behalf_of>"
	Subject    string
	OnBehalfOf string
	Fields     Fields
	Hash       []byte
	Audience   []string // the type's fixed audience
	Scope      string
}

// ValidateNote shapes POST /v1/notes (4.9a, 5.3). Whether on_behalf_of may
// write for the subject needs the roster; the journal checks that.
func ValidateNote(reg *Registry, in journalapi.Note) (*NormNote, *Rejection) {
	if !ValidName(in.RecordType) {
		return nil, schemaErr("invalid record_type")
	}
	t, ok := reg.Active(in.RecordType)
	if !ok || !t.Household() {
		return nil, schemaErr("%q is not a household record type", in.RecordType)
	}
	if !ValidDate(in.Date) {
		return nil, schemaErr("date must be YYYY-MM-DD")
	}
	if in.OnBehalfOf == "" || badScopeValue(in.OnBehalfOf) {
		return nil, schemaErr("on_behalf_of names the roster member the note is written for")
	}
	if in.DataSubject == "" && !t.AllowNoSubject {
		return nil, schemaErr("data_subject is required for %s", t.Name)
	}
	if in.DataSubject != "" && badScopeValue(in.DataSubject) {
		return nil, schemaErr("invalid data_subject")
	}
	if _, ok := in.Fields["date"]; ok {
		return nil, schemaErr("date is given once, at the top level of the note")
	}
	fields := make(map[string]json.RawMessage, len(in.Fields)+1)
	for k, v := range in.Fields {
		fields[k] = v
	}
	fields["date"] = encode(in.Date)
	f, rej := NormalizeFields(t, fields)
	if rej != nil {
		return nil, rej
	}
	aud, rej := NormalizeAudience(t.FixedAudience)
	if rej != nil {
		return nil, rej
	}
	return &NormNote{
		Type: t, Key: in.Date + ":" + in.OnBehalfOf, Subject: in.DataSubject, OnBehalfOf: in.OnBehalfOf,
		Fields: f, Hash: f.Hash(), Audience: aud, Scope: HouseholdScope(t.Name, in.DataSubject),
	}, nil
}

// badText reports invalid UTF-8, C0/C1 and bidi controls.
func badText(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || suspicious(r) {
			return true
		}
	}
	return false
}

// badScopeValue applies the scope-value rules (length, UTF-8, controls).
func badScopeValue(s string) bool {
	_, rej := ParseScope([]journalapi.ScopePart{{PartSource, s}})
	return rej != nil
}
