package schoology

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// jsonString decodes a string field ("" for null).
func jsonString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %s: %v", raw, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

// bodyLine returns the value of the item body's "<name>: " line.
func bodyLine(body, name string) string {
	for _, l := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(l, name+": "); ok {
			return v
		}
	}
	return ""
}

// recordOf finds the record with key in the snapshot of subject and type.
func recordOf(t *testing.T, snaps []journalapi.Snapshot, subject, recordType, key string) (journalapi.Record, bool) {
	t.Helper()
	for _, s := range snaps {
		if s.RecordType != recordType || s.Scope[2][1] != subject {
			continue
		}
		for _, r := range s.Records {
			if r.Key == key {
				return r, true
			}
		}
	}
	return journalapi.Record{}, false
}

// Spec "parity tests showing that records and today's items agree": every
// item staged from the listing has a record with the same facts, every
// detail item staged from a clean read has a record, and both carry the
// item's data_subject and audience. Records are a superset: rows already
// in the seen set are records too.
func TestRecordsAgreeWithTheItems(t *testing.T) {
	sink := &recorderSink{}
	run := newRecordsRun(t, t.TempDir(), true, sink)
	if err := run.poll(t); err != nil {
		t.Fatal(err)
	}
	// Poll 2: one new row for k1; the others are already seen.
	extra := listRow(9100000013, "EXAMPLE Field Notes", "EXAMPLE SCIENCE 7")
	run.fixture.rows[1001] = append(run.fixture.rows[1001], extra)
	run.fixture.details[extra.ID] = regularDetail(extra.ID, extra.Title, extra.CourseTitle)
	run.now = run.now.Add(time.Hour)
	before := len(run.items(t))
	sink.mu.Lock()
	sink.snaps = nil
	sink.mu.Unlock()
	if err := run.poll(t); err != nil {
		t.Fatal(err)
	}
	snaps := sink.all()
	validateAll(t, snaps)
	if staged := len(run.items(t)) - before; staged != 2 {
		t.Fatalf("poll 2 staged %d items, want the new row and its detail", staged)
	}
	// Superset: k1's listing holds all three rows, though only one was
	// staged in this poll.
	if _, ok := recordOf(t, snaps, "e_child1", TypeLMSAssignment, "9100000011"); !ok {
		t.Fatal("a row already in the seen set is not a record")
	}

	assignments, details := 0, 0
	for _, it := range run.items(t) {
		m := it.meta
		switch {
		case m.Tags["kind"] == detailSurface:
			details++
			rec, ok := recordOf(t, snaps, m.DataSubject, TypeLMSAssignmentDetail, m.Tags["assignment_id"])
			if !ok {
				// Staged in poll 1 and not re-read in poll 2 (refresh not
				// due): it was a record in poll 1.
				continue
			}
			if rec.DataSubject != m.DataSubject || !reflect.DeepEqual(rec.Audience, m.Audience) {
				t.Errorf("detail %s: record %s %v, item %s %v", m.Tags["assignment_id"], rec.DataSubject, rec.Audience, m.DataSubject, m.Audience)
			}
		case m.Source == "schoology":
			assignments++
			link := bodyLine(it.content, "Link")
			id := strings.TrimPrefix(link, "/assignment/")
			rec, ok := recordOf(t, snaps, m.DataSubject, TypeLMSAssignment, id)
			if !ok {
				t.Fatalf("assignment item %s (%s) has no record in poll 2", id, m.DataSubject)
			}
			f := rec.Fields
			got := map[string]string{
				"title": jsonString(t, f["title"]), "course": jsonString(t, f["course"]),
				"status": jsonString(t, f["status"]), "due": jsonString(t, f["due"]), "link": jsonString(t, f["link"]),
			}
			want := map[string]string{
				"title": m.Subject, "course": m.Tags["course"], "status": m.Tags["status"],
				"due": m.Tags["due_date"], "link": link,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("assignment %s: record %v, item %v", id, got, want)
			}
			if rec.DataSubject != m.DataSubject || !reflect.DeepEqual(rec.Audience, m.Audience) {
				t.Errorf("assignment %s: record %s %v, item %s %v", id, rec.DataSubject, rec.Audience, m.DataSubject, m.Audience)
			}
		}
	}
	if assignments != 4 || details != 4 {
		t.Fatalf("%d assignment and %d detail items, want 4 and 4", assignments, details)
	}
	// The new row's detail was read in poll 2: its record is there.
	if _, ok := recordOf(t, snaps, "e_child1", TypeLMSAssignmentDetail, "9100000013"); !ok {
		t.Fatal("the new row's detail is not a record")
	}
}
