package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestNotes(t *testing.T) {
	r := newRig(t)
	note := `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1",
		"on_behalf_of": "e_parent1", "fields": {"what_happened": "fever"}}`
	var resp journalapi.NoteResponse
	if code := r.do("POST", journalapi.PathNotes, "gw", note, &resp); code != 200 || resp.Status != "created" || resp.Key != "2026-10-14:e_parent1" {
		t.Fatalf("%d %+v", code, resp)
	}
	for name, c := range map[string]struct {
		token, body string
		status      int
		reason      string
	}{
		"audience smuggled":    {"gw", `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "audience": ["public"]}`, 400, journalapi.ReasonBadRequest},
		"grants smuggled":      {"gw", `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "grants": [["e_child2", "guardians"]]}`, 400, journalapi.ReasonBadRequest},
		"no grant":             {"gw", `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_guest1"}`, 403, journalapi.ReasonPolicy},
		"service may not":      {"dash", note, 403, journalapi.ReasonPolicy},
		"not a household type": {"gw", `{"record_type": "assignment", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1"}`, 422, journalapi.ReasonSchema},
		"unknown subject":      {"gw", `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_stranger", "on_behalf_of": "e_parent1"}`, 409, journalapi.ReasonUnknownSubject},
	} {
		var rr journalapi.Response
		if code := r.do("POST", journalapi.PathNotes, c.token, c.body, &rr); code != c.status || rr.Reason != c.reason {
			t.Errorf("%s: %d %+v", name, code, rr)
		}
	}
}

func noteBody(onBehalfOf, subject, what string) string {
	return fmt.Sprintf(`{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": %q, "on_behalf_of": %q, "fields": {"what_happened": %q}}`,
		subject, onBehalfOf, what)
}

// on_behalf_of follows the service's readers, both ways: the gateway
// (readers "*") writes for any roster member who holds a grant; the test
// note writer (readers [e_parent2]) writes for e_parent2 and for nobody
// else, not even another guardian with the same grants. The writer and the
// member are recorded as the actor.
func TestNoteOnBehalfOfFollowsPolicy(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		token, onBehalfOf, subject string
		status                     int
		reason                     string
	}{
		"gateway for a guardian":         {"gw", "e_parent1", "e_child1", 200, ""},
		"gateway for the subject":        {"gw", "e_child2", "e_child2", 200, ""},
		"gateway for a sibling":          {"gw", "e_child2", "e_child1", 403, journalapi.ReasonPolicy},
		"gateway for a stranger":         {"gw", "e_nobody", "e_child1", 403, journalapi.ReasonPolicy},
		"note writer for its member":     {"nw", "e_parent2", "e_child1", 200, ""},
		"note writer for another parent": {"nw", "e_parent1", "e_child1", 403, journalapi.ReasonPolicy},
		"note writer for a child":        {"nw", "e_child1", "e_child1", 403, journalapi.ReasonPolicy},
		"reader-only service":            {"ro", "e_parent1", "e_child1", 403, journalapi.ReasonPolicy},
		"counts-only service":            {"co", "e_parent1", "e_child1", 403, journalapi.ReasonPolicy},
		"snapshot writer":                {"ps", "e_parent1", "e_child1", 403, journalapi.ReasonPolicy},
	} {
		code, raw := r.send("POST", journalapi.PathNotes, c.token, "application/json", noteBody(c.onBehalfOf, c.subject, "fever"))
		var rr journalapi.Response
		_ = json.Unmarshal(raw, &rr)
		if code != c.status || rr.Reason != c.reason {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	var ev journalapi.EventsResponse
	if code := r.do("GET", journalapi.PathEvents+"?reader=e_parent1", "gw", nil, &ev); code != 200 {
		t.Fatalf("events: %d", code)
	}
	actors := map[string]bool{}
	for _, e := range ev.Events {
		actors[e.Key+" "+e.Actor] = true
	}
	for _, want := range []string{
		"2026-10-14:e_parent1 " + journaltest.SAGateway + ";on_behalf_of=e_parent1",
		"2026-10-14:e_child2 " + journaltest.SAGateway + ";on_behalf_of=e_child2",
		"2026-10-14:e_parent2 " + SANoteWriter + ";on_behalf_of=e_parent2",
	} {
		if !actors[want] {
			t.Errorf("no event %q in %v", want, actors)
		}
	}
	if len(ev.Events) != 3 {
		t.Errorf("events: %+v", ev.Events)
	}
}

// A resend is unchanged, a change is updated; the body never carries the
// key's writer other than as on_behalf_of.
func TestNoteUpdateAndResend(t *testing.T) {
	r := newRig(t)
	var resp journalapi.NoteResponse
	body := noteBody("e_parent1", "e_child1", "fever")
	if code := r.do("POST", journalapi.PathNotes, "gw", body, &resp); code != 200 || resp.Status != journalapi.NoteCreated || resp.Seq == 0 {
		t.Fatalf("%d %+v", code, resp)
	}
	resp = journalapi.NoteResponse{}
	if code := r.do("POST", journalapi.PathNotes, "gw", body, &resp); code != 200 || resp.Status != journalapi.NoteUnchanged || resp.Seq != 0 {
		t.Errorf("resend: %d %+v", code, resp)
	}
	if code := r.do("POST", journalapi.PathNotes, "gw", noteBody("e_parent1", "e_child1", "a cold"), &resp); code != 200 || resp.Status != journalapi.NoteUpdated {
		t.Errorf("update: %d %+v", code, resp)
	}
}

// The ban and the missing roster come from the journal and are mapped as
// 4.3 maps them for snapshots.
func TestNoteJournalRefusals(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.Erase(context.Background(), journalapiAdmin, "e_child2"); err != nil {
		t.Fatal(err)
	}
	var rr journalapi.Response
	if code := r.do("POST", journalapi.PathNotes, "gw", noteBody("e_parent1", "e_child2", "x"), &rr); code != 410 || rr.Reason != journalapi.ReasonErasedSubject {
		t.Errorf("about an erased member: %d %+v", code, rr)
	}
	if code := r.do("POST", journalapi.PathNotes, "gw", noteBody("e_child2", "e_child1", "x"), &rr); code != 410 || rr.Reason != journalapi.ReasonErasedSubject {
		t.Errorf("by an erased member: %d %+v", code, rr)
	}
	r.ros.Store(nil)
	if code := r.do("POST", journalapi.PathNotes, "gw", noteBody("e_parent1", "e_child1", "x"), &rr); code != 409 || rr.Reason != journalapi.ReasonUnknownSubject {
		t.Errorf("no roster: %d %+v", code, rr)
	}
}

// Decoding is strict: unknown, duplicate and case-variant keys, trailing
// data and a non-object are 400; a wrong Content-Type is 400 before the body.
func TestNoteStrictDecoding(t *testing.T) {
	r := newAuthRig(t)
	for name, b := range map[string]string{
		"unknown key":        `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "reader": "e_child1"}`,
		"duplicate key":      `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_guest1", "on_behalf_of": "e_parent1"}`,
		"case variant":       `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "On_Behalf_Of": "e_guest1"}`,
		"duplicate in field": `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "fields": {"what_happened": "a", "what_happened": "b"}}`,
		"trailing data":      `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1"} {}`,
		"not an object":      `["attendance_note"]`,
		"empty":              ``,
	} {
		code, raw := r.send("POST", journalapi.PathNotes, "gw", "application/json", b)
		var rr journalapi.Response
		_ = json.Unmarshal(raw, &rr)
		if code != 400 || rr.Reason != journalapi.ReasonBadRequest {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	for _, ct := range []string{"", "text/plain"} {
		code, raw := r.send("POST", journalapi.PathNotes, "gw", ct, noteBody("e_parent1", "e_child1", "x"))
		if code != 400 || !strings.Contains(string(raw), journalapi.ReasonBadRequest) {
			t.Errorf("Content-Type %q: %d %s", ct, code, raw)
		}
	}
}

func TestNoteKeyLimitsFollowTheRegistry(t *testing.T) {
	if got := noteKeyLimits(journaltest.Registry(t)); got.MaxObjectKeys != 5+noteKeyMargin || got.MaxTotalKeys != 5+4+noteKeyMargin {
		t.Errorf("fixtures: %+v", got)
	}
	fields := map[string]record.FieldDef{"date": {Type: record.Date}}
	for i := 0; i < 11; i++ {
		fields[fmt.Sprintf("f%d", i)] = record.FieldDef{Type: record.String}
	}
	wide := record.Type{Name: "wide_note", SchemaVersion: 1, Owner: record.OwnerHousehold, FixedAudience: []string{"guardians"}, Fields: fields}
	// A wide source type does not widen the note caps.
	src := record.Type{Name: "wide_source", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: []string{"guardians"}, Fields: fields}
	if got := noteKeyLimits(journaltest.Registry(t, wide)); got.MaxObjectKeys != 12+noteKeyMargin || got.MaxTotalKeys != 5+12+noteKeyMargin {
		t.Errorf("wide note type: %+v", got)
	}
	if got := noteKeyLimits(journaltest.Registry(t, src)); got.MaxObjectKeys != 5+noteKeyMargin || got.MaxTotalKeys != 5+4+noteKeyMargin {
		t.Errorf("wide source type: %+v", got)
	}
}

// fieldsBody is a note whose fields object has n keys.
func fieldsBody(n int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "fields": {`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d": 1`, i)
	}
	b.WriteString("}}")
	return b.Bytes()
}

// The body is capped at maxNoteBytes and its keys at noteKeyLimits: over
// either is 413 too_large; at the caps it is decoded (and then refused by
// validation for its made-up fields, never as too large).
func TestNoteBodyCaps(t *testing.T) {
	r := newAuthRig(t)
	lim := r.s.noteKeys
	if lim.MaxObjectKeys != 9 || lim.MaxTotalKeys != 13 || maxNoteBytes != 512<<10 {
		t.Fatalf("caps %+v %d", lim, maxNoteBytes)
	}
	post := func(b []byte) (int, journalapi.Response) {
		code, raw := r.send("POST", journalapi.PathNotes, "gw", "application/json", b)
		var rr journalapi.Response
		_ = json.Unmarshal(raw, &rr)
		return code, rr
	}
	for name, c := range map[string]struct {
		body   []byte
		capped bool
	}{
		// fields at the per-object cap is 5+9 = 14 keys, over the total.
		"fields at the total cap": {fieldsBody(lim.MaxTotalKeys - 5), false},
		"fields over the total":   {fieldsBody(lim.MaxTotalKeys - 5 + 1), true},
		"top level over the cap":  {[]byte(`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9,"j":10}`), true},
		"top level at the cap":    {[]byte(`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9}`), false},
	} {
		code, rr := post(c.body)
		if c.capped && (code != 413 || rr.Reason != journalapi.ReasonTooLarge) {
			t.Errorf("%s: %d %+v", name, code, rr)
		}
		if !c.capped && (code == 413 || code/100 != 4) {
			t.Errorf("%s: %d %+v", name, code, rr)
		}
	}
	// 100k keys are refused in well under a second.
	start := time.Now()
	if code, rr := post(fieldsBody(100_000)); code != 413 || rr.Reason != journalapi.ReasonTooLarge {
		t.Errorf("100k keys: %d %+v", code, rr)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("100k keys took %s", el)
	}
	// The byte cap: padded with whitespace to exactly maxNoteBytes it is
	// read (and refused for its subject only later, by the journal, which
	// the auth rig cannot reach: 500); one byte more is 413.
	base := []byte(noteBody("e_parent1", "e_child1", "x"))
	at := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), maxNoteBytes-len(base))...)
	if code, rr := post(at); code == 413 {
		t.Errorf("at the byte cap: %d %+v", code, rr)
	}
	if code, rr := post(append(at, ' ')); code != 413 || rr.Reason != journalapi.ReasonTooLarge {
		t.Errorf("over the byte cap: %d %+v", code, rr)
	}
}

// The widest legitimate note fits the byte cap: what_happened at the text
// limit with every byte \u-escaped (6 bytes each), the two strings likewise.
func TestWidestNoteFitsTheByteCap(t *testing.T) {
	r := newRig(t)
	esc := func(n int) string { return strings.Repeat(`\u0041`, n) }
	body := `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "fields": {` +
		`"what_happened": "` + esc(record.MaxTextBytes) + `", "heard_from": "` + esc(record.MaxStringBytes) +
		`", "explained_to_school": "` + esc(record.MaxStringBytes) + `"}}`
	if len(body) > maxNoteBytes {
		t.Fatalf("the widest note is %d bytes", len(body))
	}
	var resp journalapi.NoteResponse
	if code := r.do("POST", journalapi.PathNotes, "gw", body, &resp); code != 200 || resp.Status != journalapi.NoteCreated {
		t.Errorf("%d %+v", code, resp)
	}
}

// No refusal of a note echoes a value it was sent.
func TestNoteRefusalsDoNotEchoValues(t *testing.T) {
	r := newRig(t)
	for name, b := range map[string]string{
		"record_type":     `{"record_type": "secretvalue", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1"}`,
		"date":            `{"record_type": "attendance_note", "date": "SECRETVALUE", "data_subject": "e_child1", "on_behalf_of": "e_parent1"}`,
		"subject":         `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_SECRETVALUE", "on_behalf_of": "e_parent1"}`,
		"bad subject":     `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "SECRET\u0001VALUE", "on_behalf_of": "e_parent1"}`,
		"on_behalf_of":    `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_SECRETVALUE"}`,
		"field value":     `{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1", "fields": {"heard_from": ["SECRETVALUE"]}}`,
		"unknown key":     `{"record_type": "attendance_note", "SECRETVALUE": 1}`,
		"not a household": `{"record_type": "secretvalue_type", "date": "2026-10-14", "data_subject": "e_child1", "on_behalf_of": "e_parent1"}`,
		"policy":          noteBody("e_SECRETVALUE", "e_child1", "SECRETVALUE"),
	} {
		code, raw := r.send("POST", journalapi.PathNotes, "gw", "application/json", b)
		if code/100 != 4 || strings.Contains(strings.ToLower(string(raw)), "secretvalue") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	code, raw := r.send("POST", journalapi.PathNotes, "nw", "application/json", noteBody("e_parent1", "e_child1", "SECRETVALUE"))
	if code != 403 || strings.Contains(string(raw), "SECRETVALUE") || strings.Contains(string(raw), "e_parent1") {
		t.Errorf("note writer: %d %s", code, raw)
	}
}

// A database failure is a 500 that says nothing about it.
func TestNoteDatabaseFailureIs500(t *testing.T) {
	r := newAuthRig(t)
	code, raw := r.send("POST", journalapi.PathNotes, "gw", "application/json", noteBody("e_parent1", "e_child1", "fever"))
	var rr journalapi.Response
	_ = json.Unmarshal(raw, &rr)
	if code != 500 || rr.Reason != journalapi.ReasonInternal || rr.Detail != "" {
		t.Errorf("%d %s", code, raw)
	}
	if !strings.Contains(r.scrape(), `code="500",endpoint="notes"`) {
		t.Error("the 500 was not counted under notes")
	}
}

// A service that writes no household type is refused before its body is
// read or decoded.
func TestNoteWriterCheckComesFirst(t *testing.T) {
	r := newAuthRig(t)
	for _, tok := range []string{"dash", "ps", "ro", "co", "admin"} {
		code, raw := r.send("POST", journalapi.PathNotes, tok, "text/plain", "not json at all")
		var rr journalapi.Response
		_ = json.Unmarshal(raw, &rr)
		if code != 403 || rr.Reason != journalapi.ReasonPolicy {
			t.Errorf("%s: %d %s", tok, code, raw)
		}
	}
}
