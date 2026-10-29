package powerschool

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	pslib "github.com/leftathome/powerschool-go"
)

// Spec: "parity tests showing that records and today's items agree", on
// the item fixtures, through one real poll.

// surfaceTypes maps each item surface to the record type that covers it.
var surfaceTypes = map[string]string{
	gradeSurface:          TypeCourseMark,
	attendanceSurface:     TypeAttendanceTotal,
	assignmentSurface:     TypeAssignment,
	attendanceDaySurface:  TypeAttendanceMark,
	termGradeSurface:      TypeTermGrade,
	teacherCommentSurface: TypeTermComment,
}

// checkpointIDs are the ids the item path wrote for kid on surface.
func checkpointIDs(t *testing.T, cp *memCheckpoint, surface, kid string) []string {
	t.Helper()
	var out []string
	for k := range checkpointMap(t, cp) {
		if id, ok := strings.CutPrefix(k, surface+":"+kid+":"); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func recordKeys(snaps []journalapi.Snapshot, ty, subject string) []string {
	var out []string
	for _, s := range snaps {
		if s.RecordType != ty || subjectOfScope(s) != subject {
			continue
		}
		out = append(out, keysOfSnap(s)...)
	}
	sort.Strings(out)
	return out
}

func parityPoll(t *testing.T, client *fakeClient) ([]stagedItem, *memCheckpoint, []journalapi.Snapshot) {
	t.Helper()
	sink := &recorderSink{}
	items, cp, _ := pollWithSink(t, client, recordsConfig(), sink)
	return items, cp.(*memCheckpoint), sink.all()
}

// On the item fixtures every surface's record keys are exactly the item
// path's checkpoint ids.
func TestParityRecordKeysAreTheCheckpointIDs(t *testing.T) {
	_, cp, snaps := parityPoll(t, extraFixture("111111"))
	for kid, subject := range map[string]string{"k1": "e_child1", "k2": "e_child2"} {
		for surface, ty := range surfaceTypes {
			ids := checkpointIDs(t, cp, surface, kid)
			keys := recordKeys(snaps, ty, subject)
			if len(ids) == 0 || !reflect.DeepEqual(ids, keys) {
				t.Errorf("%s %s: checkpoint ids %v, record keys %v", kid, surface, ids, keys)
			}
		}
	}
}

// Records cover a superset: a course with no grade posted and no absence
// stages no item and is still a record.
func TestParityRecordsAreASuperset(t *testing.T) {
	client := extraFixture("111111")
	home := client.HomeFunc
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) {
		p, err := home(ctx)
		if err == nil {
			g := sampleGrade("00200", "EXAMPLE ART", "")
			g.StudentID = client.Selected
			p.Grades = append(p.Grades, g)
		}
		return p, err
	}
	_, cp, snaps := parityPoll(t, client)
	for _, surface := range []string{gradeSurface, attendanceSurface} {
		ids := checkpointIDs(t, cp, surface, "k1")
		keys := recordKeys(snaps, surfaceTypes[surface], "e_child1")
		if !reflect.DeepEqual(ids, []string{"00100"}) || !reflect.DeepEqual(keys, []string{"00100", "00200"}) {
			t.Errorf("%s: ids %v keys %v", surface, ids, keys)
		}
	}
}

// str is a field's value as text: a JSON string unquoted, null empty,
// anything else as written; then one line.
func str(raw json.RawMessage) string {
	if string(raw) == "null" || raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return records.OneLine(s)
	}
	return string(raw)
}

// findRecord returns the record of type ty about subject whose fields
// match want (field -> one-line text).
func findRecord(snaps []journalapi.Snapshot, ty, subject string, want map[string]string) (journalapi.Record, bool) {
	for _, s := range snaps {
		if s.RecordType != ty || subjectOfScope(s) != subject {
			continue
		}
	next:
		for _, r := range s.Records {
			for k, v := range want {
				if str(r.Fields[k]) != records.OneLine(v) {
					continue next
				}
			}
			return r, true
		}
	}
	return journalapi.Record{}, false
}

// For each staged item, the record with its key carries the same facts as
// the item's tags, and the same subject and audience.
func TestParityRecordFactsMatchTheItemTags(t *testing.T) {
	items, _, snaps := parityPoll(t, extraFixture("111111"))
	if len(items) == 0 {
		t.Fatal("no items")
	}
	for _, it := range items {
		tags := it.Meta.Tags
		surface := tags["surface"]
		ty := surfaceTypes[surface]
		var find map[string]string
		var same map[string]string // record field -> item tag
		switch surface {
		case gradeSurface:
			find = map[string]string{"course": tags["course"]}
		case attendanceSurface:
			find = map[string]string{"course": tags["course"]}
			same = map[string]string{"absences": "absences", "tardies": "tardies"}
		case assignmentSurface:
			find = map[string]string{"title": it.Meta.Subject}
			same = map[string]string{"score": "score", "max_score": "max_score", "percent": "percent", "letter": "letter_grade",
				"category": "category", "due": "due_day", "missing": "missing", "late": "late", "incomplete": "incomplete",
				"exempt": "exempt", "collected": "collected", "absent": "absent"}
		case attendanceDaySurface:
			find = map[string]string{"date": tags["date"], "course": tags["course"]}
			same = map[string]string{"code": "code", "kind": "kind", "code_description": "code_description", "period": "period"}
		case termGradeSurface:
			find = map[string]string{"course": tags["course"], "term": tags["term"]}
			same = map[string]string{"grade": "grade", "earned": "credit_earned", "attempted": "credit_attempted"}
		case teacherCommentSurface:
			find = map[string]string{"course": tags["course"], "term": tags["term"]}
		default:
			t.Fatalf("item of surface %q", surface)
		}
		r, ok := findRecord(snaps, ty, it.Meta.DataSubject, find)
		if !ok {
			t.Errorf("%s item %q: no %s record matching %v", surface, it.Meta.Subject, ty, find)
			continue
		}
		for field, tag := range same {
			if got, want := str(r.Fields[field]), records.OneLine(tags[tag]); got != want {
				t.Errorf("%s item %q: record %s = %q, tag %s = %q", surface, it.Meta.Subject, field, got, tag, want)
			}
		}
		if surface == gradeSurface {
			for _, term := range termOrder {
				l, p := splitGrade(tags["grade_"+strings.ToLower(term)])
				name := strings.ToLower(term)
				if str(r.Fields[name+"_letter"]) != l || str(r.Fields[name+"_percent"]) != p {
					t.Errorf("grade %s: record %s/%s, tag %q", term, r.Fields[name+"_letter"], r.Fields[name+"_percent"], tags["grade_"+name])
				}
			}
		}
		if r.DataSubject != it.Meta.DataSubject || !reflect.DeepEqual(r.Audience, it.Meta.Audience) {
			t.Errorf("%s item %q: record subject %q audience %v, item %q %v", surface, it.Meta.Subject,
				r.DataSubject, r.Audience, it.Meta.DataSubject, it.Meta.Audience)
		}
	}
}
