package powerschool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// assignmentReads is two courses: c1 with a recent assignment, one far
// outside every lookback and a shared-section id repeated; c2 with one.
func assignmentReads() []courseRead {
	recent := sampleAssignment("5001", "Lab report")
	old := sampleAssignment("5002", "Old quiz")
	old.DueDate = due(2025, 9, 2)
	old.Score, old.Status = floatPtr(9), pslib.StatusGraded
	shared1 := sampleAssignment("5003", "Shared section work")
	shared2 := sampleAssignment("5003", "Shared section work")
	other := sampleAssignment("6001", "Art sketch")
	return []courseRead{
		{grade: sampleGrade("c1", "EXAMPLE SCIENCE 7", "B+88"), assignments: []*pslib.Assignment{recent, nil, old, shared1, shared2}},
		{grade: sampleGrade("c2", "EXAMPLE ART", ""), assignments: []*pslib.Assignment{other}},
	}
}

func TestAssignmentsOneSnapshotPerCourseRead(t *testing.T) {
	snaps := BuildAssignments(bind1, assignmentReads(), testTZ(t), obsAt, nil)
	validateAll(t, snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	s := snaps[0]
	if s.RecordType != TypeAssignment || !s.Complete || s.CompleteRange != nil {
		t.Fatalf("snapshot %+v", s)
	}
	wantScope(t, s, "powerschool", "assignments", "e_child1", "course", "c1")
	if got := strings.Join(keysOfSnap(s), ","); got != "c1:5001,c1:5002,c1:5003,c1:5003#2" {
		t.Fatalf("keys %s", got)
	}
	// Outside the lookback, and still a record (spec 5.1).
	wantJSON(t, fieldsOf(t, s, "c1:5002"), map[string]string{"due": `"2025-09-02"`, "score": "9", "max_score": "10", "title": `"Old quiz"`})
	wantJSON(t, fieldsOf(t, s, "c1:5001"), map[string]string{"due": `"2026-10-08"`, "score": "null", "max_score": "10",
		"percent": "null", "missing": "false", "absent": "false", "course": `"EXAMPLE SCIENCE 7"`, "category": `"Classwork"`,
		"letter": "null", "teacher_comment": "null", "description": "null", "category_description": "null"})
	if AssignmentRecordKey(sampleGrade("c1", "", ""), "5003", 2) != "c1:5003#2" {
		t.Fatal("AssignmentRecordKey")
	}
}

func TestAssignmentsZeroReadsZeroSnapshots(t *testing.T) {
	if n := len(BuildAssignments(bind1, nil, testTZ(t), obsAt, nil)); n != 0 {
		t.Fatalf("%d snapshots", n)
	}
	// A course read with no assignments is a complete, empty view of it.
	snaps := BuildAssignments(bind1, []courseRead{{grade: sampleGrade("c1", "EXAMPLE SCIENCE 7", "")}}, testTZ(t), obsAt, nil)
	validateAll(t, snaps)
	if len(snaps) != 1 || len(snaps[0].Records) != 0 {
		t.Fatalf("snapshots %+v", snaps)
	}
}

func TestAssignmentScoresAndFlags(t *testing.T) {
	zero := sampleAssignment("1", "Entered zero")
	zero.Status = pslib.StatusGraded // the library leaves Score nil for an entered 0
	unscored := sampleAssignment("2", "No points")
	unscored.MaxScore = 0
	flagged := sampleAssignment("3", "Flags")
	flagged.Flags = []pslib.AssignmentFlag{pslib.FlagMissing, pslib.FlagLate, pslib.FlagIncomplete, pslib.FlagExempt, pslib.FlagCollected, pslib.FlagAbsent}
	flagged.Score, flagged.Percentage, flagged.LetterGrade = floatPtr(7.5), floatPtr(75), "C"
	undated := sampleAssignment("4", "Undated")
	undated.DueDate = time.Time{}
	snaps := BuildAssignments(bind1, []courseRead{{grade: sampleGrade("c1", "EXAMPLE SCIENCE 7", ""),
		assignments: []*pslib.Assignment{zero, unscored, flagged, undated}}}, testTZ(t), obsAt, nil)
	validateAll(t, snaps)
	s := snaps[0]
	wantJSON(t, fieldsOf(t, s, "c1:1"), map[string]string{"score": "0", "max_score": "10"})
	wantJSON(t, fieldsOf(t, s, "c1:2"), map[string]string{"score": "null", "max_score": "null"})
	wantJSON(t, fieldsOf(t, s, "c1:3"), map[string]string{"score": "7.5", "percent": "75", "letter": `"C"`,
		"missing": "true", "late": "true", "incomplete": "true", "exempt": "true", "collected": "true", "absent": "true"})
	wantJSON(t, fieldsOf(t, s, "c1:4"), map[string]string{"due": "null"})
}

// Text fields carry what the library returned: no cleaning, no 4 KiB cut,
// no marker.
func TestAssignmentTextArrivesUnchanged(t *testing.T) {
	a := sampleAssignment("5001", "Lab report")
	comment := "Good work.\r\n\tShow the  units next time.‮ reversed"
	a.TeacherComment, a.HasTeacherComment = comment, true
	a.Description = strings.Repeat("d", 5000) + "\n\nend"
	a.CategoryDescription = "Work done in class."
	same := sampleAssignment("5002", "Same text")
	same.Description, same.CategoryDescription = "Shared text.", "Shared text."
	stray := sampleAssignment("5003", "Stray comment")
	stray.TeacherComment = "stray" // without the library's flag: no comment
	snaps := BuildAssignments(bind1, []courseRead{{grade: sampleGrade("c1", "EXAMPLE SCIENCE 7", ""),
		assignments: []*pslib.Assignment{a, same, stray}}}, testTZ(t), obsAt, nil)
	validateAll(t, snaps)
	f := fieldsOf(t, snaps[0], "c1:5001")
	var got string
	if err := json.Unmarshal(f["teacher_comment"], &got); err != nil || got != comment {
		t.Fatalf("teacher_comment %q (%v)", got, err)
	}
	if err := json.Unmarshal(f["description"], &got); err != nil || got != a.Description {
		t.Fatalf("description cut or changed: %d bytes", len(got))
	}
	wantJSON(t, f, map[string]string{"category_description": `"Work done in class."`})
	wantJSON(t, fieldsOf(t, snaps[0], "c1:5002"), map[string]string{"description": `"Shared text."`, "category_description": "null"})
	wantJSON(t, fieldsOf(t, snaps[0], "c1:5003"), map[string]string{"teacher_comment": "null"})
}

func TestAssignmentLongDescriptionIsCutOnce(t *testing.T) {
	a := sampleAssignment("5001", "Lab report")
	a.Description = strings.Repeat("€", 70<<10/3+10) // about 70 KiB of 3-byte runes
	cuts := 0
	snaps := BuildAssignments(bind1, []courseRead{{grade: sampleGrade("c1", "EXAMPLE SCIENCE 7", ""),
		assignments: []*pslib.Assignment{a}}}, testTZ(t), obsAt, func() { cuts++ })
	validateAll(t, snaps)
	var got string
	if err := json.Unmarshal(fieldsOf(t, snaps[0], "c1:5001")["description"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got) > 64<<10 || len(got) < 64<<10-3 || cuts != 1 {
		t.Fatalf("description %d bytes, %d cuts", len(got), cuts)
	}
}

// A course whose snapshot would be refused 413 at every poll is left out,
// logged and counted.
func TestAssignmentSnapshotTooLargeIsNotEmitted(t *testing.T) {
	var many []*pslib.Assignment
	for i := 0; i < 140; i++ {
		a := sampleAssignment(fmt.Sprintf("a%d", i), "Big")
		a.Description = strings.Repeat("x", 60<<10)
		many = append(many, a)
	}
	var skipped []string
	b := bind1
	b.OnSkip = func(r string) { skipped = append(skipped, r) }
	logs := captureLogs(t)
	snaps := BuildAssignments(b, []courseRead{
		{grade: sampleGrade("c1", "EXAMPLE SCIENCE 7", ""), assignments: many},
		{grade: sampleGrade("c2", "EXAMPLE ART", ""), assignments: []*pslib.Assignment{sampleAssignment("1", "Small")}},
	}, testTZ(t), obsAt, nil)
	if len(snaps) != 1 || snaps[0].Scope[3][1] != "c2" {
		t.Fatalf("%d snapshots", len(snaps))
	}
	if len(skipped) != 1 || skipped[0] != "snapshot_too_large" || !strings.Contains(logs.String(), "record_type=assignment") {
		t.Fatalf("skipped %v; logs %s", skipped, logs.String())
	}
}

// The record keys are the ids the item path checkpoints for the same reads.
func TestAssignmentKeysEqualTheCheckpointIDs(t *testing.T) {
	env, _ := newTestEnv(t, &fakeClient{})
	reads := assignmentReads()
	stageAssignments(context.Background(), env, testKid, reads, AssignmentsConfig{LookbackDays: 30})
	snaps := BuildAssignments(bind1, reads, env.tz(), obsAt, nil)
	n := 0
	for _, s := range snaps {
		for _, r := range s.Records {
			n++
			if _, ok := env.CP.Load(CheckpointKey(assignmentSurface, "k1", r.Key)); !ok {
				t.Errorf("record %q has no checkpoint id", r.Key)
			}
		}
	}
	if n != 5 {
		t.Fatalf("%d records, want 5", n)
	}
}
