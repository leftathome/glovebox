package powerschool

import (
	"context"
	"encoding/json"
	"testing"

	pslib "github.com/leftathome/powerschool-go"
)

var guardiansBinding = Binding{DataSubject: "e_child1", Audience: []string{"guardians"}}

func TestTermGradesKeysEqualTheCheckpointIDs(t *testing.T) {
	env, _ := newTestEnv(t, &fakeClient{})
	page := historyFixture("111111", "A-")
	repeat := *page.Grades[0]
	undated := &pslib.HistoricalGrade{CourseName: "EXAMPLE ART", LetterGrade: "P", CreditAttempted: 0.5, CreditEarned: 0.5}
	page.Grades = append(page.Grades, &repeat, nil, undated, &pslib.HistoricalGrade{CourseName: "NO GRADE"})
	ProcessTermGrades(context.Background(), env, testKid, page, TermGradesConfig{LookbackDays: 36500})
	snaps := BuildTermGrades(bind1, page, obsAt)
	validateAll(t, snaps)
	if len(snaps) != 1 || len(snaps[0].Records) != 3 {
		t.Fatalf("snapshots %+v", snaps)
	}
	s := snaps[0]
	wantScope(t, s, "powerschool", "term_grades", "e_child1")
	if !s.Complete || s.CompleteRange != nil || s.RecordType != TypeTermGrade {
		t.Fatalf("snapshot %+v", s)
	}
	for _, r := range s.Records {
		if _, ok := env.CP.Load(CheckpointKey(termGradeSurface, "k1", r.Key)); !ok {
			t.Errorf("record %q has no checkpoint id", r.Key)
		}
	}
	first := "SCI6001:2026-06:1|example es|6|example science 6"
	if s.Records[0].Key != first || s.Records[1].Key != first+"#2" {
		t.Fatalf("keys %v", keysOfSnap(s))
	}
	wantJSON(t, fieldsOf(t, s, first), map[string]string{"course": `"EXAMPLE SCIENCE 6"`, "term": `"2026-06"`,
		"grade": `"A-"`, "earned": "1", "attempted": "1"})
	wantJSON(t, fieldsOf(t, s, "name:example art:undated:0.5||0|example art"), map[string]string{"term": `"undated"`, "earned": "0.5"})
	if TermGradeRecordKey(page.Grades[0], 1) != first {
		t.Fatal("TermGradeRecordKey")
	}
}

func TestTermGradesNoPageNoSnapshot(t *testing.T) {
	if n := len(BuildTermGrades(bind1, nil, obsAt)); n != 0 {
		t.Fatalf("%d snapshots", n)
	}
}

func TestTermCommentsKeysEqualTheCheckpointIDs(t *testing.T) {
	env, _ := newTestEnv(t, &fakeClient{})
	page := commentsFixture("111111", "Q1", true, "Careful lab work.\r\n\tPlease  review unit 2.")
	ProcessTeacherComments(context.Background(), env, testKid, page, TeacherCommentsConfig{})
	snaps := BuildTermComments(guardiansBinding, []*pslib.TeacherCommentsPage{page}, obsAt, nil)
	validateAll(t, snaps)
	if len(snaps) != 1 || len(snaps[0].Records) != 1 {
		t.Fatalf("snapshots %+v", snaps) // the empty advisory comment is not a record
	}
	s := snaps[0]
	wantScope(t, s, "powerschool", "term_comments", "e_child1", "term", "Q1")
	r := s.Records[0]
	if r.Key != "SCI7001|1(A):Q1" || TermCommentRecordKey(page.Comments[1], "Q1", 1) != r.Key {
		t.Fatalf("key %q", r.Key)
	}
	if _, ok := env.CP.Load(CheckpointKey(teacherCommentSurface, "k1", r.Key)); !ok {
		t.Fatal("record key is not the checkpoint id")
	}
	if len(r.Audience) != 1 || r.Audience[0] != "guardians" {
		t.Fatalf("audience %v", r.Audience)
	}
	var got string
	if err := json.Unmarshal(r.Fields["comment"], &got); err != nil || got != "Careful lab work.\r\n\tPlease  review unit 2." {
		t.Fatalf("comment %q (%v)", got, err)
	}
	wantJSON(t, r.Fields, map[string]string{"course": `"EXAMPLE SCIENCE 7A"`, "term": `"Q1"`})
}

func TestTermCommentsPerTermAndUnconfirmed(t *testing.T) {
	q1 := commentsFixture("111111", "Q1", true, "First quarter.")
	s1 := commentsFixture("111111", "s1", true, "First semester.")
	unconfirmed := commentsFixture("111111", "Q2", false, "Not confirmed.")
	blank := commentsFixture("111111", "", true, "No term.")
	snaps := BuildTermComments(guardiansBinding, []*pslib.TeacherCommentsPage{q1, nil, unconfirmed, s1, blank}, obsAt, nil)
	validateAll(t, snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	wantScope(t, snaps[0], "powerschool", "term_comments", "e_child1", "term", "Q1")
	wantScope(t, snaps[1], "powerschool", "term_comments", "e_child1", "term", "S1")
}

// A term page whose comments are all empty is a complete, empty view of
// that term: earlier comments of the term are removed.
func TestTermCommentsEmptyTermIsEmptySnapshot(t *testing.T) {
	page := commentsFixture("111111", "Q1", true, "  \n ")
	snaps := BuildTermComments(guardiansBinding, []*pslib.TeacherCommentsPage{page}, obsAt, nil)
	validateAll(t, snaps)
	if len(snaps) != 1 || len(snaps[0].Records) != 0 {
		t.Fatalf("snapshots %+v", snaps)
	}
}

// Defence in depth: a binding that is not guardians-only gives nothing.
func TestTermCommentsRefuseAWiderAudience(t *testing.T) {
	var skipped []string
	b := bind1 // [subject, guardians]
	b.OnSkip = func(r string) { skipped = append(skipped, r) }
	page := commentsFixture("111111", "Q1", true, "A comment.")
	if n := len(BuildTermComments(b, []*pslib.TeacherCommentsPage{page}, obsAt, nil)); n != 0 {
		t.Fatalf("%d snapshots for a [subject, guardians] binding", n)
	}
	if len(skipped) != 1 || skipped[0] != "audience_not_guardians_only" {
		t.Fatalf("skipped %v", skipped)
	}
}
