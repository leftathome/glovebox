package journal

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

const gateway = "openclaw/openclaw-gateway"

func (r *rig) note(onBehalfOf, subject, date, fields string) (journalapi.NoteResponse, *record.Rejection) {
	r.t.Helper()
	var f map[string]json.RawMessage
	_ = json.Unmarshal([]byte(fields), &f)
	n, rej := record.ValidateNote(r.reg, journalapi.Note{RecordType: "attendance_note", Date: date,
		DataSubject: subject, Fields: f, OnBehalfOf: onBehalfOf})
	if rej != nil {
		return journalapi.NoteResponse{}, rej
	}
	resp, rej, err := r.j.WriteNote(ctx, gateway, n)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp, rej
}

func TestNotesCreateUpdateAndStayPerWriter(t *testing.T) {
	r := newRig(t)
	resp, rej := r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "fever", "heard_from": "subject"}`)
	if rej != nil || resp.Status != "created" || resp.Key != "2026-10-14:e_parent1" || resp.Seq == 0 {
		t.Fatalf("%+v %v", resp, rej)
	}
	ev := r.events("e_child1")[0]
	if ev.Actor != gateway+";on_behalf_of=e_parent1" || ev.SnapshotID.Valid || len(ev.Audience) != 2 ||
		ev.Scope != "source=household/record_type=attendance_note/data_subject=e_child1" {
		t.Errorf("event %+v", ev)
	}
	r.clock.Set(T0.Add(time.Hour))
	if resp, _ := r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "fever", "heard_from": "subject"}`); resp.Status != "unchanged" {
		t.Errorf("same note again: %+v", resp)
	}
	resp, _ = r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "fever, back Thursday", "heard_from": "subject"}`)
	if resp.Status != "updated" {
		t.Errorf("own correction: %+v", resp)
	}
	upd := r.events("e_child1")[1]
	if upd.Kind != "updated" || !upd.PrevObserved.Valid || !upd.PrevObserved.Time.Equal(T0) {
		t.Errorf("update %+v", upd)
	}
	resp, _ = r.note("e_child1", "e_child1", "2026-10-14", `{"what_happened": "I was sick"}`)
	if resp.Status != "created" || resp.Key != "2026-10-14:e_child1" {
		t.Errorf("the subject's own note is a separate record: %+v", resp)
	}
	if c, _ := r.current("attendance_note", "e_child1", "2026-10-14:e_parent1"); string(c.Fields["what_happened"]) != `"fever, back Thursday"` {
		t.Error("someone else's note changed the parent's")
	}
}

func TestNoteWriteNeedsAGrantForTheFixedAudience(t *testing.T) {
	r := newRig(t)
	if _, rej := r.note("e_guest1", "e_child1", "2026-10-14", `{}`); rej == nil || rej.Status != 403 {
		t.Errorf("a guest wrote about a child: %v", rej)
	}
	if _, rej := r.note("e_child2", "e_child1", "2026-10-14", `{}`); rej == nil || rej.Status != 403 {
		t.Errorf("a sibling wrote (siblings is not in the fixed audience): %v", rej)
	}
	if _, rej := r.note("e_nobody", "e_child1", "2026-10-14", `{}`); rej == nil || rej.Status != 403 {
		t.Errorf("a non-member wrote: %v", rej)
	}
	if _, rej := r.note("e_parent1", "e_stranger", "2026-10-14", `{}`); rej == nil || rej.Status != 409 {
		t.Errorf("a note about a subject outside the roster: %v", rej)
	}
}

func TestNoteTextIsScanned(t *testing.T) {
	r := newRig(t)
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "INJECT"}`)
	if c, _ := r.current("attendance_note", "e_child1", "2026-10-14:e_parent1"); c.Scan["what_happened"].Verdict != "quarantine" {
		t.Errorf("scan %+v", c.Scan)
	}
}

func (r *rig) eraseAt(subject string, at time.Time) {
	r.t.Helper()
	if _, err := r.j.db.Exec(`INSERT INTO erasure (data_subject, erased_at, actor, rows) VALUES ($1, $2, 'test', '{}')`, subject, at); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) eventCount(kind string) int {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	n := 0
	for k, v := range r.rec.events {
		if strings.HasPrefix(k, kind+"/") {
			n += v
		}
	}
	return n
}

func TestNoteRejectionReasonsAndNothingWritten(t *testing.T) {
	r := newRig(t)
	_, rej := r.note("e_guest1", "e_child1", "2026-10-14", `{}`)
	if rej == nil || rej.Status != 403 || rej.Reason != journalapi.ReasonPolicy {
		t.Errorf("no grant: %v", rej)
	}
	_, rej = r.note("e_nobody", "e_child1", "2026-10-14", `{}`)
	if rej == nil || rej.Status != 403 || rej.Reason != journalapi.ReasonPolicy {
		t.Errorf("non-member: %v", rej)
	}
	_, rej = r.note("e_parent1", "e_stranger", "2026-10-14", `{}`)
	if rej == nil || rej.Status != 409 || rej.Reason != journalapi.ReasonUnknownSubject {
		t.Errorf("unknown subject: %v", rej)
	}
	if r.count("record_event") != 0 || r.count("record_current") != 0 || r.eventCount("created") != 0 {
		t.Error("a refused note left state or metrics")
	}
}

func TestNoteGrantedWritersAndFixedAudience(t *testing.T) {
	r := newRig(t)
	for i, w := range []string{"e_parent1", "e_parent2", "e_child1"} {
		resp, rej := r.note(w, "e_child1", "2026-10-14", `{"what_happened": "x"}`)
		if rej != nil || resp.Status != "created" {
			t.Fatalf("%s: %+v %v", w, resp, rej)
		}
		ev := r.events("e_child1")[i]
		if !reflect.DeepEqual(ev.Audience, []string{"guardians", "subject"}) || ev.Kind != "created" ||
			ev.Actor != gateway+";on_behalf_of="+w {
			t.Errorf("%s: event %+v", w, ev)
		}
		c, ok := r.current("attendance_note", "e_child1", "2026-10-14:"+w)
		if !ok || !reflect.DeepEqual(c.Audience, []string{"guardians", "subject"}) {
			t.Errorf("%s: current %+v", w, c)
		}
	}
	if r.eventCount("created") != 3 {
		t.Errorf("created metric %d", r.eventCount("created"))
	}
}

func TestNoteWithoutARosterIsRefused(t *testing.T) {
	r := newRig(t)
	r.ros.Store(nil)
	_, rej := r.note("e_parent1", "e_child1", "2026-10-14", `{}`)
	if rej == nil || rej.Status != 409 || rej.Reason != journalapi.ReasonUnknownSubject {
		t.Errorf("no roster: %v", rej)
	}
	if r.count("record_event") != 0 {
		t.Error("a note was written without a roster")
	}
}

func TestNoteErasureGuard(t *testing.T) {
	r := newRig(t)
	r.eraseAt("e_child2", T0) // another subject's erasure does not matter
	if _, rej := r.note("e_parent1", "e_child1", "2026-10-14", `{}`); rej != nil {
		t.Fatalf("unrelated erasure refused a note: %v", rej)
	}
	r.eraseAt("e_child1", T0.Add(-record.MaxFutureSkew+time.Microsecond))
	_, rej := r.note("e_parent1", "e_child1", "2026-10-15", `{}`)
	if rej == nil || rej.Status != 410 || rej.Reason != journalapi.ReasonErasedSubject {
		t.Errorf("inside the erasure skew window: %v", rej)
	}
	if _, ok := r.current("attendance_note", "e_child1", "2026-10-15:e_parent1"); ok || r.count("record_event") != 1 {
		t.Error("a refused note was stored")
	}
	r.clock.Set(T0.Add(time.Microsecond)) // now == erased_at + MaxFutureSkew
	if resp, rej := r.note("e_parent1", "e_child1", "2026-10-15", `{}`); rej != nil || resp.Status != "created" {
		t.Errorf("exactly erased_at + skew: %+v %v", resp, rej)
	}
}

func TestNoteScanVerdictsBothSides(t *testing.T) {
	r := newRig(t)
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "fine", "heard_from": "INJECT"}`)
	c, _ := r.current("attendance_note", "e_child1", "2026-10-14:e_parent1")
	if c.Scan["what_happened"].Verdict != "pass" {
		t.Errorf("clean text: %+v", c.Scan)
	}
	if _, ok := c.Scan["heard_from"]; ok {
		t.Errorf("a non-text field was scanned: %+v", c.Scan)
	}
	if r.rec.withheld != 0 {
		t.Errorf("withheld metric %d", r.rec.withheld)
	}
	r.clock.Set(T0.Add(time.Hour))
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "INJECT", "heard_from": "INJECT"}`)
	c, _ = r.current("attendance_note", "e_child1", "2026-10-14:e_parent1")
	if c.Scan["what_happened"].Verdict != "quarantine" || r.rec.withheld != 1 {
		t.Errorf("update not scanned: %+v withheld %d", c.Scan, r.rec.withheld)
	}
}

func TestNoteScanErrorFailsClosed(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Scanner = errScanner{} })
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "hello"}`)
	c, _ := r.current("attendance_note", "e_child1", "2026-10-14:e_parent1")
	if c.Scan["what_happened"].Verdict != record.VerdictError || r.rec.withheld != 1 {
		t.Errorf("scan error: %+v withheld %d", c.Scan, r.rec.withheld)
	}
}

type errScanner struct{}

func (errScanner) Verdict(string) string { return record.VerdictError }
func (errScanner) Version() string       { return "err-1" }

func TestNoteUnchangedWritesNothingAndUpdateMetric(t *testing.T) {
	r := newRig(t)
	r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	r.clock.Set(T0.Add(time.Hour))
	resp, _ := r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "a"}`)
	if resp.Status != "unchanged" || resp.Seq != 0 || resp.Key != "2026-10-14:e_parent1" || len(r.events("e_child1")) != 1 {
		t.Errorf("unchanged: %+v", resp)
	}
	resp, _ = r.note("e_parent1", "e_child1", "2026-10-14", `{"what_happened": "b"}`)
	evs := r.events("e_child1")
	if resp.Status != "updated" || resp.Seq != evs[1].Seq || r.eventCount("updated") != 1 {
		t.Errorf("update: %+v", resp)
	}
}
