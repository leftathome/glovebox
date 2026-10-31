package powerschool

import (
	"reflect"
	"strings"
	"testing"

	pslib "github.com/leftathome/powerschool-go"
)

func TestCourseMarksOneScopePerCourse(t *testing.T) {
	g1 := sampleGrade("c1", "EXAMPLE SCIENCE 7", "B+88")
	g1.Q2Grade = "A-91"
	g2 := sampleGrade("", "EXAMPLE  ART\t", "") // no frn: name|period key; no grade posted
	b := Binding{DataSubject: "e_child1", Audience: []string{"subject", "guardians"}}
	snaps := BuildCourseMarks(b, []*pslib.Grade{g1, nil, g2}, obsAt)
	validateAll(t, snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	s := snaps[0]
	if !s.Complete || s.CompleteRange != nil || len(s.Records) != 1 || s.Records[0].Key != "c1" {
		t.Fatalf("snapshot %+v", s)
	}
	if s.RecordType != TypeCourseMark || s.SchemaVersion != 1 || !s.ObservedAt.Equal(obsAt) {
		t.Fatalf("snapshot header %+v", s)
	}
	wantScope(t, s, "powerschool", "grades", "e_child1", "course", "c1")
	r := s.Records[0]
	if r.DataSubject != "e_child1" || !reflect.DeepEqual(r.Audience, []string{"subject", "guardians"}) {
		t.Fatalf("record subject %q audience %v", r.DataSubject, r.Audience)
	}
	f := fieldsOf(t, s, "c1")
	wantJSON(t, f, map[string]string{"q1_letter": `"B+"`, "q1_percent": "88", "q2_letter": `"A-"`, "q2_percent": "91",
		"s1_letter": "null", "s1_percent": "null", "course": `"EXAMPLE SCIENCE 7"`, "period": `"1(A)"`, "teacher": `"Teacher, Example"`})
	if snaps[1].Records[0].Key != courseKey(g2) {
		t.Fatal("fallback key must be courseKey (decision S7)")
	}
	wantJSON(t, fieldsOf(t, snaps[1], courseKey(g2)), map[string]string{"course": `"EXAMPLE ART"`, "q1_letter": "null", "q1_percent": "null"})
}

// A cell that is neither letter nor percent keeps its raw text as the
// letter (splitGrade); a percent alone has a null letter.
func TestCourseMarksOddCells(t *testing.T) {
	g := sampleGrade("c1", "EXAMPLE SCIENCE 7", "--")
	g.Q2Grade = "93"
	g.S1Grade = "P"
	snaps := BuildCourseMarks(bind1, []*pslib.Grade{g}, obsAt)
	validateAll(t, snaps)
	wantJSON(t, fieldsOf(t, snaps[0], "c1"), map[string]string{"q1_letter": `"--"`, "q1_percent": "null",
		"q2_letter": "null", "q2_percent": "93", "s1_letter": `"P"`, "s1_percent": "null"})
}

func TestAttendanceTotalsEveryCourseIncludingZero(t *testing.T) {
	g := sampleGrade("c1", "EXAMPLE SCIENCE 7", "B+88")
	g2 := sampleGrade("c2", "EXAMPLE ART", "")
	g2.Absences, g2.Tardies = 3, 1
	snaps := BuildAttendanceTotals(bind1, []*pslib.Grade{g, g2}, obsAt)
	validateAll(t, snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	wantJSON(t, fieldsOf(t, snaps[0], "c1"), map[string]string{"absences": "0", "tardies": "0", "course": `"EXAMPLE SCIENCE 7"`})
	wantJSON(t, fieldsOf(t, snaps[1], "c2"), map[string]string{"absences": "3", "tardies": "1"})
	wantScope(t, snaps[0], "powerschool", "attendance_totals", "e_child1", "course", "c1")
	if !snaps[0].Complete || snaps[0].RecordType != TypeAttendanceTotal {
		t.Fatalf("snapshot %+v", snaps[0])
	}
}

func TestCourseKeyWithControlCharactersIsHashedConsistently(t *testing.T) {
	g := sampleGrade("", "EXAMPLE\nSCIENCE", "B")
	a := BuildCourseMarks(bind1, []*pslib.Grade{g}, obsAt)
	b := BuildAttendanceTotals(bind1, []*pslib.Grade{g}, obsAt)
	validateAll(t, append(a, b...))
	if a[0].Records[0].Key != b[0].Records[0].Key || !strings.HasPrefix(a[0].Records[0].Key, "h:") {
		t.Fatal("same course, same safe key on both surfaces")
	}
	if a[0].Scope[3][1] != a[0].Records[0].Key {
		t.Fatalf("scope course %q, key %q", a[0].Scope[3][1], a[0].Records[0].Key)
	}
	if CourseRecordKey(g) != a[0].Records[0].Key {
		t.Fatal("CourseRecordKey is the record key")
	}
}

// Two rows with one course key on a page: the first is kept, the second
// counted, so the scope is not sent twice.
func TestCourseMarksDuplicateKeyKeepsTheFirst(t *testing.T) {
	g1 := sampleGrade("c1", "EXAMPLE SCIENCE 7", "B+88")
	g2 := sampleGrade("c1", "EXAMPLE SCIENCE 7 AGAIN", "C70")
	var skipped []string
	b := bind1
	b.OnSkip = func(reason string) { skipped = append(skipped, reason) }
	snaps := BuildCourseMarks(b, []*pslib.Grade{g1, g2}, obsAt)
	validateAll(t, snaps)
	if len(snaps) != 1 || string(fieldsOf(t, snaps[0], "c1")["q1_letter"]) != `"B+"` {
		t.Fatalf("snapshots %+v", snaps)
	}
	tot := BuildAttendanceTotals(b, []*pslib.Grade{g1, g2}, obsAt)
	if len(tot) != 1 || !reflect.DeepEqual(skipped, []string{"duplicate_key", "duplicate_key"}) {
		t.Fatalf("totals %d, skipped %v", len(tot), skipped)
	}
}

func TestGradeBuildersNoRowsNoSnapshots(t *testing.T) {
	if n := len(BuildCourseMarks(bind1, nil, obsAt)) + len(BuildAttendanceTotals(bind1, []*pslib.Grade{nil}, obsAt)); n != 0 {
		t.Fatalf("%d snapshots from no rows", n)
	}
}
